package traectl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestProxyConfiguration 验证全局与自定义代理 URL 解析。
func TestProxyConfiguration(t *testing.T) {
	oldGlobal := GlobalProxy()
	defer SetGlobalProxy(oldGlobal)

	SetGlobalProxy("http://127.0.0.1:8888")
	if got := GlobalProxy(); got != "http://127.0.0.1:8888" {
		t.Fatalf("GlobalProxy() = %q, want http://127.0.0.1:8888", got)
	}

	fn := proxyFuncFor("")
	req, _ := http.NewRequest(http.MethodGet, "https://api.trae.cn/test", nil)
	u, err := fn(req)
	if err != nil {
		t.Fatalf("proxyFuncFor err: %v", err)
	}
	if u == nil || u.String() != "http://127.0.0.1:8888" {
		t.Errorf("got proxy %v, want http://127.0.0.1:8888", u)
	}

	// 账号单独指定代理优先于全局
	fnSpecific := proxyFuncFor("socks5://127.0.0.1:1080")
	uSpecific, err := fnSpecific(req)
	if err != nil {
		t.Fatalf("proxyFuncFor specific err: %v", err)
	}
	if uSpecific == nil || uSpecific.String() != "socks5://127.0.0.1:1080" {
		t.Errorf("got proxy %v, want socks5://127.0.0.1:1080", uSpecific)
	}
}

// TestPerAccountProxyRouting 验证 Store 与 Client 实际走指定的代理服务器。
func TestPerAccountProxyRouting(t *testing.T) {
	var proxyHit atomic.Int32

	// 起一个模拟 HTTP 代理服务器
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHit.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"data":{"credits":888}}`))
	}))
	defer proxyServer.Close()

	// 构造绑定该代理的 Store
	s := &Store{
		Product: Product{Key: "trae-cn", Label: "Trae CN"},
		Cred: Credentials{
			Token:  "test-token",
			UserID: "12345",
			Host:   proxyServer.URL,
		},
		proxy: proxyServer.URL,
	}

	if s.Proxy() != proxyServer.URL {
		t.Fatalf("s.Proxy() = %q want %q", s.Proxy(), proxyServer.URL)
	}

	// 额度查询应通过代理发送并命中
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, proxyServer.URL+"/test", nil)
	cl := s.httpClient()
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("cl.Do: %v", err)
	}
	_ = resp.Body.Close()

	if proxyHit.Load() == 0 {
		t.Error("请求未命中指定代理")
	}

	// 验证同代理 Client 复用
	cl2 := proxyChatClient(proxyServer.URL)
	cl3 := proxyChatClient(proxyServer.URL)
	if cl2 != cl3 {
		t.Error("相同代理的客户端应该复用连接池")
	}
}
