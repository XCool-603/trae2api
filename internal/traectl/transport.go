package traectl

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 本文件是出站 Transport 的**单一事实来源**：所有连接层参数集中在此，
// 便于测试回读断言，也避免"三处各写一个超时"的口径漂移。
//
// 要解决的问题：**半死连接**。TCP 连接可以进入一种"两端都以为还活着、
// 但数据发不出去也收不回来"的状态（NAT 表项过期、对端崩溃未发 RST、
// 中间设备静默丢包）。此时：
//
//   - 默认 `net.Dialer` **没有超时上限**，半开连接可以卡到系统 TCP 重传窗口
//     （实测有单次 TTFB 卡 936 秒的记录）；
//   - 默认 keepalive 是 **2 小时**才发第一个探测 —— 在 NAT 黑洞里，
//     2 小时足够一条死连接被反复复用；
//   - 连接池默认保留空闲连接 90 秒，而 WAF/网关侧常主动掐闲置连接，
//     池里的多半已经死了。
//
// 四道闸分别管"建立期"与"复用期"。
const (
	// dialTimeout TCP 建连上限。半死连接的第一道闸：连不上就快速失败去轮转换号，
	// 而不是干等系统重传。
	dialTimeout = 10 * time.Second

	// dialKeepAlive TCP keepalive 探测周期。默认 2h 太长 ——
	// 15s 周期能让死连接在 15~30s 内被内核掐掉（RST/ETIMEDOUT），
	// 复用侧立即感知，而不是卡到重传窗口。
	dialKeepAlive = 15 * time.Second

	// tlsHandshakeTimeout TLS 握手上限。**这一项此前完全缺失**：
	// 握手挂起时没有任何层兜底（ResponseHeaderTimeout 只在请求写完后才计时），
	// 只能干等到 Client.Timeout。
	tlsHandshakeTimeout = 10 * time.Second

	// idleConnTimeout 空闲连接池保留时长。
	// 从 30s 提升到 90s：避免正常思考停顿后频繁断连导致重新进行 1~2s 的 TLS 握手。
	idleConnTimeout = 90 * time.Second

	// responseHeaderTimeout 响应头到达的硬上限。
	//
	// ⚠️ 语义要点：它**只计响应头到达之前**的时长，头到达后 SSE 长流不受影响
	//（流中空闲由 idle.go 单独监控）。所以调小它不会误杀长流。
	//
	// 取 60s：实测成功请求的 TTFB 最坏到过 16s（慢模型冷启动），
	// 60s ≈ 3.75× 观测最坏值，留足余量；同时把半死连接的单请求卡死
	// 从 2 分钟压到 1 分钟。
	responseHeaderTimeout = 60 * time.Second

	maxIdleConns        = 200
	maxIdleConnsPerHost = 50
)

// newDialer 构造出站拨号器（参数集中，供测试回读）。
func newDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: dialKeepAlive,
	}
}

var (
	globalProxyMu  sync.RWMutex
	globalProxyURL string
)

// SetGlobalProxy 设置全局出站代理（留空表示使用系统环境变量代理）。
func SetGlobalProxy(proxyURL string) {
	globalProxyMu.Lock()
	defer globalProxyMu.Unlock()
	globalProxyURL = strings.TrimSpace(proxyURL)
}

// GlobalProxy 获取当前配置的全局代理。
func GlobalProxy() string {
	globalProxyMu.RLock()
	defer globalProxyMu.RUnlock()
	return globalProxyURL
}

func proxyFuncFor(proxyURL string) func(*http.Request) (*url.URL, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		gp := GlobalProxy()
		if gp != "" {
			if u, err := url.Parse(gp); err == nil {
				return http.ProxyURL(u)
			}
		}
		return http.ProxyFromEnvironment
	}
	if u, err := url.Parse(proxyURL); err == nil {
		return http.ProxyURL(u)
	}
	return http.ProxyFromEnvironment
}

// newTransport 构造共享的出站 Transport。
//
// ⚠️ **真正禁 h2 的唯一正确写法是置空的 TLSNextProto 映射**：
//
//	TLSNextProto: make(map[string]func(string, *tls.Conn) http.RoundTripper)
//
// 为什么不是 `ForceAttemptHTTP2: false`：那个开关**只对自定义 Dial 生效**，
// 默认 TLS 栈仍会经 ALPN 协商出 h2。于是半死的 h2 流被复用，表现为
// `http2: timeout awaiting response headers` —— 排查时完全看不出是 h2 的问题。
// 置空映射后 ALPN 协商完成也没有 h2 处理器可用，连接退回 HTTP/1.1。
//
// 必须 `make` 而不是留 nil：nil 的含义是"让标准库注入默认 h2 映射"，
// 恰好是反效果。
func newTransport() *http.Transport {
	return newTransportWithProxy("")
}

// newTransportWithProxy 构造绑定了指定代理的 Transport。
func newTransportWithProxy(proxyURL string) *http.Transport {
	dialer := newDialer()
	return &http.Transport{
		Proxy:       proxyFuncFor(proxyURL),
		DialContext: dialer.DialContext,
		// 空映射（非 nil）真正禁 h2 —— 见函数注释
		TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		IdleConnTimeout:       idleConnTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}
}

// closeIdler 是支持清空空闲连接池的 RoundTripper。
type closeIdler interface{ CloseIdleConnections() }

// closeIdleOnFailure 在**传输层失败**后清掉空闲连接池。
//
// 为什么需要：失败的连接可能仍留在空闲池里（还没到 IdleConnTimeout），
// 下一个请求会继续捡到它 —— 于是"一次网络抖动"会变成"连续几次失败"。
// 清掉池子后下个请求会重新握手。
//
// 代价：下个请求多一次 TCP+TLS 握手（毫秒级），与"复用半死连接卡 60s"
// 完全不成比例。
//
// 挂载点：**传输层出口**，而不是错误分类处。因为传输层失败根本没有 body
// 可分类（错误分类看的是业务信封），这类失败的正确处置就是连接层清理。
// 关闭是 best-effort：rt 为 nil 或未实现该接口时静默跳过。
func closeIdleOnFailure(rt http.RoundTripper) {
	if rt == nil {
		return
	}
	if ci, ok := rt.(closeIdler); ok {
		ci.CloseIdleConnections()
	}
}

// newHTTPClient 构造带加固 Transport 的客户端。
//
// timeout <= 0 表示不设整体超时（长流场景由流中空闲监控负责，
// 见 idle.go —— 用一个整体超时去管长流会误杀正常的长回复）。
func newHTTPClient(timeout time.Duration) *http.Client {
	return newHTTPClientWithProxy(timeout, "")
}

// newHTTPClientWithProxy 构造带指定代理与加固 Transport 的客户端。
func newHTTPClientWithProxy(timeout time.Duration, proxyURL string) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: newTransportWithProxy(proxyURL),
	}
}
