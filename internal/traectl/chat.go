package traectl

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// chat 上游常量。
//
// 实测（见 Trae2API 阶段一验证报告）：
//   - base 用官方 product.json 中出现的 trae-api-cn.mchost.guru（**不是** api.trae.cn）；
//   - 三级端点回退：llm_utils_chat → ide/v1/chat → agent/v3/create_agent_task；
//   - 上游恒为流式（请求体 stream 必须为 true，非流式由网关在出口聚合）；
//   - chat 上游**不校验设备指纹**（只有 OAuth 续期严格校验）。
const (
	// DefaultChatBase CN 区 chat 上游 base。
	DefaultChatBase = "https://trae-api-cn.mchost.guru"
	// chatAppID 是 chat 请求头 x-app-id 的固定值（客户端常量）。
	chatAppID = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	// chatIDEPath 主端点（轻量对话）。
	chatIDEPath = "/api/agent/v3/llm_utils_chat"
	// ideV1ChatPath 回退 1。
	ideV1ChatPath = "/api/ide/v1/chat"
	// agentTaskPath 回退 2（完整 agent）。
	agentTaskPath = "/api/agent/v3/create_agent_task"
)

// chatPaths 是端点回退顺序。
var chatPaths = []string{chatIDEPath, ideV1ChatPath, agentTaskPath}

// defaultUserAgent 是 chat 请求必须携带的 User-Agent。
//
// ⚠️ 这是实测踩出来的硬性要求（见 Trae2API 阶段一验证报告）：
// 上游**按 User-Agent 做机器人识别与限流**。Go 的默认 UA `Go-http-client/1.1`
// 会被直接拒绝，返回 HTTP 200 + 业务错误码 4011「requests have exceeded the
// rate limit」—— 错误信息完全指向"限流"，与真实原因（UA 被识别为爬虫）无关，
// 极难排查。对照实验：
//
//	Node 默认 UA (`node`)                              → 正常
//	Go 默认 UA (`Go-http-client/1.1`)                  → 4011
//	官方客户端 UA (下方这个)                            → 正常
//	axios/1.7.7                                        → 正常
//
// 该值取自 Trae 客户端 bundle 内置的 node-fetch 默认 UA
// （out/main.js: `y.has("User-Agent")||y.set("User-Agent","node-fetch/1.0 (...)")`），
// 即官方客户端实际发出的值。
const defaultUserAgent = "node-fetch/1.0 (+https://github.com/bitinn/node-fetch)"

// ChatPart 是已规范化的消息内容片段（不依赖 OpenAI 结构，由 server 层转换）。
type ChatPart struct {
	// Type "text" 或 "image_url"。
	Type string
	// Text Type=="text" 时的文本。
	Text string
	// ImageURL Type=="image_url" 时的地址（http(s) 或 data: URL）。
	ImageURL string
}

// ChatMessage 是已规范化的对话消息。
type ChatMessage struct {
	// Role system / user / assistant。
	Role string
	// Parts 内容片段。
	Parts []ChatPart
}

// Tool 是已规范化的工具定义。
type Tool struct {
	Name        string
	Description string
	// Parameters 是 JSON Schema。**object 或 JSON 字符串都接受**，
	// 内部统一序列化为字符串后发给上游（见 buildTools）。
	Parameters json.RawMessage
}

// ChatRequest 是一次对话请求（已规范化，与 OpenAI 结构解耦）。
type ChatRequest struct {
	Model      string
	Messages   []ChatMessage
	Tools      []Tool
	ToolChoice any
	MaxTokens  int
	// SessionID 为空时自动生成；非空时用于会话粘性/上游会话关联。
	SessionID string
	// RequestID 为空时自动生成。
	RequestID string
	// PromptCacheKey 上游前缀缓存键（按账号隔离，见 cachekey.go）。
	// 为空时不发送该字段（保持与"客户端直连"一致的行为）。
	PromptCacheKey string
}

// ToolCallDelta 是流式工具调用的增量。
//
// 上游帧结构（实测）：
//
//	{"index":0,"id":"call_xxx","type":"function",
//	 "function_call":{"name":"get_weather","arguments":"","partial_arguments":null,"namespace":null}}
//
// 注意字段名是 function_call（不是 OpenAI 的 tool_call），且 name 只在首帧给出、
// arguments 在后续帧流式续传，故消费方需按 Index 聚合。
type ToolCallDelta struct {
	Index int
	ID    string
	Name  string
	Args  string
}

// Usage 是上游 token_usage 事件（字段比 OpenAI 丰富，可直接映射）。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// 缓存命中（映射 OpenAI prompt_tokens_details.cached_tokens）
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	// 思维链 token（映射 OpenAI completion_tokens_details.reasoning_tokens）
	ReasoningTokens int `json:"reasoning_tokens"`
}

// Delta 是一次流式增量。
type Delta struct {
	// Content 正文增量。
	Content string
	// Reasoning 思维链增量。
	Reasoning string
	// ToolCalls 工具调用增量（可能为空）。
	ToolCalls []ToolCallDelta
	// FinishReason 仅在流结束时非空。取值 stop / length / tool_calls。
	FinishReason string
	// Usage 仅在流结束时非空。
	Usage *Usage
	// UpstreamModel 是上游真实后端模型名（来自 timing_cost 事件，仅用于诊断）。
	UpstreamModel string
	// Keepalive 表示这一帧只是心跳（来自 metadata / timing_cost / 排队事件），
	// **不含任何内容**，绝不能累加进 Content。
	//
	// 为什么需要它：上游在真正吐内容之前会先发 metadata / request_wait_in_queue
	// 等帧（排队时可能隔很久才有内容）。若把这些帧全丢掉，网关就只能等到
	// 第一个内容帧才知道"上游确实开始响应了" —— 于是要么提前写 200
	// （牺牲错误处理与重试能力），要么让客户端干等到超时。
	// 把它们转成心跳，两个问题一起解决：字节早到，且不必提前承诺 200。
	Keepalive bool
}

// Client 是 Trae 渠道的对话客户端。
type Client struct {
	Store *Store
	// Base 覆盖 chat base（默认 DefaultChatBase）。
	Base string
	// IDEVersion / IDEVersionCode 覆盖请求头（默认取产品对应值）。
	IDEVersion     string
	IDEVersionCode string
	// UserAgent 覆盖 User-Agent。**留空时必须保持 defaultUserAgent** ——
	// 见 defaultUserAgent 的注释：Go 默认 UA 会被上游限流。
	UserAgent string
	// HTTP 超时（默认 5 分钟，长回复需要）。
	//
	// ⚠️ 注意它**不是**用来管长流的：一个整体超时会把正常的长回复误杀。
	// 流中空闲由 IdleTimeout 负责（见 idle.go）。
	Timeout time.Duration
	// IdleTimeout 流中空闲阈值（超过这么久没吐数据就断流）。0 = 禁用。
	IdleTimeout time.Duration
	// OnRefresh 在续期成功后回调（用于日志/指标）。
	OnRefresh func(product string, expiresAt string)
	// Proxy 该账号专用的出站代理地址（http/https/socks5；空 = 走全局/系统代理）。
	Proxy string
}

// ⚠️ Client **不能**持有 sync.Once / http.Client 这类带状态的字段。
//
// 原因：Channel.ClientFor 需要为每个账号产出一个绑定了该账号凭据的 Client，
// 做法是拷贝一份再改 Store。而**拷贝带锁/带状态的类型是错的** ——
// go vet 会报 "assignment copies lock value"，运行时则是数据竞争，
// 且每个副本会各自建一个 Transport（连接池不共享）。
//
// 所有账号打上游主机时，同代理配置共用连接池。
var (
	chatClientOnce sync.Once
	chatClientVal  *http.Client

	chatProxyMu      sync.RWMutex
	chatProxyClients = map[string]*http.Client{}
)

// sharedChatClient 返回全部渠道共用的默认聊天 HTTP 客户端。
//
// 不设整体 Timeout：长回复（推理模型思考几分钟）是常态，用一个总时长去管
// 会误杀正常请求。超时由两层分别负责：
//   - 响应头之前：Transport.ResponseHeaderTimeout（60s）
//   - 响应头之后：流中空闲监控 IdleTimeout（见 idle.go）
func sharedChatClient() *http.Client {
	chatClientOnce.Do(func() {
		chatClientVal = newHTTPClient(0)
	})
	return chatClientVal
}

// proxyChatClient 获取或创建绑定指定代理的聊天客户端（连接池复用）。
func proxyChatClient(proxyURL string) *http.Client {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		gp := GlobalProxy()
		if gp == "" {
			return sharedChatClient()
		}
		proxyURL = gp
	}
	chatProxyMu.RLock()
	cl, ok := chatProxyClients[proxyURL]
	chatProxyMu.RUnlock()
	if ok {
		return cl
	}
	chatProxyMu.Lock()
	defer chatProxyMu.Unlock()
	if cl, ok := chatProxyClients[proxyURL]; ok {
		return cl
	}
	cl = newHTTPClientWithProxy(0, proxyURL)
	chatProxyClients[proxyURL] = cl
	return cl
}

// client 返回共享的 HTTP 客户端。
//
// c.Timeout 若非零则单独构造一个（给"需要独立超时"的场景留口子），
// 但**默认路径是按代理共享的** —— 否则每次调用都新建 Transport，连接池失效。
func (c *Client) client() *http.Client {
	if c.Timeout > 0 {
		return newHTTPClientWithProxy(c.Timeout, c.Proxy)
	}
	return proxyChatClient(c.Proxy)
}

// transport 返回底层 Transport（用于失败后清空闲连接池）。
func (c *Client) transport() http.RoundTripper { return c.client().Transport }

func (c *Client) base() string {
	if c.Base != "" {
		return strings.TrimRight(c.Base, "/")
	}
	return DefaultChatBase
}

// ensureFresh 在需要时续期（提前 refreshSkew）。
const refreshSkew = 10 * time.Minute

func (c *Client) ensureFresh(ctx context.Context) error {
	if !c.Store.NeedsRefresh(refreshSkew) {
		return nil
	}
	if err := c.Store.Refresh(ctx); err != nil {
		return err
	}
	if c.OnRefresh != nil {
		cred, _, _ := c.Store.Snapshot()
		c.OnRefresh(c.Store.Product.Key, cred.ExpiredAt)
	}
	return nil
}

// Chat 执行一次流式对话，把每个增量交给 onDelta。
//
// 行为要点：
//   - 上游恒为流式；非流式由调用方聚合（见 Aggregate）。
//   - 401 会触发一次续期并重试（token 可能在请求瞬间过期）。
//   - 三级端点回退：仅当**上一个端点返回非 2xx** 时才降级；
//     一旦拿到 2xx 就开始消费，中途出错不再降级（避免重复计费）。
func (c *Client) Chat(ctx context.Context, req ChatRequest, onDelta func(Delta) error) error {
	if err := c.ensureFresh(ctx); err != nil {
		return err
	}

	var lastErr error
	for i, path := range chatPaths {
		// 首端点前不重试；后续端点沿用同一 token（chat 不校验指纹，token 有效即可）
		err := c.chatOnce(ctx, path, req, onDelta, i == 0)
		if err == nil {
			return nil
		}
		// 已开始消费（拿到了 2xx 但流中断）→ 不再降级重试，避免重复扣费/重复输出
		var consumed *consumedError
		if errors.As(err, &consumed) {
			return consumed.err
		}
		lastErr = err
	}
	return lastErr
}

// consumedError 标记「已开始消费响应体」的错误，用于阻止端点降级重试。
type consumedError struct{ err error }

func (e *consumedError) Error() string { return e.err.Error() }
func (e *consumedError) Unwrap() error { return e.err }

// chatOnce 向单个端点发起请求并消费 SSE。
// allowRefreshRetry 为 true 时，401 会续期后重试一次。
func (c *Client) chatOnce(ctx context.Context, path string, req ChatRequest, onDelta func(Delta) error, allowRefreshRetry bool) error {
	body, err := c.buildBody(req)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("traectl: 序列化 chat 请求: %w", err)
	}

	// 流中空闲监控需要一个可取消的 context（见 idle.go）。
	// 取消它等于中断阻塞中的 body Read，让"上游永远不吐数据"这种情况能收场。
	reqCtx, cancelReq := context.WithCancel(ctx)
	defer cancelReq()

	do := func() (*http.Response, error) {
		cred, machineID, _ := c.Store.Snapshot()
		httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.base()+path, bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("traectl: 构造 chat 请求: %w", err)
		}
		for k, v := range c.headers(cred, machineID) {
			httpReq.Header.Set(k, v)
		}
		return c.client().Do(httpReq)
	}

	resp, err := do()
	if err != nil {
		// 失败的连接可能仍留在空闲池里（还没到 IdleConnTimeout），
		// 下个请求会继续捡到它 —— 于是"一次抖动"变成"连续几次失败"。
		closeIdleOnFailure(c.transport())
		return &TransportError{Op: "chat", Err: err}
	}
	if resp.StatusCode == http.StatusUnauthorized && allowRefreshRetry {
		// token 可能在请求瞬间过期：续期一次再试
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if rerr := c.Store.Refresh(ctx); rerr != nil {
			return fmt.Errorf("traectl: 401 后续期失败: %w", rerr)
		}
		if c.OnRefresh != nil {
			cred, _, _ := c.Store.Snapshot()
			c.OnRefresh(c.Store.Product.Key, cred.ExpiredAt)
		}
		resp, err = do()
		if err != nil {
			closeIdleOnFailure(c.transport())
			return &TransportError{Op: "chat(重试)", Err: err}
		}
	}

	// 非 2xx：先读错误 body（此时还没包空闲监控）
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return &UpstreamError{Status: resp.StatusCode, Body: string(raw), Path: path}
	}

	// 2xx：包上流中空闲监控再交给 SSE 解析。
	// 计时从这一刻开始 —— 首字节阶段由 Transport.ResponseHeaderTimeout 管，
	// 这里不抢跑（否则"上游正在冷启动"会被误判成"流中空闲"）。
	var respBody io.ReadCloser = resp.Body
	if c.IdleTimeout > 0 {
		respBody = monitorBody(resp.Body, c.IdleTimeout, cancelReq)
	}
	defer respBody.Close()
	return c.consume(respBody, onDelta)

}

// TransportError 表示请求**未能到达上游**（DNS 失败、连接被拒、TLS 错误、超时等）。
//
// 为什么要单独成类型：这类失败几乎总是本地网络问题，**不是账号的问题**。
// 若把它当成账号失败去冷却/熔断，一次断网就会把整个健康的账号池罚一遍
// （workbuddy2api 的明确规则：传输层失败不罚号，清连接池而不是罚账号）。
// 上层据此跳过冷却处置，只记日志。
type TransportError struct {
	Op  string
	Err error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("traectl: %s 网络失败（未到达上游，不归咎账号）: %v", e.Op, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

// UpstreamError 是上游非 2xx 响应。
type UpstreamError struct {
	Status int
	Body   string
	Path   string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("traectl: 上游 %s HTTP %d: %s", e.Path, e.Status, truncate(e.Body, 300))
}

// UpstreamCode 尽力从错误体里提取业务码（如 4001）。
func (e *UpstreamError) UpstreamCode() int {
	var v struct {
		Code int `json:"code"`
	}
	if json.Unmarshal([]byte(e.Body), &v) == nil {
		return v.Code
	}
	return 0
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// headers 构造 chat 请求头。
func (c *Client) headers(cred Credentials, machineID string) map[string]string {
	deviceID := sha256hex(machineID)[:32]
	ideVer := c.IDEVersion
	if ideVer == "" {
		ideVer = appVersion
	}
	verCode := c.IDEVersionCode
	if verCode == "" {
		// 必须纯数字：点分 build 串会被上游以 4001 expr_path=app_version_code 拒绝
		verCode = "20260901"
	}
	ua := c.UserAgent
	if ua == "" {
		// 绝不能让 Go 的默认 UA 出去 —— 会被上游判成爬虫并返回误导性的 4011。
		ua = defaultUserAgent
	}
	return map[string]string{
		"Authorization":        "Cloud-IDE-JWT " + cred.Token,
		"X-Cloudide-Token":     cred.Token,
		"x-uid":                cred.UserID,
		"x-app-id":             chatAppID,
		"x-device-id":          deviceID,
		"x-machine-id":         machineID,
		"x-request-id":         newUUID(),
		"x-ide-version":        ideVer,
		"x-ide-version-code":   verCode,
		"x-device-type":        "windows",
		"x-os-version":         "Windows 10",
		"request-traffic-type": "prod",
		"User-Agent":           ua,
		"Content-Type":         "application/json",
		"Accept":               "text/event-stream",
	}
}

// traeMessage 是发给上游的消息结构。
type traeMessage struct {
	Role    string     `json:"role"`
	Content []traePart `json:"content"`
}

// traePart 是出站消息里的一个内容片段。
//
// ⚠️ 格式来自对客户端 workbench 包的逆向（**不是猜的**）：
//
//	文本  {"type":"text","value":"…"}         ← 也兼容 {"type":"text","text":"…"}
//	图片  {"type":"image_url","value":{"mimeType":"image/png","data":"<base64>"}}
//
// 图片**必须**用 `value` 这个嵌套对象形态。我最初实现的是
// `{"type":"image","image":{"data":…,"format":…}}` —— 上游对任何请求都返回
// 业务码 4001「param is invalid」，而且**三种格式（OpenAI image_url / Anthropic
// base64 / Anthropic url）全部失败**，因为问题出在出站结构上，与入站格式无关。
//
// 客户端的判定函数（q2i）只认这 6 种 MIME：
//
//	image/png image/jpeg image/jpg image/gif image/webp image/bmp
//
// 其他类型（如 image/svg+xml、image/avif）会被客户端当作 `data` 片段处理，
// 而不是图片 —— 上游对 `data` 片段的处理方式不同，所以这里也要跟着白名单走。
type traePart struct {
	Type string `json:"type"`
	// Text 兼容形态（上游接受，实测有效）。文本片段两个字段都可以用。
	Text string `json:"text,omitempty"`
	// Value 是客户端的规范形态：文本时是字符串，图片时是 {mimeType,data} 对象。
	// 用 RawMessage 因为它是多态的。
	Value json.RawMessage `json:"value,omitempty"`
}

// traeImageValue 是图片片段的 value 结构（客户端的规范形态）。
type traeImageValue struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type traeTool struct {
	Type     string           `json:"type"`
	Function traeToolFunction `json:"function"`
}

type traeToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Parameters **必须是 JSON 字符串**：传 object 会被上游以
	// HTTP 400 `cannot unmarshal object into Go struct field
	// FunctionDefinition.tools.function.parameters of type string` 拒绝（已实测）。
	Parameters string `json:"parameters"`
}

type chatBody struct {
	Messages   []traeMessage `json:"messages"`
	Model      string        `json:"model"`
	Function   string        `json:"function"`
	Stream     bool          `json:"stream"`
	RequestID  string        `json:"request_id"`
	SessionID  string        `json:"session_id"`
	Tools      []traeTool    `json:"tools,omitempty"`
	ToolChoice any           `json:"tool_choice,omitempty"`
	MaxTokens  int           `json:"max_tokens,omitempty"`
	// PromptCacheKey 上游前缀缓存键。omitempty：为空时完全不发这个字段，
	// 避免给上游塞一个它可能不认的空值。
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
}

func (c *Client) buildBody(req ChatRequest) (*chatBody, error) {
	msgs := make([]traeMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		tm := traeMessage{Role: m.Role}
		for _, p := range m.Parts {
			switch p.Type {
			case "image_url":
				// 图片：用客户端的规范形态 {type:"image_url", value:{mimeType,data}}。
				// 转换失败时**不静默丢弃** —— 补一条文本说明，让用户在对话里
				// 看到"这张图没被发出去"，而不是收到一个看起来正常的回答。
				if part, ok := imagePart(p.ImageURL); ok {
					tm.Content = append(tm.Content, part)
				} else {
					tm.Content = append(tm.Content, traePart{
						Type: "text",
						Text: "[图片未能发送：只支持 data URL 形式的 png/jpeg/gif/webp/bmp]",
					})
				}
			default:
				tm.Content = append(tm.Content, traePart{Type: "text", Text: p.Text})
			}
		}
		if len(tm.Content) == 0 {
			tm.Content = []traePart{{Type: "text", Text: ""}}
		}
		msgs = append(msgs, tm)
	}

	sid := req.SessionID
	if sid == "" {
		sid = newUUID()
	}
	rid := req.RequestID
	if rid == "" {
		rid = sid
	}

	b := &chatBody{
		Messages:  msgs,
		Model:     req.Model,
		Function:  c.Store.Product.Function,
		Stream:    true, // 上游恒为流式
		RequestID: rid,
		SessionID: sid,
		MaxTokens: req.MaxTokens,
	}
	if len(req.Tools) > 0 {
		b.Tools = buildTools(req.Tools)
		if req.ToolChoice != nil {
			b.ToolChoice = req.ToolChoice
		}
	}
	return b, nil
}

// buildTools 把规范化工具转成上游结构，**parameters 一律序列化为 JSON 字符串**。
func buildTools(tools []Tool) []traeTool {
	out := make([]traeTool, 0, len(tools))
	for _, t := range tools {
		params := strings.TrimSpace(string(t.Parameters))
		switch {
		case params == "" || params == "null":
			params = `{"type":"object","properties":{}}`
		case params[0] == '"':
			// 已经是 JSON 字符串字面量：解出内层再原样发（避免双重编码）
			var inner string
			if json.Unmarshal(t.Parameters, &inner) == nil && strings.TrimSpace(inner) != "" {
				params = inner
			}
		}
		out = append(out, traeTool{
			Type: "function",
			Function: traeToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return out
}

// imageFromURL 解析 data: URL 或普通 URL 为上游图片结构。
// imagePart 把一个图片 URL（data URL 或 http(s) URL）转成出站的图片片段。
//
// 返回 (片段, 是否成功)。**失败时不产生片段**，由调用方决定如何提示 ——
// 塞一个空片段会让上游 4001，而用户完全看不出是图片的问题。
func imagePart(u string) (traePart, bool) {
	u = strings.TrimSpace(u)
	if u == "" {
		return traePart{}, false
	}
	if !strings.HasPrefix(u, "data:") {
		// 网络 URL：客户端也支持，但我们无法在此校验可达性。
		// 上游的图片片段只认 base64，所以网络 URL 只能当文本提示透传 ——
		// 直接塞进 mimeType/data 会得到一个空 data → 4001。
		return traePart{}, false
	}
	comma := strings.IndexByte(u, ',')
	if comma < 0 {
		return traePart{}, false
	}
	meta := u[len("data:"):comma]
	mimeType := ""
	if i := strings.IndexByte(meta, ';'); i >= 0 {
		mimeType = meta[:i]
	} else {
		mimeType = meta
	}
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	// 只接受客户端白名单里的 6 种（见 traePart 注释）
	if !isSupportedImageMime(mimeType) {
		return traePart{}, false
	}
	// 允许 base64 之外的编码吗？上游要的是 base64 字符串。
	// data URL 里没有 ;base64 标记时，data 段可能是 URL 编码的原文 —— 那种情况
	// 直接发出去必然无效，所以也拒掉。
	if !strings.Contains(strings.ToLower(meta), "base64") {
		return traePart{}, false
	}
	payload := u[comma+1:]
	if payload == "" {
		return traePart{}, false
	}
	v, err := json.Marshal(traeImageValue{MimeType: mimeType, Data: payload})
	if err != nil {
		return traePart{}, false
	}
	return traePart{Type: "image_url", Value: v}, true
}

// isSupportedImageMime 判定 MIME 是否在客户端的图片白名单里。
//
// 白名单来自客户端函数 q2i 的 switch（逆向所得，非猜测）：
// png / jpeg / jpg / gif / webp / bmp。
// 不在名单里的（svg、avif、tiff…）客户端会当普通 `data` 片段处理，
// 走的是另一条上游路径 —— 这里跟着拒掉，让上层给出明确提示。
func isSupportedImageMime(mt string) bool {
	switch mt {
	case "image/png", "image/jpeg", "image/jpg", "image/gif", "image/webp", "image/bmp":
		return true
	}
	return false
}

// ---- SSE 解析 ----

// sseOutput 是 output 事件。
//
// 实测：response 与 reasoning_content **同帧互斥**（一帧只填一个，另一个为 null）。
type sseOutput struct {
	Response   string          `json:"response"`
	Reasoning  string          `json:"reasoning_content"`
	ToolCalls  []sseToolCall   `json:"tool_calls"`
	Multimodal json.RawMessage `json:"multimodal_contents"`
	Phase      any             `json:"phase"`
}

type sseToolCall struct {
	Index        int    `json:"index"`
	ID           string `json:"id"`
	Type         string `json:"type"`
	FunctionCall *struct {
		Name             string `json:"name"`
		Arguments        string `json:"arguments"`
		PartialArguments any    `json:"partial_arguments"`
		Namespace        any    `json:"namespace"`
	} `json:"function_call"`
}

// sseExtraInfo 是 extra_info 事件。
//
// ⚠️ 实测陷阱 T1：其中的 Content 字段是**完整思维链全文**，不是正文。
// 若把它当正文累加，会把整段思考混进回复（本项目的原型实现就踩过这个坑）。
type sseExtraInfo struct {
	Model   string `json:"model"`
	Content string `json:"content"`
}

type sseDone struct {
	FinishReason string `json:"finish_reason"`
}

type sseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Error   string `json:"error"`
}

// consume 解析上游 SSE 并回调增量。
func (c *Client) consume(r io.Reader, onDelta func(Delta) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20) // 单帧可能较大（含 multimodal）

	var (
		event    string
		sawTool  bool
		finish   string
		usage    *Usage
		upModel  string
		errEvent *sseError
		// lastKeepalive 是上一次发心跳的时刻（限流用，避免灌满无意义帧）。
		lastKeepalive time.Time
	)

	emit := func(d Delta) error {
		if onDelta == nil {
			return nil
		}
		return onDelta(d)
	}

	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			event = ""
			continue
		}
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
			continue
		case strings.HasPrefix(line, "id:"):
			// 实测陷阱 T3：SSE 流里含 id:N 行，必须忽略而不是报错
			continue
		case !strings.HasPrefix(line, "data:"):
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" {
			continue
		}

		switch event {
		case "output":
			var o sseOutput
			if err := json.Unmarshal([]byte(data), &o); err != nil {
				continue // 未知帧形状：跳过而不是中断整条流
			}
			if o.Reasoning != "" {
				if err := emit(Delta{Reasoning: o.Reasoning}); err != nil {
					return &consumedError{err}
				}
			}
			if o.Response != "" {
				if err := emit(Delta{Content: o.Response}); err != nil {
					return &consumedError{err}
				}
			}
			for _, tc := range o.ToolCalls {
				sawTool = true
				d := ToolCallDelta{Index: tc.Index, ID: tc.ID}
				if tc.FunctionCall != nil {
					d.Name = tc.FunctionCall.Name
					d.Args = tc.FunctionCall.Arguments
				}
				if err := emit(Delta{ToolCalls: []ToolCallDelta{d}}); err != nil {
					return &consumedError{err}
				}
			}

		case "extra_info":
			// T1：只取 model 做诊断，**绝不当正文**。
			var x sseExtraInfo
			if json.Unmarshal([]byte(data), &x) == nil && x.Model != "" {
				upModel = x.Model
			}

		case "token_usage":
			var u Usage
			if json.Unmarshal([]byte(data), &u) == nil {
				usage = &u
			}

		case "done":
			var d sseDone
			if json.Unmarshal([]byte(data), &d) == nil {
				finish = d.FinishReason
			}

		case "error":
			var e sseError
			if json.Unmarshal([]byte(data), &e) == nil {
				errEvent = &e
			}

		default:
			// metadata / timing_cost / queue_begin / queue_end /
			// request_wait_in_queue / progress_notice 等一律**不进正文**
			//（排队位置若进 content 会污染对话历史），但转成**心跳**发出去：
			// 让网关能据此尽早开流（见 Delta.Keepalive 注释），
			// 同时避免客户端在排队期间干等而超时。
			//
			// 限流到 2 秒一次：上游这些帧可能很密（timing_cost 与进度通知），
			// 全转发会把 SSE 流灌满无意义的注释帧。
			if now := time.Now(); now.Sub(lastKeepalive) >= 2*time.Second {
				lastKeepalive = now
				if err := emit(Delta{Keepalive: true, UpstreamModel: upModel}); err != nil {
					return &consumedError{err}
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return &consumedError{fmt.Errorf("traectl: 读取 SSE: %w", err)}
	}

	if errEvent != nil {
		return &consumedError{&UpstreamError{
			Status: 200, // 上游用 200 + error 事件表达错误
			Body:   fmt.Sprintf(`{"code":%d,"message":%q}`, errEvent.Code, errEvent.Message),
			Path:   "sse:error",
		}}
	}

	// 实测陷阱：上游发出 tool_calls 时 finish_reason 仍是 "stop"，
	// 必须改写为 "tool_calls"，否则客户端不会执行工具。
	if sawTool && (finish == "" || finish == "stop") {
		finish = "tool_calls"
	}
	if finish == "" {
		finish = "stop"
	}
	return emit(Delta{FinishReason: finish, Usage: usage, UpstreamModel: upModel})
}

// Aggregate 把流式增量聚合成完整结果（非流式出口用）。
type Aggregated struct {
	Content       string
	Reasoning     string
	ToolCalls     []ToolCallDelta
	FinishReason  string
	Usage         *Usage
	UpstreamModel string
}

// Aggregate 消费整个流并聚合（内部用 Chat）。
func (c *Client) Aggregate(ctx context.Context, req ChatRequest) (*Aggregated, error) {
	agg := &Aggregated{}
	byIndex := map[int]*ToolCallDelta{}
	var order []int
	err := c.Chat(ctx, req, func(d Delta) error {
		agg.Content += d.Content
		agg.Reasoning += d.Reasoning
		for _, tc := range d.ToolCalls {
			cur, ok := byIndex[tc.Index]
			if !ok {
				cur = &ToolCallDelta{Index: tc.Index}
				byIndex[tc.Index] = cur
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			if tc.Name != "" {
				cur.Name = tc.Name
			}
			cur.Args += tc.Args
		}
		if d.FinishReason != "" {
			agg.FinishReason = d.FinishReason
		}
		if d.Usage != nil {
			agg.Usage = d.Usage
		}
		if d.UpstreamModel != "" {
			agg.UpstreamModel = d.UpstreamModel
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, i := range order {
		agg.ToolCalls = append(agg.ToolCalls, *byIndex[i])
	}
	return agg, nil
}

// sha256hex 返回 SHA-256 的 hex。
func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// newUUID 生成 RFC 4122 v4 UUID（不引入第三方依赖）。
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败在实践中不可恢复；退化用时间戳保证非空唯一
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
