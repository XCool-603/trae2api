package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"trae2api/internal/realm"
	"trae2api/internal/traectl"
	"trae2api/internal/usermgr"
)

// anthropicRequest 是 /v1/messages 的请求体
type anthropicRequest struct {
	Model       string          `json:"model"`
	Messages    []anthMessage   `json:"messages"`
	System      json.RawMessage `json:"system,omitempty"` // 可以是 string 或 []anthContentBlock
	MaxTokens   int             `json:"max_tokens"`
	Temperature *float64        `json:"temperature,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Tools       []anthTool      `json:"tools,omitempty"`
}

type anthMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string 或 []anthContentBlock
}

type anthContentBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   string         `json:"content,omitempty"`
}

type anthTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// handleAnthropicMessages 处理 POST /v1/messages 原生 Anthropic 协议
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "parse json: "+err.Error())
		return
	}

	// 1. 模型别名解析
	origModel := req.Model
	if target, ok := s.cfg.ModelAliases[req.Model]; ok && target != "" {
		req.Model = target
	}

	// 2. 租户配额与白名单检查
	caller := userFrom(r)
	if caller != nil {
		if caller.Role != usermgr.RoleAdmin && len(caller.ModelWhitelist) > 0 {
			allowed := false
			for _, m := range caller.ModelWhitelist {
				if m == origModel || m == req.Model {
					allowed = true
					break
				}
			}
			if !allowed {
				writeAnthropicError(w, http.StatusForbidden, "permission_error",
					fmt.Sprintf("用户 %s 无权调用模型 %q（该 Key 受模型白名单限制）", caller.Username, origModel))
				return
			}
		}

		if caller.Quota > 0 && caller.UsedQuota >= caller.Quota {
			writeAnthropicError(w, http.StatusTooManyRequests, "rate_limit_error",
				fmt.Sprintf("用户 %s 的配额已耗尽（上限: %d, 已用: %d）", caller.Username, caller.Quota, caller.UsedQuota))
			return
		}
	}

	realmKey, bareModel := realm.ResolveModel(req.Model)
	ch := s.channels[realmKey]
	if ch == nil {
		if alt, ok := s.otherRealmChannel(realmKey); ok {
			ch = alt
			realmKey = alt.Realm
		}
	}
	if ch == nil {
		writeAnthropicError(w, http.StatusServiceUnavailable, "api_error",
			fmt.Sprintf("渠道 %q 未启用（可用渠道：%s）", realmKey, strings.Join(s.realmNames(), ", ")))
		return
	}
	if bareModel == "" {
		bareModel = ch.DefaultModel()
	}

	// 3. 将 Anthropic 结构转为内部 traectl.ChatRequest
	creq := s.convertAnthropicToChatRequest(req, bareModel)
	cost := s.modelCreditCost(ch, bareModel)
	st := newChatStat(time.Now(), req.Model, req.Stream)
	st.setRealm(realmKey)
	st.setCredits(cost)
	st.collector = s.metrics
	defer st.done()

	owners := []string{""}
	if caller != nil && caller.Role != usermgr.RoleAdmin {
		owners = []string{"", caller.Username, caller.ID}
	}
	tried := map[string]bool{}

	var lastErr error
	for attempt := 0; attempt < s.maxRotate; attempt++ {
		var newCh *Channel
		var newRealm string
		acct, newCh, newRealm := s.pickAccountWithFailover(r.Context(), ch, realmKey, bareModel, owners, tried)
		if acct == nil {
			break
		}
		if newRealm != realmKey {
			ch = newCh
			realmKey = newRealm
			st.setRealm(realmKey)
			cost = s.modelCreditCost(ch, bareModel)
			st.setCredits(cost)
		}
		tried[acct.UID] = true
		if !s.pool.Acquire(acct.UID) {
			continue
		}
		st.setAccount(acct.UID, acct.Label)

		cl := ch.ClientFor(acct)
		cl.OnRefresh = func(product, expiresAt string) {
			s.pool.NoteSuccess(acct.UID)
		}

		var runErr error
		if !req.Stream {
			runErr = s.runAnthropicNonStream(w, r.Context(), cl, creq, req.Model, bareModel, caller, cost, st)
		} else {
			runErr = s.runAnthropicStream(w, r.Context(), cl, creq, req.Model, bareModel, caller, cost, st)
		}

		s.pool.Release(acct.UID)
		if runErr == nil {
			s.pool.NoteSuccess(acct.UID)
			return
		}
		lastErr = runErr
		s.noteFailure(ch, acct, bareModel, runErr)
		if errors.Is(runErr, errAlreadyStreaming) {
			st.markFailed()
			return
		}
	}

	st.markFailed()
	if lastErr != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", lastErr.Error())
		return
	}
	writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "没有可用账号")
}

func (s *Server) convertAnthropicToChatRequest(req anthropicRequest, bareModel string) traectl.ChatRequest {
	var msgs []traectl.ChatMessage

	// 解析 system prompt
	if len(req.System) > 0 {
		var sysStr string
		if err := json.Unmarshal(req.System, &sysStr); err == nil && sysStr != "" {
			msgs = append(msgs, traectl.ChatMessage{
				Role:  "system",
				Parts: []traectl.ChatPart{{Type: "text", Text: sysStr}},
			})
		} else {
			var blocks []anthContentBlock
			if err := json.Unmarshal(req.System, &blocks); err == nil {
				var sb strings.Builder
				for _, b := range blocks {
					if b.Text != "" {
						sb.WriteString(b.Text)
					}
				}
				if sb.Len() > 0 {
					msgs = append(msgs, traectl.ChatMessage{
						Role:  "system",
						Parts: []traectl.ChatPart{{Type: "text", Text: sb.String()}},
					})
				}
			}
		}
	}

	for _, m := range req.Messages {
		var parts []traectl.ChatPart
		var strContent string
		if err := json.Unmarshal(m.Content, &strContent); err == nil {
			parts = append(parts, traectl.ChatPart{Type: "text", Text: strContent})
		} else {
			var blocks []anthContentBlock
			_ = json.Unmarshal(m.Content, &blocks)
			for _, b := range blocks {
				switch b.Type {
				case "text":
					parts = append(parts, traectl.ChatPart{Type: "text", Text: b.Text})
				case "tool_result":
					parts = append(parts, traectl.ChatPart{Type: "text", Text: fmt.Sprintf("[工具返回 %s]: %s", b.ToolUseID, b.Content)})
				case "tool_use":
					inJSON, _ := json.Marshal(b.Input)
					parts = append(parts, traectl.ChatPart{Type: "text", Text: fmt.Sprintf("[调用工具 %s(%s)]", b.Name, string(inJSON))})
				}
			}
		}
		if len(parts) == 0 {
			parts = []traectl.ChatPart{{Type: "text", Text: ""}}
		}
		msgs = append(msgs, traectl.ChatMessage{Role: m.Role, Parts: parts})
	}

	var tools []traectl.Tool
	for _, t := range req.Tools {
		paramsJSON, _ := json.Marshal(t.InputSchema)
		tools = append(tools, traectl.Tool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  paramsJSON,
		})
	}

	maxTok := req.MaxTokens
	if maxTok == 0 {
		maxTok = 4096
	}

	return traectl.ChatRequest{
		Model:     bareModel,
		Messages:  msgs,
		Tools:     tools,
		MaxTokens: maxTok,
	}
}

func (s *Server) runAnthropicNonStream(w http.ResponseWriter, ctx context.Context, cl *traectl.Client, creq traectl.ChatRequest, outModel, bareModel string, caller *usermgr.User, cost float64, st *chatStat) error {
	agg, err := cl.Aggregate(ctx, creq)
	if err != nil {
		return err
	}

	id := "msg_" + randHex(20)
	resp := map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         outModel,
		"content":       []any{map[string]any{"type": "text", "text": agg.Content}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  agg.Usage.PromptTokens,
			"output_tokens": agg.Usage.CompletionTokens,
			"credits":       cost,
			"credit_cost":   cost,
		},
	}
	if cost > 0 {
		w.Header().Set("X-Credit-Cost", fmt.Sprintf("%.2f", cost))
	}
	if caller != nil && s.users != nil && agg.Usage != nil {
		s.users.DeductQuota(caller.ID, int64(agg.Usage.TotalTokens))
	}
	if st != nil {
		st.recordUsage(agg.Usage)
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Server) runAnthropicStream(w http.ResponseWriter, ctx context.Context, cl *traectl.Client, creq traectl.ChatRequest, outModel, bareModel string, caller *usermgr.User, cost float64, st *chatStat) error {
	flusher, _ := w.(http.Flusher)
	id := "msg_" + randHex(20)

	headersWritten := false
	ensureHeaders := func() {
		if headersWritten {
			return
		}
		headersWritten = true
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		if cost > 0 {
			w.Header().Set("X-Credit-Cost", fmt.Sprintf("%.2f", cost))
		}
		w.WriteHeader(http.StatusOK)
	}

	sendEvent := func(event string, data any) {
		ensureHeaders()
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	var usage *traectl.Usage
	first := true
	tokensUsed := int64(0)

	err := cl.Chat(ctx, creq, func(d traectl.Delta) error {
		if d.Keepalive {
			return nil
		}
		if first {
			first = false
			ensureHeaders()
			sendEvent("message_start", map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id":      id,
					"type":    "message",
					"role":    "assistant",
					"model":   outModel,
					"content": []any{},
					"usage":   map[string]any{"input_tokens": 10, "output_tokens": 1},
				},
			})
			sendEvent("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         0,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
		}

		if d.Content != "" {
			sendEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{"type": "text_delta", "text": d.Content},
			})
		}
		if d.Usage != nil {
			usage = d.Usage
		}
		return nil
	})

	if err != nil {
		if !headersWritten {
			return err
		}
		return fmt.Errorf("%w: %w", errAlreadyStreaming, err)
	}

	sendEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	outTokens := 10
	inTokens := 10
	if usage != nil {
		outTokens = usage.CompletionTokens
		inTokens = usage.PromptTokens
		tokensUsed = int64(usage.TotalTokens)
	}
	sendEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{
			"output_tokens": outTokens,
			"credits":       cost,
			"credit_cost":   cost,
		},
	})
	sendEvent("message_stop", map[string]any{"type": "message_stop"})

	if caller != nil && s.users != nil && tokensUsed > 0 {
		s.users.DeductQuota(caller.ID, tokensUsed)
	}
	if st != nil {
		st.recordUsage(usage)
	}
	_ = inTokens
	return nil
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": msg,
		},
	})
}
