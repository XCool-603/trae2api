package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"trae2api/internal/pool"
	"trae2api/internal/realm"
	"trae2api/internal/traectl"
	"trae2api/internal/usermgr"
)

// 本文件实现 OpenAI 兼容的 /v1/chat/completions：把 OpenAI 请求翻译成 Trae
// 协议、调用上游、再把结果翻译回 OpenAI 格式（流式与非流式）。
//
// 协议细节（字段名、必填项、陷阱）全部来自对本机客户端的逆向与实测，
// 详见项目根 README 的「协议要点」一节。

// ---- OpenAI 请求模型 ----

type chatRequest struct {
	Model               string        `json:"model"`
	Messages            []chatMessage `json:"messages"`
	Tools               []chatTool    `json:"tools"`
	ToolChoice          any           `json:"tool_choice"`
	MaxTokens           int           `json:"max_tokens"`
	MaxCompletionTokens int           `json:"max_completion_tokens"`
	Temperature         *float64      `json:"temperature"`
	Stream              *bool         `json:"stream"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// ---- 转换 ----

// contentPart 是一个内容片段。**同时兼容 OpenAI 与 Anthropic 两套形态**。
//
// 为什么要兼容两套：Claude 系客户端（以及很多"多协议"客户端）发的是
// Anthropic 的 `{"type":"image","source":{"type":"base64","media_type":...,"data":...}}`，
// 而不是 OpenAI 的 `{"type":"image_url","image_url":{"url":...}}`。
// 只认后者的话，这些客户端发的图片会被**静默丢弃**（降级成空文本片段）——
// 用户看到的是"模型说没收到图片"，而报错里什么都没说。
type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`

	// OpenAI 形态
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`

	// Anthropic 形态：source.type = "base64" | "url"
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
}

// toChatPart 把一个片段转成内部的 ChatPart。返回 (片段, 是否有效)。
func (p contentPart) toChatPart() (traectl.ChatPart, bool) {
	switch p.Type {
	case "image_url":
		if p.ImageURL != nil && p.ImageURL.URL != "" {
			return traectl.ChatPart{Type: "image_url", ImageURL: p.ImageURL.URL}, true
		}
		return traectl.ChatPart{}, false

	case "image":
		// Anthropic 形态：base64 要拼成 data URL，url 直接透传
		if p.Source == nil {
			return traectl.ChatPart{}, false
		}
		switch p.Source.Type {
		case "base64":
			if p.Source.Data == "" {
				return traectl.ChatPart{}, false
			}
			mt := p.Source.MediaType
			if mt == "" {
				// 缺 media_type 时按 png 兜底：上游大多按魔数识别，
				// 而丢掉整张图比猜一个 MIME 更糟。
				mt = "image/png"
			}
			return traectl.ChatPart{
				Type:     "image_url",
				ImageURL: "data:" + mt + ";base64," + p.Source.Data,
			}, true
		case "url":
			if p.Source.URL == "" {
				return traectl.ChatPart{}, false
			}
			return traectl.ChatPart{Type: "image_url", ImageURL: p.Source.URL}, true
		}
		return traectl.ChatPart{}, false

	case "text", "":
		return traectl.ChatPart{Type: "text", Text: p.Text}, true
	}
	return traectl.ChatPart{}, false
}

// parseContent 解析 content（字符串 或 内容片段数组）。
func parseContent(raw json.RawMessage) []traectl.ChatPart {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []traectl.ChatPart{{Type: "text", Text: s}}
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	out := make([]traectl.ChatPart, 0, len(parts))
	for _, p := range parts {
		if cp, ok := p.toChatPart(); ok {
			out = append(out, cp)
			continue
		}
		// 未识别或字段缺失的片段：**降级为文本**而不是丢弃整条请求。
		// 已知会走到这里的：input_audio、file（文档类）等。
		// 但注意——图片类片段字段缺失时降级成空文本会让用户"以为发了图"，
		// 所以下面把类型名写进文本，至少能在对话里看到线索。
		if p.Text != "" {
			out = append(out, traectl.ChatPart{Type: "text", Text: p.Text})
		} else if p.Type != "" {
			out = append(out, traectl.ChatPart{
				Type: "text",
				Text: "[不支持的片段类型：" + p.Type + "]",
			})
		}
	}
	return out
}

// toTraeRequest 把 OpenAI 请求转换为 Trae 协议请求。
//
// 角色映射（Trae 只有 system/user/assistant 三种）：
//   - tool 结果 → user 文本（带工具名与 call id，便于模型对齐上下文）
//   - assistant 的 tool_calls → 追加文本片段，保持多轮工具对话的上下文连贯
func toTraeRequest(req *chatRequest, bareModel string) traectl.ChatRequest {
	msgs := make([]traectl.ChatMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := m.Role
		parts := parseContent(m.Content)

		switch role {
		case "tool":
			var sb strings.Builder
			sb.WriteString("[工具 ")
			if m.Name != "" {
				sb.WriteString(m.Name)
			} else if m.ToolCallID != "" {
				sb.WriteString(m.ToolCallID)
			}
			sb.WriteString(" 的返回]\n")
			for _, p := range parts {
				sb.WriteString(p.Text)
			}
			msgs = append(msgs, traectl.ChatMessage{
				Role:  "user",
				Parts: []traectl.ChatPart{{Type: "text", Text: sb.String()}},
			})
			continue
		case "function":
			role = "assistant"
		case "system", "developer":
			role = "system"
		case "user", "assistant":
		default:
			role = "user"
		}

		if role == "assistant" && len(m.ToolCalls) > 0 {
			var sb strings.Builder
			for _, tc := range m.ToolCalls {
				sb.WriteString(fmt.Sprintf("[调用工具 %s(%s)]\n", tc.Function.Name, tc.Function.Arguments))
			}
			parts = append(parts, traectl.ChatPart{Type: "text", Text: sb.String()})
		}
		if len(parts) == 0 {
			parts = []traectl.ChatPart{{Type: "text", Text: ""}}
		}
		msgs = append(msgs, traectl.ChatMessage{Role: role, Parts: parts})
	}

	tools := make([]traectl.Tool, 0, len(req.Tools))
	for _, t := range req.Tools {
		if t.Function.Name == "" {
			continue
		}
		tools = append(tools, traectl.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}

	maxTok := req.MaxTokens
	if maxTok == 0 {
		maxTok = req.MaxCompletionTokens
	}
	return traectl.ChatRequest{
		Model:     bareModel,
		Messages:  msgs,
		Tools:     tools,
		MaxTokens: maxTok,
	}
}

// usageToMap 把上游 usage 映射为 OpenAI 结构（含 cached / reasoning 明细）。
func usageToMap(u *traectl.Usage) map[string]any {
	return map[string]any{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": u.CacheReadInputTokens,
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": u.ReasoningTokens,
		},
	}
}

// ---- 处理入口 ----

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse body: "+err.Error())
		return
	}

	// 1. 模型别名解析（例如 gpt-4o -> trae:glm-5.3）
	origModel := req.Model
	if target, ok := s.cfg.ModelAliases[req.Model]; ok && target != "" {
		req.Model = target
	}

	// 2. 检查租户/用户的模型白名单与剩余配额
	caller := userFrom(r)
	if caller != nil {
		// 模型白名单检查（非管理员用户如果配置了白名单，则只允许使用白名单内的模型）
		if caller.Role != usermgr.RoleAdmin && len(caller.ModelWhitelist) > 0 {
			allowed := false
			for _, m := range caller.ModelWhitelist {
				if m == origModel || m == req.Model {
					allowed = true
					break
				}
			}
			if !allowed {
				writeOpenAIError(w, http.StatusForbidden, "model_not_allowed",
					fmt.Sprintf("用户 %s 无权调用模型 %q（该 Key 已受模型白名单限制）", caller.Username, origModel))
				return
			}
		}

		// 预检配额（如果设置了配额且已耗尽）
		if caller.Quota > 0 && caller.UsedQuota >= caller.Quota {
			writeOpenAIError(w, http.StatusTooManyRequests, "insufficient_quota",
				fmt.Sprintf("用户 %s 的配额已耗尽（上限: %d, 已用: %d）", caller.Username, caller.Quota, caller.UsedQuota))
			return
		}
	}

	realmKey, bareModel := realm.ResolveModel(req.Model)

	// 未知域前缀 → **明确报错**，而不是当裸模型名透传给上游。
	//
	// 为什么必须拦：上游对未知模型名会**静默回退**到某个默认模型并正常返回 200
	// （实测：传 `trae:no-such-model` 也返回 200，内容是默认模型的回答）。
	// 于是把域写错（如 `trae2:glm-5.2`）会被当成模型名发出去，用户以为调用了 X、
	// 实际拿到 Y，还照样消耗额度 —— 这种静默错配只能在网关侧拦住。
	//
	// 判定依据：**真实 Trae 模型名里没有冒号**（自定义渠道用 `provider//model`
	// 形式，用的是斜杠）。所以"含冒号且前缀不是已知域"几乎一定是域写错了。
	if idx := strings.IndexByte(req.Model, ':'); idx > 0 {
		if prefix := req.Model[:idx]; !realm.Known(prefix) {
			writeOpenAIError(w, http.StatusBadRequest, "unknown_realm",
				fmt.Sprintf("模型名 %q 的前缀 %q 不是已知的域。可用前缀：%s；也可直接写裸模型名（默认走 %s）",
					req.Model, prefix, strings.Join(realm.All(), " / "), realm.Trae))
			return
		}
	}

	ch := s.channels[realmKey]

	// 跨版本域兜底：请求的域**完全没有渠道**时，退到另一个域。
	//
	// 为什么只在这个条件下兜底：若该域有渠道但账号都在冷却，兜底到另一域
	// 会把请求悄悄打到另一套账号体系上（计费与额度都不同），用户却以为
	// 自己用的是原来的域。而"该域一个渠道都没有"是**部署事实**（没配国际版
	// 或没导入国际账号），此时兜底是明确的便利，且会记 WARN 让用户知情。
	//
	// 对齐参考实现：它在目标域账号数为 0 时尝试另一域，并给出诊断。
	if ch == nil {
		if alt, ok := s.otherRealmChannel(realmKey); ok {
			s.logf("WARN: 渠道 %q 未启用（没有任何该域的账号），已兜底到 %q。"+
				" 若要使用 %q，请配置并导入该版本域的账号",
				realmKey, alt.Realm, realmKey)
			ch = alt
			realmKey = alt.Realm
		}
	}
	if ch == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "channel_unavailable",
			fmt.Sprintf("渠道 %q 未启用（可用渠道：%s）", realmKey, strings.Join(s.realmNames(), ", ")))
		return
	}
	if bareModel == "" {
		bareModel = ch.DefaultModel()
	}
	// 目录外的模型名**不直接拒绝**：服务端目录可能已更新而本地缓存尚未刷新，
	// 透传让上游给出明确错误，比网关猜测更可靠。
	//
	// 但必须记 WARN —— 实测上游对未知模型名会**静默回退**到某个默认模型并正常
	// 返回（HTTP 200），用户会以为"指定生效了"却拿到别的模型的回答。日志是
	// 唯一能暴露这种静默错配的线索。
	if _, ok := ch.Catalog.ByID(bareModel); !ok && len(ch.Catalog.Models) > 0 {
		if s.cfg.StrictModelsOn() {
			// 严格模式：直接拒绝，并**列出可用模型**让用户能立刻改对。
			//
			// 为什么默认严格：上游对未知模型名会静默回退到某个默认模型并返回 200，
			// 用户会以为"指定生效了"却拿到别的模型的回答，而且照样消耗额度。
			// 一个明确的 400（附可用清单）比这种静默错配好得多。
			//
			// 代价：上游新增模型而客户端还没刷新目录时，请求会被拒。
			// 此时把 trae.strict_models 设为 false 即可透传（会记 WARN）。
			writeOpenAIError(w, http.StatusBadRequest, "unknown_model",
				fmt.Sprintf("模型 %q 不在 %s 的可用清单中。可用模型：%s（也可用 `traeapi-models` 查看完整列表）。"+
					"若上游确实新增了模型而本地目录尚未刷新，可在配置里设 trae.strict_models=false 透传",
					bareModel, realmKey, strings.Join(sampleModels(ch.Catalog, 12), " / ")))
			return
		}
		s.logf("WARN: 模型 %q 不在本地目录中（渠道 %s，共 %d 个）；上游会静默回退到默认模型"+
			"（strict_models=false 时才会走到这里）",
			bareModel, realmKey, len(ch.Catalog.Models))
	}
	// 图片输入：上游不接受内联图片（实测，见 vision.go）。默认**明确失败**，
	// 而不是转上去让上游回一个笼统的 4001（用户看不出问题在图片上）。
	if s.rejectVisionIfUnsupported(w, &req) {
		return
	}

	stream := req.Stream == nil || *req.Stream
	includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage

	// System Prompt 改写（passthrough 或空文本时是零开销的空操作）。
	// 改写后 messages 变了，必须**重新解析一次**请求结构体 —— 否则下面构造的
	// Trae 请求仍是改写前的内容（改写形同虚设，而且这种错很难从外部看出来）。
	effBody := body
	if s.prompt.Enabled() {
		// 降级期内**直接**用中性提示词，不再"先撞 400 再重试" ——
		// 那样每个请求都要白挨一次失败，既慢又浪费额度。
		apply := s.prompt.Apply
		if s.degrade.Active() {
			apply = s.prompt.ApplyDegraded
		}
		if nb := apply(body); !bytes.Equal(nb, body) {
			effBody = nb
			var re chatRequest
			if err := json.Unmarshal(effBody, &re); err == nil {
				req = re
			}
		}
	}
	// 附件展开（默认关闭；未配置 allowed_roots 时自动关闭）。
	//
	// 放在 prompt 改写**之后**：改写可能引入新的文本片段，而展开只关心
	// 文件引用。两者互不干扰，但先改写能让展开看到最终的消息结构。
	//
	// 展开失败**不会**让请求失败：读不到的文件会变成一条占位文本并记进日志，
	// 让用户知道"为什么这个文件没生效"，而不是整条请求 400。
	if s.docs.Enabled() {
		if nb, st := s.expandFileRefs(effBody); len(st.Skipped) > 0 || st.Files > 0 {
			effBody = nb
			var re chatRequest
			if err := json.Unmarshal(effBody, &re); err == nil {
				req = re
			}
			s.logFileStats(st)
		}
	}

	// 工具配对自愈（**最后一步**，作用于最终消息列表）。
	//
	// 顺序有讲究：先重排让同批结果连续，再裁剪剔除孤儿。反过来的话，
	// 被插入物打断的那批结果会被当成"部分配对"而误裁。
	//
	// 这一步是"让会话能继续"的安全网：坏历史（工具失败后客户端写不回结果）
	// 若原样重放，上游会对之后每一条消息都返回 400，整条会话报废。
	if fixed, changed := fixToolPairing(req.Messages); changed {
		req.Messages = fixed
		s.logf("INFO: 已修复工具配对（剔除无法配对的 tool_call / tool 结果，让会话自愈）")
	}

	creq := toTraeRequest(&req, bareModel)

	owners := s.ownerOf(r)
	st := newChatStat(time.Now(), req.Model, stream)
	st.setRealm(realmKey)
	st.collector = s.metrics
	defer st.done()

	// 会话粘性：优先用客户端显式会话标识，缺失时用首条 user 消息派生。
	// 键带归属前缀，防止不同用户相同会话 ID 互相绑定。
	stickyKey := ""
	var stickyUID string
	if s.sessions != nil {
		stickyKey = sessionKey(body, owners)
		if stickyKey != "" {
			stickyUID, _ = s.sessions.Resolve(stickyKey)
		}
	}

	// 选号 + 轮换：最多尝试 maxRotate 个账号（首个失败则换号重试）。
	tried := map[string]bool{}
	var lastErr error
	degradedTried := false
	for attempt := 0; attempt < s.maxRotate; attempt++ {
		// WAF IP 级闸：已激活时**不再尝试下一个账号**。
		//
		// 为什么：IP 被封时逐个换号是反效果的 —— 每换一个号就多打一次上游，
		// 把请求量放大到已被封的来源 IP 上，只会延长封禁窗口。
		// 注意判定放在**取号之前**（workbuddy2api 的经验：判定要在退避/取号之前，
		// 否则本轮仍会多打一次）。
		if attempt > 0 && s.wafIP != nil && s.wafIP.Active(time.Now()) {
			s.logf("WARN: WAF IP 级闸激活中，终止本轮轮转（已试 %d 个账号，不再放大请求量）", attempt)
			break
		}

		var acct *pool.Account
		if attempt == 0 && stickyUID != "" {
			if a := ch.Account(stickyUID); a != nil {
				if s.pool.Acquire(a.UID) {
					acct = a
				}
			}
		}
		if acct == nil {
			acct = s.pickWithWait(r.Context(), realmKey, bareModel, owners, tried)
			if acct == nil {
				break
			}
			if !s.pool.Acquire(acct.UID) {
				continue // 在途已满：换一个账号
			}
		}
		tried[acct.UID] = true
		if stickyKey != "" && s.sessions != nil && stickyUID == "" {
			s.sessions.Bind(stickyKey, acct.UID)
		}

		st.setAccount(acct.UID, acct.Label)
		// prompt cache key **必须按账号生成**（见 traectl/cachekey.go）：
		// 跨账号复用同一个 key 会让上游命中错账号的前缀缓存、泄露对方对话。
		// 所以只能在选号之后设置，不能提前放进 creq。
		creq.PromptCacheKey = traectl.BuildPromptCacheKey(acct.UID, stickyKey)
		err = s.runChat(w, r, ch, acct, creq, bareModel, stream, includeUsage, st)

		// 上游 400 且启用了 prompt 改写 → 换**极简** system prompt 重试一次。
		//
		// 场景：注入的文本触发了上游的内容校验，而这与用户的实际问题无关。
		// 只重试一次（degradedTried 是全局的，不会每个账号都试一遍）。
		//
		// 注意：这要求 400 在**响应头写出之前**到达 —— 流式路径因此改成了
		// 懒发首帧（见 streamChat 注释），否则状态码已提交，重试无从谈起。
		if err != nil && !degradedTried && s.prompt.Enabled() && isHTTPStatus(err, http.StatusBadRequest) {
			degradedTried = true
			// 记下降级闸：后续请求直接走中性提示词，不必再白撞一次 400。
			// 闸到次日零点自动解除（不续期，见 degrade.go）。
			s.degrade.Trigger()
			if db := s.prompt.ApplyDegraded(effBody); !bytes.Equal(db, effBody) {
				var dre chatRequest
				if json.Unmarshal(db, &dre) == nil {
					s.logf("WARN: 上游 400，改用极简 system prompt 重试一次（acct=%s）", acct.UID)
					err = s.runChat(w, r, ch, acct, toTraeRequest(&dre, bareModel),
						bareModel, stream, includeUsage, st)
				}
			}
		}

		s.pool.Release(acct.UID)

		if err == nil {
			s.pool.NoteSuccess(acct.UID)
			return
		}
		st.markFailed()
		lastErr = err

		// WAF 拦截：先更新 IP 级闸，再走常规冷却处置（两者独立，见 wafip.go 注释）
		if isWAFBlock(err) && s.wafIP != nil {
			active, justActivated := s.wafIP.NoteWAF(acct.UID, time.Now())
			if justActivated {
				s.logf("WARN: WAF IP 级闸已激活（%s）—— 短窗口内多个不同账号被拦截，"+
					"问题在上游按来源 IP 拦截而非账号；本轮不再换号重试", s.wafIP.Status()["last_reason"])
			}
			_ = active
		}

		s.noteFailure(ch, acct, bareModel, err)

		// 已开始向客户端输出（流式）→ 无法换号重试，只能就此结束
		if errors.Is(err, errAlreadyStreaming) {
			return
		}
		// 粘性命中但该号不可用 → 解绑，让下次请求重新选号
		if stickyUID != "" && stickyKey != "" && s.sessions != nil && acct.UID == stickyUID {
			s.sessions.Unbind(stickyKey)
			stickyUID = ""
		}
	}

	if lastErr != nil {
		st.markFailed()
		writeTraeError(w, lastErr)
		return
	}
	st.markFailed()
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_account",
		"没有可用账号：全部账号均在冷却/熔断/禁用中，或已达在途并发上限")
}

// otherRealmChannel 在目标域完全没有渠道时，返回可兜底的另一个域。
//
// 配对关系：trae ↔ trae-intl、solo ↔ solo-intl（同 IsSOLO 的两个版本域）。
// 没有配对（或配对域也没渠道）时返回 false —— 不做"随便挑一个"的兜底，
// 那会让 trae 的请求落到 solo 的模型池上（两者的模型清单与 function 都不同）。
func (s *Server) otherRealmChannel(realmKey string) (*Channel, bool) {
	var altKey string
	switch realmKey {
	case realm.Trae:
		altKey = realm.TraeIntl
	case realm.TraeIntl:
		altKey = realm.Trae
	case realm.Solo:
		altKey = realm.SoloIntl
	case realm.SoloIntl:
		altKey = realm.Solo
	default:
		return nil, false
	}
	alt, ok := s.channels[altKey]
	if !ok || alt == nil {
		return nil, false
	}
	return alt, true
}

// pickWaitTimeout 是"名额满了"时愿意等多久。
//
// 取 20 秒：多数请求几秒内就结束并释放名额，20 秒足够消化常见的并发突发；
// 再长就会超过客户端的耐心（它们会先超时断开），等待也就白费了。
const pickWaitTimeout = 20 * time.Second

// pickWithWait 选号；若"账号健康但名额满了"，短暂排队等待而不是立刻放弃。
//
// ## 为什么需要
//
// 在途上限（默认每账号 3）是"避免把单个账号打爆"的保护，**不是"拒绝服务"**。
// 但原实现里名额一满就 break → 客户端收到 503。而客户端发 4 个并行请求
// 是很常见的（多标签、批量脚本），它会把 503 理解成"网关坏了"。
//
// ## 关键：只在"值得等"时才等
//
// Pick 返回 nil 有两种原因，必须区分：
//   - 账号健康、只是名额满了 → 几秒内就会释放，**值得等**
//   - 账号都在冷却/熔断/停用 → 可能要等几十分钟，**等也没用**
//
// 不做这个区分的话：要么在"全池冷却 24 小时"时每个请求白等 20 秒
// （把小问题放大成雪崩），要么在"只是并发突发"时直接给客户端 503。
func (s *Server) pickWithWait(ctx context.Context, realmKey, model string, owners []string, tried map[string]bool) *pool.Account {
	if a := s.pool.Pick(realmKey, model, owners, tried); a != nil {
		return a
	}
	// 没有健康账号 → 等也没用，立刻返回让上层给出明确原因
	if s.pool.HealthyIgnoringInflight(realmKey) == 0 {
		return nil
	}

	deadline := time.Now().Add(pickWaitTimeout)
	t := time.NewTicker(150 * time.Millisecond)
	defer t.Stop()
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			// 客户端已断开：没必要再等
			return nil
		case <-s.pool.SlotNotify():
			if a := s.pool.Pick(realmKey, model, owners, tried); a != nil {
				s.logf("INFO: 收到名额释放通知，排队 %.2fs 后取得名额",
					time.Since(deadline.Add(-pickWaitTimeout)).Seconds())
				return a
			}
		case <-t.C:
			if a := s.pool.Pick(realmKey, model, owners, tried); a != nil {
				s.logf("INFO: 账号名额已满，排队 %.1fs 后取得名额",
					time.Since(deadline.Add(-pickWaitTimeout)).Seconds())
				return a
			}
		}
	}
	s.logf("WARN: 等待账号名额超时（%s），放弃", pickWaitTimeout)
	return nil
}

// isHTTPStatus 判定错误是否为指定的上游 HTTP 状态码。
func isHTTPStatus(err error, status int) bool {
	var ue *traectl.UpstreamError
	return errors.As(err, &ue) && ue.Status == status
}

// errAlreadyStreaming 表示响应已开始输出（HTTP 200 已发出），无法再换号或改状态码。
var errAlreadyStreaming = errors.New("response already streaming")

// runChat 执行一次对话（流式或非流式）。
func (s *Server) runChat(w http.ResponseWriter, r *http.Request, ch *Channel, acct *pool.Account,
	creq traectl.ChatRequest, bareModel string, stream, includeUsage bool, st *chatStat) error {

	cl := ch.ClientFor(acct)
	// 续期成功后同步池内状态（余额/健康判定依赖过期时间）
	cl.OnRefresh = func(product, expiresAt string) {
		s.pool.NoteSuccess(acct.UID)
		s.logf("INFO: [%s] 账号 %s token 已续期，有效期至 %s", product, acct.UID, expiresAt)
	}

	if !stream {
		agg, err := cl.Aggregate(r.Context(), creq)
		if err != nil {
			return err
		}
		writeCompletion(w, bareModel, agg)
		st.recordUsage(agg.Usage)
		if u := userFrom(r); u != nil && s.users != nil && agg.Usage != nil {
			s.users.DeductQuota(u.ID, int64(agg.Usage.TotalTokens))
		}
		return nil
	}
	err := s.streamChat(w, r.Context(), cl, creq, bareModel, includeUsage, st)
	if err == nil || errors.Is(err, errAlreadyStreaming) {
		tokens := st.promptTok + st.compTok
		if u := userFrom(r); u != nil && s.users != nil && tokens > 0 {
			s.users.DeductQuota(u.ID, tokens)
		}
	}
	return err
}

// streamChat 流式输出（OpenAI SSE）。
//
// 一旦写出 HTTP 200 与首帧，就无法再改状态码 —— 此时若上游报错，
// 只能以 SSE error 帧收尾，并返回 errAlreadyStreaming 阻止上层换号重试
// （否则会把两个账号的内容混进同一条回复）。
func (s *Server) streamChat(w http.ResponseWriter, ctx context.Context, cl *traectl.Client,
	creq traectl.ChatRequest, bareModel string, includeUsage bool, st *chatStat) error {

	flusher, _ := w.(http.Flusher)
	id := "chatcmpl-" + randHex(12)
	created := time.Now().Unix()

	// **懒写响应头**：直到第一个真实增量到达才写 HTTP 200。
	//
	// 为什么必须这样：若提前写 200，上游随后返回 400/403 时状态码已提交，
	// 我们只能塞一个 SSE error 帧 —— 调用方再也无法据此换号重试或降级重试
	// （HTTP 语义上"已经成功了"）。懒写让非 2xx 能原样冒泡回上层，
	// 换号与 prompt 降级重试才成立。
	//
	// 代价：上游迟迟不吐时客户端要多等一会儿（见下面的空响应兜底）。
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
		w.WriteHeader(http.StatusOK)
	}

	writeChunk := func(delta map[string]any, finish any) {
		ensureHeaders()
		obj := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": bareModel,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		b, _ := json.Marshal(obj)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	var usage *traectl.Usage
	firstByte := true
	roleSent := false
	// toolBuf 按 index 聚合工具调用增量（流结束时统一校验并发出）。
	toolBuf := map[int]*toolAccum{}
	err := cl.Chat(ctx, creq, func(d traectl.Delta) error {
		// 任何一帧（含心跳）都说明上游已返回 2xx 并开始吐了 → 可以安全开流。
		// 心跳帧本身只发一条 SSE 注释，客户端会忽略它，但字节到了就不会超时。
		if !headersWritten {
			ensureHeaders()
		}
		if !roleSent {
			roleSent = true
			writeChunk(map[string]any{"role": "assistant", "content": ""}, nil)
		}
		if d.Keepalive {
			// SSE 注释帧：合法且被所有解析器忽略，只用于"续命"
			fmt.Fprint(w, ": keepalive\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		}
		if firstByte {
			firstByte = false
			st.ttfb = time.Since(st.start)
			st.ttfbSet = true
		}
		if d.Reasoning != "" {
			writeChunk(map[string]any{"reasoning_content": d.Reasoning}, nil)
		}
		if d.Content != "" {
			writeChunk(map[string]any{"content": d.Content}, nil)
		}
		// 工具调用**先缓冲不发出**（见文件末尾的 truncation 说明）：
		// SSE 被截断时 arguments 会只剩半截 JSON，原样发给客户端会让它
		// 解析失败并卡死会话。所以攒齐、校验、再一次性发出。
		// 但工具调用同样是模型的有效输出，计入首字节时间并定期发送心跳保活。
		if len(d.ToolCalls) > 0 && firstByte {
			firstByte = false
			st.ttfb = time.Since(st.start)
			st.ttfbSet = true
		}
		for _, tc := range d.ToolCalls {
			buf := toolBuf[tc.Index]
			if buf == nil {
				buf = &toolAccum{}
				toolBuf[tc.Index] = buf
			}
			if tc.ID != "" {
				buf.id = tc.ID
			}
			if tc.Name != "" {
				buf.name = tc.Name
			}
			buf.args.WriteString(tc.Args)
		}
		if len(d.ToolCalls) > 0 {
			fmt.Fprint(w, ": keepalive\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		if d.Usage != nil {
			usage = d.Usage
		}
		if d.FinishReason != "" {
			st.setFinish(d.FinishReason)
		}
		return nil
	})

	if err != nil {
		if !headersWritten {
			// 还没写头 → 原样冒泡，让上层换号 / 降级重试 / 设正确状态码。
			// 这是懒写头带来的最大好处。
			s.logf("WARN: 流式未开始即失败（可重试）model=%s acct=%s: %v",
				bareModel, st.accountUID, err)
			return err
		}
		// 已开始输出 → 只能以 SSE error 帧收尾
		s.logf("WARN: 流式中断 model=%s acct=%s: %v", bareModel, st.accountUID, err)
		eb, _ := json.Marshal(map[string]any{
			"error": map[string]any{"message": err.Error(), "type": "upstream_error"},
		})
		fmt.Fprintf(w, "data: %s\n\n", eb)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return fmt.Errorf("%w: %v", errAlreadyStreaming, err)
	}

	// 上游正常结束但一个增量都没发（空回复）：补上头与 role，让客户端拿到完整形状
	if !roleSent {
		writeChunk(map[string]any{"role": "assistant", "content": ""}, nil)
	}

	// 工具调用：校验完整性后再发出。
	//
	// 为什么必须校验：SSE 被截断（连接中断 / finish_reason==length）时，
	// arguments 会只剩半截 JSON。把它原样交给客户端，客户端 JSON.parse 抛错，
	// 很多客户端会因此**卡死整个会话**。
	//
	// 关键区分（见 isTruncatedArguments）：只把"非空但解析不了"当截断。
	// 空串是合法的"无参数工具"；能解析但类型不对属于模型输出错误，
	// 交给客户端的 schema 校验，不在这里擅自丢弃。
	if len(toolBuf) > 0 {
		idx := make([]int, 0, len(toolBuf))
		for i := range toolBuf {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		emitted, dropped := 0, 0
		for _, i := range idx {
			b := toolBuf[i]
			args := b.args.String()
			if isTruncatedArguments(args) {
				dropped++
				s.logf("WARN: 丢弃残缺的工具调用（arguments 不是合法 JSON，长度 %d）—— "+
					"流被截断时会出现，原样下发会让客户端解析失败并卡死会话", len(args))
				continue
			}
			fn := map[string]any{"name": b.name, "arguments": args}
			call := map[string]any{"index": i, "type": "function", "function": fn}
			if b.id != "" {
				call["id"] = b.id
			}
			writeChunk(map[string]any{"tool_calls": []any{call}}, nil)
			emitted++
		}
		// 全部被丢弃时不能还宣称 finish_reason=tool_calls ——
		// 那会让客户端等一个永远不来的工具调用。改成 stop 并说明。
		if emitted == 0 && dropped > 0 {
			st.setFinish("stop")
		}
	}

	writeChunk(map[string]any{}, st.finishReason())
	if includeUsage && usage != nil {
		obj := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": bareModel,
			"choices": []any{},
			"usage":   usageToMap(usage),
		}
		b, _ := json.Marshal(obj)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	st.recordUsage(usage)
	return nil
}

// writeCompletion 非流式响应。
func writeCompletion(w http.ResponseWriter, model string, agg *traectl.Aggregated) {
	msg := map[string]any{"role": "assistant", "content": agg.Content}
	if agg.Reasoning != "" {
		msg["reasoning_content"] = agg.Reasoning
	}
	if len(agg.ToolCalls) > 0 {
		calls := make([]any, 0, len(agg.ToolCalls))
		for _, tc := range agg.ToolCalls {
			id := tc.ID
			if id == "" {
				id = "call_" + randHex(8)
			}
			calls = append(calls, map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": tc.Name, "arguments": tc.Args},
			})
		}
		msg["tool_calls"] = calls
	}
	resp := map[string]any{
		"id":      "chatcmpl-" + randHex(12),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "finish_reason": agg.FinishReason,
		}},
	}
	if agg.Usage != nil {
		resp["usage"] = usageToMap(agg.Usage)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

// ---- 错误处置 ----

// noteFailure 把协议错误映射为池内的分级冷却。
//
// 分级原则：
//   - 限流 4011 → **账号级**软冷却，按连续次数指数退避（短时避让即可恢复）
//   - 凭证失效/续期失败 → **账号级硬冷却**（调了必然失败，需用户重新登录）
//   - 4001 等模型相关错误 → **模型级**冷却（换模型即豁免，不连累同账号其他模型）
//   - 5xx / 网络错误 → 账号级短冷却 + 记一次失败（参与熔断）
func (s *Server) noteFailure(ch *Channel, a *pool.Account, model string, err error) {
	var ue *traectl.UpstreamError
	if errors.As(err, &ue) {
		switch {
		case ue.UpstreamCode() == 4011 || ue.Status == http.StatusTooManyRequests:
			d := s.pool.CooldownSoftRate(a.UID, "上游 4011 限流")
			s.logf("WARN: 账号 %s 限流，冷却 %s", a.UID, d)
			return
		case ue.Status == http.StatusUnauthorized:
			s.pool.Cooldown(a.UID, pool.CoolHard, 24*time.Hour, "凭证失效（需重新登录客户端）")
			s.logf("WARN: 账号 %s 凭证失效，已硬冷却（请在客户端重新登录）", a.UID)
			if s.webhook != nil {
				s.webhook.SendAlert("Trae 账号凭证失效", fmt.Sprintf("账号 %s 凭证已失效（收到 401 Unauthorized），已被硬冷却 24 小时，请尽快重新登录客户端刷新凭据。", a.UID), "error")
			}
			return
		case ue.Status == http.StatusForbidden:
			// 403 **不冷却任何东西**：它既不是账号的问题，也不是模型的问题，
			// 而是来源 IP 被上游拦了。这类问题由 IP 级闸负责（见 wafip.go），
			// 在这里冷却账号或模型只会污染池状态。
			s.logf("WARN: 账号 %s 收到上游 403（疑似 IP 级拦截），不冷却账号/模型", a.UID)
			return
		case ue.Status >= 500:
			s.pool.Cooldown(a.UID, pool.CoolSoft, 30*time.Second, fmt.Sprintf("上游 %d", ue.Status))
			s.pool.NoteError(a.UID)
			return
		case isClientRequestError(ue):
			// 请求本身不合法（实测 4001 "param is invalid"：assistant 消息 content 为空
			// 就会触发）。这**既不是账号的问题、也不是模型的问题** ——
			// 同一账号、同一模型服务其他客户端完全正常。
			// 冷却任何东西都是误伤：会把一个健康模型对所有人封掉几分钟。
			s.logf("WARN: 上游拒绝请求本身（code=%d，非账号/模型问题），不冷却: %s",
				ue.UpstreamCode(), trimForLog(ue.Body, 160))
			return
		default:
			// 其余上游码：多为该账号对该模型不可用 → 只冷却该模型
			if model != "" {
				d := s.modelCooldown
				s.pool.CooldownModel(a.UID, model, d, fmt.Sprintf("上游 code=%d", ue.UpstreamCode()))
				s.logf("WARN: 账号 %s 模型 %s 不可用（code=%d），已冷却该模型 %s",
					a.UID, model, ue.UpstreamCode(), d)
			}
			return
		}
	}
	var re *traectl.RefreshError
	if errors.As(err, &re) {
		s.pool.Cooldown(a.UID, pool.CoolHard, 24*time.Hour, "token 续期失败（需重新登录客户端）")
		s.logf("WARN: 账号 %s 续期失败，已硬冷却: %v", a.UID, re)
		return
	}
	if errors.Is(err, errAlreadyStreaming) {
		return // 已输出，不再叠加冷却判断（内层已按真实错误处置过）
	}
	// 传输层失败**不罚号**：这类失败是本地网络问题（DNS/连接/TLS/超时），
	// 不是账号的问题。罚号会让一次断网把整个健康池冷却一遍 ——
	// 上层轮换（maxRotate）已经能处理"这个号这次不通"，不需要再叠加惩罚。
	var te *traectl.TransportError
	if errors.As(err, &te) {
		s.logf("WARN: 账号 %s 传输层失败（不罚号，交给轮换）: %v", a.UID, te)
		s.noteUnclassifiedFailure(a.UID)
		return
	}
	// 未分类错误同样不罚号（保守）：宁可多轮换一次，也不要把好账号罚掉。
	// 真要罚，必须先能把它归类到明确的账号级问题（401/限流/额度）。
	s.logf("WARN: 账号 %s 未分类错误（不罚号）: %v", a.UID, err)
	s.noteUnclassifiedFailure(a.UID)
}

// noteUnclassifiedFailure 把"不知道原因的失败"喂给连败兜底。
//
// ## 为什么需要这条路径
//
// 上面两个分支都明确"不罚号"—— 因为罚号的前提是能把它归类到明确的账号级问题。
// 但那留下一个洞：**一个持续坏掉但每次都报未分类错误的账号永远不会被移出池**，
// 只会被反复选中、反复失败。
//
// 所以加一个连败兜底：单次不罚，但连续 N 次都是"说不清原因的失败"就临时出池。
// 既守住"不因一次抖动罚号"，又能在持续坏掉时真正止损。
//
// ⚠️ **只喂这里**。带权威分类的错误（401/403/429/5xx/请求不合法）各有
// "知道原因的惩罚"，再喂连败是重复计罚。
func (s *Server) noteUnclassifiedFailure(uid string) {
	if s.pool.NoteFailures(uid) {
		s.logf("WARN: 账号 %s 连续多次「说不清原因的失败」，已临时降权出池（%s）"+
			"—— 这是连败兜底：单次不罚，但持续坏掉要止损",
			uid, s.pool.DegradeCooldown())
	}
}

// sampleModels 取前 n 个模型 ID 用于错误提示（清单太长时截断并说明总数）。
func sampleModels(cat *traectl.Catalog, n int) []string {
	ids := cat.IDs()
	if len(ids) <= n {
		return ids
	}
	out := append([]string(nil), ids[:n]...)
	out = append(out, fmt.Sprintf("…（共 %d 个）", len(ids)))
	return out
}

// trimForLog 截断日志里的长文本（按字节，仅用于日志，不涉及显示宽度）。
func trimForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// 避免把多字节字符切一半：回退到最近的合法 UTF-8 边界
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// isClientRequestError 判定上游错误是否属于"请求本身不合法"。
//
// 这类错误与账号、模型都无关：同样的请求换个账号发一样会被拒，
// 而同一个账号发别的请求完全正常。所以**不该冷却任何东西**。
//
// 已确认的成员（实测）：
//   - code 4001 "param is invalid" —— assistant 消息 content 为 null/空串/缺失时触发
//   - HTTP 400
//
// 判断依据刻意收窄：宁可漏判（退回 default 去冷却模型，代价有限且会自愈），
// 也不要把真正该罚的码误放行。
func isClientRequestError(ue *traectl.UpstreamError) bool {
	if ue.Status == http.StatusBadRequest {
		return true
	}
	switch ue.UpstreamCode() {
	case 4001: // param is invalid
		return true
	}
	return false
}

// writeTraeError 把协议错误映射为 OpenAI 错误响应。
func writeTraeError(w http.ResponseWriter, err error) {
	var ue *traectl.UpstreamError
	if errors.As(err, &ue) {
		switch {
		case ue.Status == http.StatusUnauthorized:
			writeOpenAIError(w, http.StatusUnauthorized, "upstream_unauthorized",
				"Trae 凭证失效：请在 Trae 客户端重新登录（网关会自动读取新凭据）")
		case ue.UpstreamCode() == 4011 || ue.Status == http.StatusTooManyRequests:
			writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_exceeded",
				"Trae 上游限流（4011）：账号已进入冷却，请稍后重试")
		case ue.Status >= 500:
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", ue.Error())
		default:
			writeOpenAIError(w, http.StatusBadRequest, "upstream_rejected", ue.Error())
		}
		return
	}
	var re *traectl.RefreshError
	if errors.As(err, &re) {
		writeOpenAIError(w, http.StatusUnauthorized, "token_refresh_failed",
			"Trae token 续期失败："+re.Error())
		return
	}
	msg := err.Error()
	if errors.Is(err, errAlreadyStreaming) {
		msg = strings.TrimPrefix(msg, errAlreadyStreaming.Error()+": ")
	}
	writeOpenAIError(w, http.StatusBadGateway, "upstream_error", msg)
}

// writeOpenAIError 输出 OpenAI 风格的错误信封。
func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": code, "code": code},
	})
}

// toolAccum 按 index 聚合一个工具调用的流式增量。
type toolAccum struct {
	id   string
	name string
	args strings.Builder
}

// isTruncatedArguments 判定工具参数字符串是否因分片丢失而**残缺**
// （区别于"该工具本就无参数"）。
//
//   - 空串 / 纯空白 → false（合法的无参工具）
//   - 非空但 JSON 解析失败 → true（截断）
//   - 能解析（含 null / 标量 / 数组等任何合法 JSON）→ false
func isTruncatedArguments(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	var v any
	return json.Unmarshal([]byte(trimmed), &v) != nil
}
