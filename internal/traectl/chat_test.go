package traectl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// testStore 构造一个 token 长期有效的 Store，避免测试触发真实续期。
func testStore(t *testing.T, p Product) *Store {
	t.Helper()
	return &Store{
		Product: p,
		Cred: Credentials{
			Token:     "test-token",
			UserID:    "916348159159136",
			Host:      DefaultUGHost,
			ExpiredAt: formatTraeTime(time.Now().Add(24 * time.Hour)),
		},
		MachineID: "690ad7a95fbe7fa6aabbccddeeff00112233445566778899aabbccddeeff0011",
		DeviceID:  "3366058269681403",
	}
}

// sseServer 起一个回放固定 SSE 文本的假上游。
func sseServer(t *testing.T, sse string, capture *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			*capture = map[string]any{"body": body, "header": r.Header.Clone(), "path": r.URL.Path}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
}

// TestChatSSEParsing 覆盖实测发现的全部 SSE 陷阱：
// T1 extra_info.content 是思维链全文不得当正文；T2 response/reasoning 互斥；
// T3 存在 id: 行；finish_reason 需由 stop 改写为 tool_calls。
func TestChatSSEParsing(t *testing.T) {
	sse := strings.Join([]string{
		"id:1",
		"event: metadata",
		`data: {"model":"","session_id":"s1"}`,
		"",
		"id:2",
		"event: timing_cost",
		`data: {"name":"llm_raw_chat_v2","provider_model_name":"ep-20260730175830-94ltc"}`,
		"",
		"id:3",
		"event: output",
		`data: {"response":"","reasoning_content":"思考","tool_calls":null,"multimodal_contents":null,"phase":null}`,
		"",
		"id:4",
		"event: output",
		`data: {"response":"你好","reasoning_content":null,"tool_calls":null,"multimodal_contents":null,"phase":null}`,
		"",
		"id:5",
		"event: output",
		`data: {"response":"","reasoning_content":null,"tool_calls":[{"index":0,"id":"call_abc","type":"function","function_call":{"name":"get_weather","arguments":"","partial_arguments":null,"namespace":null}}],"multimodal_contents":null,"phase":null}`,
		"",
		"id:6",
		"event: output",
		`data: {"response":"","reasoning_content":null,"tool_calls":[{"index":0,"id":"","type":"","function_call":{"name":"","arguments":"{\"city\": \"北京\"}","partial_arguments":null,"namespace":null}}],"multimodal_contents":null,"phase":null}`,
		"",
		"id:7",
		// T1：content 是完整思维链全文，绝不能被当成正文
		"event: extra_info",
		`data: {"model":"ep-20260730175830-94ltc","version":1,"content":"这是完整思维链全文，不该出现在正文里","input_token":30,"expire_time":1}`,
		"",
		"id:8",
		"event: token_usage",
		`data: {"prompt_tokens":48,"completion_tokens":31,"total_tokens":79,"cache_read_input_tokens":7,"reasoning_tokens":30}`,
		"",
		"id:9",
		// 上游发出 tool_calls 时 finish_reason 仍是 stop
		"event: done",
		`data: {"finish_reason":"stop"}`,
		"",
	}, "\n")

	srv := sseServer(t, sse, nil)
	defer srv.Close()

	c := &Client{Store: testStore(t, Products[0]), Base: srv.URL}
	var content, reasoning string
	var tools []ToolCallDelta
	var finish string
	var usage *Usage
	var upModel string

	err := c.Chat(context.Background(), ChatRequest{
		Model:    "glm-5.2",
		Messages: []ChatMessage{{Role: "user", Parts: []ChatPart{{Type: "text", Text: "hi"}}}},
	}, func(d Delta) error {
		content += d.Content
		reasoning += d.Reasoning
		tools = append(tools, d.ToolCalls...)
		if d.FinishReason != "" {
			finish = d.FinishReason
		}
		if d.Usage != nil {
			usage = d.Usage
		}
		if d.UpstreamModel != "" {
			upModel = d.UpstreamModel
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if content != "你好" {
		t.Errorf("正文 = %q want %q（T1：extra_info.content 混入正文？）", content, "你好")
	}
	if strings.Contains(content, "完整思维链") {
		t.Error("T1 未处理：extra_info.content 混进了正文")
	}
	if reasoning != "思考" {
		t.Errorf("思维链 = %q want %q", reasoning, "思考")
	}
	if len(tools) != 2 {
		t.Fatalf("工具调用增量 = %d want 2", len(tools))
	}
	if tools[0].ID != "call_abc" || tools[0].Name != "get_weather" {
		t.Errorf("首帧工具调用不符: %+v", tools[0])
	}
	if tools[1].Args != `{"city": "北京"}` {
		t.Errorf("次帧 arguments = %q", tools[1].Args)
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %q want tool_calls（必须由 stop 改写）", finish)
	}
	if usage == nil || usage.TotalTokens != 79 || usage.ReasoningTokens != 30 || usage.CacheReadInputTokens != 7 {
		t.Errorf("usage 解析不符: %+v", usage)
	}
	if upModel != "ep-20260730175830-94ltc" {
		t.Errorf("上游模型名 = %q", upModel)
	}
}

// TestChatAggregate 聚合出口：工具调用按 index 归并。
func TestChatAggregate(t *testing.T) {
	sse := strings.Join([]string{
		"event: output",
		`data: {"response":"A","reasoning_content":null,"tool_calls":null}`,
		"",
		"event: output",
		`data: {"response":"B","reasoning_content":"R","tool_calls":null}`,
		"",
		"event: output",
		`data: {"response":"","reasoning_content":null,"tool_calls":[{"index":0,"id":"c1","type":"function","function_call":{"name":"f","arguments":"{\"a\""}}]}`,
		"",
		"event: output",
		`data: {"response":"","reasoning_content":null,"tool_calls":[{"index":0,"id":"","type":"","function_call":{"name":"","arguments":":1}"}}]}`,
		"",
		"event: token_usage",
		`data: {"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}`,
		"",
		"event: done",
		`data: {"finish_reason":"stop"}`,
		"",
	}, "\n")
	srv := sseServer(t, sse, nil)
	defer srv.Close()

	c := &Client{Store: testStore(t, Products[0]), Base: srv.URL}
	agg, err := c.Aggregate(context.Background(), ChatRequest{
		Model:    "glm-5.2",
		Messages: []ChatMessage{{Role: "user", Parts: []ChatPart{{Type: "text", Text: "x"}}}},
	})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if agg.Content != "AB" || agg.Reasoning != "R" {
		t.Errorf("聚合内容不符: content=%q reasoning=%q", agg.Content, agg.Reasoning)
	}
	if len(agg.ToolCalls) != 1 {
		t.Fatalf("工具调用数 = %d want 1", len(agg.ToolCalls))
	}
	if got := agg.ToolCalls[0]; got.Name != "f" || got.ID != "c1" || got.Args != `{"a":1}` {
		t.Errorf("工具调用聚合不符: %+v", got)
	}
	if agg.FinishReason != "tool_calls" {
		t.Errorf("finish = %q want tool_calls", agg.FinishReason)
	}
	if agg.Usage == nil || agg.Usage.TotalTokens != 3 {
		t.Errorf("usage = %+v", agg.Usage)
	}
}

// TestChatKeepaliveOnQueueFrames 上游在真正吐内容前发的元信息帧必须转成**心跳**。
//
// 为什么重要：Trae 在排队时会先发 metadata / request_wait_in_queue，然后长时间
// 没有内容。若把这些帧全丢掉，网关就只能等到第一个内容帧才知道上游开始响应了 ——
// 于是要么提前写 200（牺牲错误处理与重试能力），要么让客户端干等到超时。
// 转成心跳后两个问题一起解决。
//
// 同时验证：排队位置**绝不能进正文**（否则会污染对话历史）。
func TestChatKeepaliveOnQueueFrames(t *testing.T) {
	sse := strings.Join([]string{
		"event: metadata",
		`data: {"model":"","session_id":"s1"}`,
		"",
		"event: request_wait_in_queue",
		`data: {"data":{"position":1234}}`,
		"",
		"event: timing_cost",
		`data: {"name":"llm_raw_chat_v2","provider_model_name":"ep-x"}`,
		"",
		"event: output",
		`data: {"response":"你好","reasoning_content":null,"tool_calls":null}`,
		"",
		"event: done",
		`data: {"finish_reason":"stop"}`,
		"",
	}, "\n")
	srv := sseServer(t, sse, nil)
	defer srv.Close()

	c := &Client{Store: testStore(t, Products[0]), Base: srv.URL}
	var kinds []string
	var content string
	keepaliveBeforeContent := false

	err := c.Chat(context.Background(), ChatRequest{
		Model:    "glm-5.2",
		Messages: []ChatMessage{{Role: "user", Parts: []ChatPart{{Type: "text", Text: "hi"}}}},
	}, func(d Delta) error {
		switch {
		case d.Keepalive:
			kinds = append(kinds, "keepalive")
			if content == "" {
				keepaliveBeforeContent = true
			}
		case d.Content != "":
			kinds = append(kinds, "content")
			content += d.Content
		case d.FinishReason != "":
			kinds = append(kinds, "finish")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if !keepaliveBeforeContent {
		t.Fatalf("应在内容之前发出心跳，实际帧序列: %v", kinds)
	}
	if content != "你好" {
		t.Errorf("正文 = %q want 你好", content)
	}
	if strings.Contains(content, "1234") {
		t.Error("排队位置混进了正文 —— 会污染对话历史")
	}
	n := 0
	for _, k := range kinds {
		if k == "keepalive" {
			n++
		}
	}
	if n < 1 {
		t.Errorf("心跳帧数 = %d want >= 1", n)
	}
}

// TestChatKeepaliveThrottled 心跳必须限流（上游元信息帧可能很密，
// 全转发会把 SSE 流灌满无意义的注释帧）。
func TestChatKeepaliveThrottled(t *testing.T) {
	var b strings.Builder
	// 30 帧密集元信息（同一瞬间到达，应只触发 1 次心跳）
	for i := 0; i < 30; i++ {
		b.WriteString("event: timing_cost\n")
		b.WriteString("data: {\"name\":\"x\"}\n\n")
	}
	b.WriteString("event: output\ndata: {\"response\":\"ok\"}\n\n")
	b.WriteString("event: done\ndata: {\"finish_reason\":\"stop\"}\n\n")

	srv := sseServer(t, b.String(), nil)
	defer srv.Close()

	c := &Client{Store: testStore(t, Products[0]), Base: srv.URL}
	keepalives := 0
	err := c.Chat(context.Background(), ChatRequest{Model: "m"}, func(d Delta) error {
		if d.Keepalive {
			keepalives++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if keepalives != 1 {
		t.Errorf("密集元信息帧应只产生 1 次心跳（2s 限流），实际 %d 次", keepalives)
	}
}

// TestChatErrorEvent 上游用 200 + error 事件表达错误，必须转成错误而不是静默成功。
func TestChatErrorEvent(t *testing.T) {
	sse := strings.Join([]string{
		"event: error",
		`data: {"code":1001,"error":"","message":"We're sorry, but we are not able to authenticate you."}`,
		"",
	}, "\n")
	srv := sseServer(t, sse, nil)
	defer srv.Close()

	c := &Client{Store: testStore(t, Products[0]), Base: srv.URL}
	err := c.Chat(context.Background(), ChatRequest{Model: "m", Messages: nil}, func(Delta) error { return nil })
	if err == nil {
		t.Fatal("error 事件应导致返回错误")
	}
	var ue *UpstreamError
	if !asUpstream(err, &ue) {
		t.Fatalf("错误类型不符: %v", err)
	}
	if ue.UpstreamCode() != 1001 {
		t.Errorf("业务码 = %d want 1001", ue.UpstreamCode())
	}
}

// asUpstream 沿 Unwrap 链找 *UpstreamError。
func asUpstream(err error, target **UpstreamError) bool {
	for err != nil {
		if ue, ok := err.(*UpstreamError); ok {
			*target = ue
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestBuildToolsParameters 锁定实测结论：parameters 必须是 JSON 字符串。
func TestBuildToolsParameters(t *testing.T) {
	obj := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)
	str := json.RawMessage(`"{\"type\":\"object\"}"`)
	empty := json.RawMessage(``)
	null := json.RawMessage(`null`)

	got := buildTools([]Tool{
		{Name: "a", Parameters: obj},
		{Name: "b", Parameters: str},
		{Name: "c", Parameters: empty},
		{Name: "d", Parameters: null},
	})
	if len(got) != 4 {
		t.Fatalf("工具数 = %d", len(got))
	}
	if got[0].Function.Parameters != string(obj) {
		t.Errorf("object 形态应原样序列化，得到 %q", got[0].Function.Parameters)
	}
	if got[1].Function.Parameters != `{"type":"object"}` {
		t.Errorf("已是字符串字面量应解出内层（避免双重编码），得到 %q", got[1].Function.Parameters)
	}
	for _, i := range []int{2, 3} {
		if !strings.HasPrefix(got[i].Function.Parameters, `{"type":"object"`) {
			t.Errorf("空/null 应补默认 schema，得到 %q", got[i].Function.Parameters)
		}
	}
	// 关键：发出去的一定是 JSON 字符串，绝不能被序列化成 object
	raw, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"parameters":"{`) {
		t.Errorf("parameters 未被序列化为字符串: %s", raw)
	}
}

// TestBuildBody 请求体：function 随产品、request_id/session_id 必填、stream 恒 true。
func TestBuildBody(t *testing.T) {
	for _, p := range Products {
		c := &Client{Store: testStore(t, p)}
		b, err := c.buildBody(ChatRequest{
			Model:    "glm-5.2",
			Messages: []ChatMessage{{Role: "user", Parts: []ChatPart{{Type: "text", Text: "hi"}}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if b.Function != p.Function {
			t.Errorf("%s: function = %q want %q", p.Key, b.Function, p.Function)
		}
		if !b.Stream {
			t.Error("stream 必须为 true（上游恒流式）")
		}
		if b.RequestID == "" || b.SessionID == "" {
			t.Error("request_id / session_id 必填 —— 缺失会让上游回误导性 4011/4001 错误")
		}
		if len(b.Messages) != 1 || b.Messages[0].Content[0].Text != "hi" {
			t.Errorf("消息构造不符: %+v", b.Messages)
		}
	}
}

// TestImagePart 图片片段必须用**客户端的规范格式**。
//
// ⚠️ 这是本项目踩过的一个大坑的回归测试：我最初实现的是
// `{"type":"image","image":{"data":…,"format":…}}` —— 而客户端用的是
//
//	{"type":"image_url","value":{"mimeType":"image/png","data":"<base64>"}}
//
// 格式错的后果是**上游对所有带图的请求返回业务码 4001「param is invalid」**，
// 而且因为问题在**出站结构**上，三种入站格式（OpenAI image_url / Anthropic
// base64 / Anthropic url）会**全部失败** —— 极易误判成"入站格式没解析对"。
func TestImagePart(t *testing.T) {
	// 合法：data URL + 白名单 MIME
	p, ok := imagePart("data:image/png;base64,QUJD")
	if !ok {
		t.Fatal("合法 data URL 应被接受")
	}
	if p.Type != "image_url" {
		t.Errorf("type = %q want image_url（不是 image）", p.Type)
	}
	var v traeImageValue
	if err := json.Unmarshal(p.Value, &v); err != nil {
		t.Fatalf("value 不是合法 JSON: %v", err)
	}
	if v.MimeType != "image/png" || v.Data != "QUJD" {
		t.Errorf("value 不符: %+v", v)
	}
	// 字段名必须是 camelCase 的 mimeType（不是 mime_type）
	if !strings.Contains(string(p.Value), `"mimeType"`) {
		t.Errorf("字段名应为 mimeType: %s", p.Value)
	}

	// 白名单内的其他 MIME
	for _, mt := range []string{"image/jpeg", "image/jpg", "image/gif", "image/webp", "image/bmp"} {
		if _, ok := imagePart("data:" + mt + ";base64,QUJD"); !ok {
			t.Errorf("白名单 MIME %s 应被接受", mt)
		}
	}

	// 白名单外：客户端会当 `data` 片段走另一条路径，我们必须拒掉
	for _, mt := range []string{"image/svg+xml", "image/avif", "image/tiff", "text/plain"} {
		if _, ok := imagePart("data:" + mt + ";base64,QUJD"); ok {
			t.Errorf("非白名单 MIME %s 不该被当作图片发出（上游会 4001）", mt)
		}
	}

	// 非法输入：空、非 data URL、无 base64 标记、空 payload、无逗号
	for _, bad := range []string{
		"", "   ", "https://example.com/a.png",
		"data:image/png,QUJD",    // 缺 ;base64
		"data:image/png;base64,", // 空 payload
		"data:image/png;base64",  // 无逗号
	} {
		if _, ok := imagePart(bad); ok {
			t.Errorf("非法输入 %q 不该产生图片片段", bad)
		}
	}
}

// TestHeaders 请求头：必须含 Cloud-IDE-JWT / x-app-id / 纯数字 version-code。
func TestHeaders(t *testing.T) {
	c := &Client{Store: testStore(t, Products[0])}
	cred, mid, _ := c.Store.Snapshot()
	h := c.headers(cred, mid)
	if h["Authorization"] != "Cloud-IDE-JWT test-token" {
		t.Errorf("Authorization = %q", h["Authorization"])
	}
	if h["X-Cloudide-Token"] != "test-token" {
		t.Errorf("X-Cloudide-Token = %q", h["X-Cloudide-Token"])
	}
	if h["x-app-id"] != chatAppID {
		t.Errorf("x-app-id = %q", h["x-app-id"])
	}
	if len(h["x-device-id"]) != 32 {
		t.Errorf("x-device-id 长度 = %d want 32（sha256(machineId) 前 32）", len(h["x-device-id"]))
	}
	for _, r := range h["x-ide-version-code"] {
		if r < '0' || r > '9' {
			t.Errorf("x-ide-version-code 必须纯数字，得到 %q（点分 build 串会报 4001）", h["x-ide-version-code"])
			break
		}
	}
	if h["request-traffic-type"] != "prod" {
		t.Errorf("request-traffic-type = %q", h["request-traffic-type"])
	}
	// 关键回归防护：绝不能让 Go 默认 UA 出去（会被上游以误导性的 4011 拒绝）
	if h["User-Agent"] != defaultUserAgent {
		t.Errorf("User-Agent = %q want %q", h["User-Agent"], defaultUserAgent)
	}
	if strings.Contains(h["User-Agent"], "Go-http-client") {
		t.Fatal("User-Agent 是 Go 默认值 —— 会被上游判成爬虫并返回 4011")
	}
	// 显式覆盖必须生效
	c2 := &Client{Store: testStore(t, Products[0]), UserAgent: "custom/1.0"}
	cred2, mid2, _ := c2.Store.Snapshot()
	if got := c2.headers(cred2, mid2)["User-Agent"]; got != "custom/1.0" {
		t.Errorf("UserAgent 覆盖未生效: %q", got)
	}
}

// TestUpstreamErrorStatus 非 2xx 必须变成 *UpstreamError 且能被降级逻辑识别为「未消费」。
func TestUpstreamErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":4001,"message":"bad request"}`))
	}))
	defer srv.Close()

	c := &Client{Store: testStore(t, Products[0]), Base: srv.URL}
	err := c.Chat(context.Background(), ChatRequest{Model: "m"}, func(Delta) error { return nil })
	if err == nil {
		t.Fatal("应返回错误")
	}
	// 三级回退都失败后，最终错误应是最后一个端点的 UpstreamError
	var ue *UpstreamError
	if !asUpstream(err, &ue) {
		t.Fatalf("错误类型不符: %v", err)
	}
	if ue.UpstreamCode() != 4001 {
		t.Errorf("业务码 = %d want 4001", ue.UpstreamCode())
	}
}

// TestChatLive 真实上游联调（默认跳过；TRAE_LIVE_TEST=1 时启用）。
//
// 会消耗真实额度，故默认不跑。启用方式：
//
//	$env:TRAE_LIVE_TEST="1"; go test ./internal/traectl/ -run TestChatLive -v
func TestChatLive(t *testing.T) {
	if os.Getenv("TRAE_LIVE_TEST") != "1" {
		t.Skip("需 TRAE_LIVE_TEST=1（会消耗真实额度）")
	}
	for _, p := range Products {
		t.Run(p.Key, func(t *testing.T) {
			path, err := DefaultStoragePath(p)
			if err != nil {
				t.Skip(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Skip("未安装")
			}
			s, err := OpenStore(p, "")
			if err != nil {
				t.Skipf("打开凭据失败: %v", err)
			}
			c := &Client{Store: s}
			agg, err := c.Aggregate(context.Background(), ChatRequest{
				Model:    "glm-5.2",
				Messages: []ChatMessage{{Role: "user", Parts: []ChatPart{{Type: "text", Text: "只回复两个字：你好"}}}},
			})
			if err != nil {
				t.Fatalf("真实上游调用失败: %v", err)
			}
			t.Logf("正文=%q 思维链=%d字 usage=%+v 上游模型=%s",
				agg.Content, len([]rune(agg.Reasoning)), agg.Usage, agg.UpstreamModel)
			if strings.TrimSpace(agg.Content) == "" {
				t.Error("正文为空")
			}
		})
	}
}

// TestChatLiveTools 真实上游工具调用联调（默认跳过）。
func TestChatLiveTools(t *testing.T) {
	if os.Getenv("TRAE_LIVE_TOOLS") != "1" {
		t.Skip("需 TRAE_LIVE_TOOLS=1（会消耗真实额度）")
	}
	s, err := OpenStore(Products[0], "")
	if err != nil {
		t.Skipf("打开凭据失败: %v", err)
	}
	c := &Client{Store: s}
	agg, err := c.Aggregate(context.Background(), ChatRequest{
		Model:    "glm-5.2",
		Messages: []ChatMessage{{Role: "user", Parts: []ChatPart{{Type: "text", Text: "北京天气如何？必须调用工具。"}}}},
		Tools: []Tool{{
			Name:        "get_weather",
			Description: "查询城市天气",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		}},
	})
	if err != nil {
		t.Fatalf("真实上游工具调用失败: %v", err)
	}
	t.Logf("finish=%s tool_calls=%+v", agg.FinishReason, agg.ToolCalls)
	if agg.FinishReason != "tool_calls" || len(agg.ToolCalls) == 0 {
		t.Errorf("未拿到工具调用: finish=%s tools=%d", agg.FinishReason, len(agg.ToolCalls))
	}
}

// TestChatKeepAliveStreamTermination 关键防护：当上游发送 event: done 或 event: error 但保持 TCP 连接不关闭时，
// 客户端必须立刻结束消费并返回，绝不能一直阻塞在 Scanner.Scan() 中等待 EOF 导致客户端停住/超时卡顿。
func TestChatKeepAliveStreamTermination(t *testing.T) {
	// 1. 测试 event: done 场景
	blockCh1 := make(chan struct{})
	srvDone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 模拟上游吐了几行后发送 done
		fmt.Fprintf(w, "event: output\ndata: {\"response\":\"第一行\\n\"}\n\n")
		fmt.Fprintf(w, "event: output\ndata: {\"response\":\"第二行\\n\"}\n\n")
		fmt.Fprintf(w, "event: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 关键点：保持连接打开不退出 handler，模拟 HTTP Keep-Alive 未断流
		select {
		case <-blockCh1:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(blockCh1)
		srvDone.Close()
	}()

	c := &Client{Store: testStore(t, Products[0]), Base: srvDone.URL}
	doneCh := make(chan struct{})
	var content string
	var finish string

	go func() {
		err := c.Chat(context.Background(), ChatRequest{
			Model:    "glm-5.2",
			Messages: []ChatMessage{{Role: "user", Parts: []ChatPart{{Type: "text", Text: "hi"}}}},
		}, func(d Delta) error {
			content += d.Content
			if d.FinishReason != "" {
				finish = d.FinishReason
			}
			return nil
		})
		if err != nil {
			t.Errorf("Chat error: %v", err)
		}
		close(doneCh)
	}()

	select {
	case <-doneCh:
		if content != "第一行\n第二行\n" {
			t.Errorf("content = %q, want %q", content, "第一行\n第二行\n")
		}
		if finish != "stop" {
			t.Errorf("finish = %q, want stop", finish)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event: done 到达后未能立刻断流结束，流卡死阻塞在扫描器中！")
	}

	// 2. 测试 event: error（如 4011 限流）未断流场景
	blockCh2 := make(chan struct{})
	srvErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		fmt.Fprintf(w, "event: error\ndata: {\"code\":4011,\"message\":\"requests have exceeded the rate limit\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-blockCh2:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(blockCh2)
		srvErr.Close()
	}()

	cErr := &Client{Store: testStore(t, Products[0]), Base: srvErr.URL}
	errDoneCh := make(chan struct{})
	go func() {
		err := cErr.Chat(context.Background(), ChatRequest{
			Model:    "glm-5.2",
			Messages: []ChatMessage{{Role: "user", Parts: []ChatPart{{Type: "text", Text: "hi"}}}},
		}, func(d Delta) error {
			return nil
		})
		if err == nil {
			t.Error("4011 应返回错误，实际 nil")
		}
		var ue *UpstreamError
		if !strings.Contains(err.Error(), "4011") {
			t.Errorf("错误信息应包含 4011，实际: %v", err)
		}
		_ = ue
		close(errDoneCh)
	}()

	select {
	case <-errDoneCh:
		// 成功立刻返回
	case <-time.After(2 * time.Second):
		t.Fatal("event: error 到达后未能立刻返回，4011 限流卡死阻塞在扫描器中！")
	}
}

var _ = fmt.Sprintf
