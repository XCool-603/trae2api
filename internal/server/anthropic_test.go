package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"trae2api/internal/config"
	"trae2api/internal/pool"
	"trae2api/internal/traectl"
)

func TestAnthropicVisionRejection(t *testing.T) {
	s := &Server{
		cfg:  &config.Config{},
		pool: pool.New(pool.Options{}),
	}
	h := s.Handler()

	body := `{
		"model": "claude-3-5-sonnet",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "What is in this image?"},
					{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}}
				]
			}
		]
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for inline image, got %d, body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "当前上游不接受内联图片") {
		t.Errorf("expected vision rejection error, got: %s", w.Body.String())
	}
}

func TestAnthropicStreamingWithReasoning(t *testing.T) {
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		// 1. 发送思考片段
		_, _ = w.Write([]byte("event: output\ndata: {\"response\":\"\",\"reasoning_content\":\"thinking step 1\"}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		// 2. 发送文本片段
		_, _ = w.Write([]byte("event: output\ndata: {\"response\":\"Hello world\",\"reasoning_content\":\"\"}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		// 3. 发送完成
		_, _ = w.Write([]byte("event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer fakeUpstream.Close()

	p := pool.New(pool.Options{})
	st := &traectl.Store{
		Product: traectl.Product{Key: "trae-cn", Edition: traectl.EditionCN},
		Cred: traectl.Credentials{
			Token:     "test-token",
			UserID:    "123456",
			Host:      fakeUpstream.URL,
			ExpiredAt: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		},
		DeviceID: "1234567890123456",
	}
	acct := &pool.Account{
		UID:   "trae-cn-test",
		Realm: "trae",
		Label: "测试账号",
		Store: st,
	}
	p.Add(acct)

	cfg := &config.Config{}

	s := &Server{
		cfg:       cfg,
		pool:      p,
		maxRotate: 1,
		channels: map[string]*Channel{
			"trae": {
				Realm:   "trae",
				Product: traectl.Product{Key: "trae-cn", Edition: traectl.EditionCN},
				Catalog: &traectl.Catalog{
					Models: []traectl.Model{
						{ID: "claude-3-7-sonnet", ContextWindow: 200000},
					},
				},
				client: &traectl.Client{Base: fakeUpstream.URL},
				accounts: map[string]*pool.Account{
					acct.UID: acct,
				},
			},
		},
	}
	h := s.Handler()

	body := `{
		"model": "claude-3-7-sonnet",
		"stream": true,
		"messages": [
			{"role": "user", "content": "hi"}
		]
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	respStr := w.Body.String()

	// 验证包含 message_start, content_block_start (thinking), content_block_delta (thinking_delta), content_block_start (text), message_stop
	if !strings.Contains(respStr, "thinking_delta") {
		t.Errorf("expected thinking_delta in stream, got:\n%s", respStr)
	}
	if !strings.Contains(respStr, "Hello world") {
		t.Errorf("expected text content in stream, got:\n%s", respStr)
	}
	if !strings.Contains(respStr, "message_stop") {
		t.Errorf("expected message_stop in stream, got:\n%s", respStr)
	}
}

func TestAnthropicStreamMidflightError(t *testing.T) {
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		// 1. 发送一部分正常消息
		_, _ = w.Write([]byte("event: output\ndata: {\"response\":\"Initial chunk\",\"reasoning_content\":\"\"}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		// 2. 发送上游错误 frame
		_, _ = w.Write([]byte("event: error\ndata: {\"code\": 4011, \"message\": \"rate limit exceeded\"}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer fakeUpstream.Close()

	p := pool.New(pool.Options{})
	st := &traectl.Store{
		Product: traectl.Product{Key: "trae-cn", Edition: traectl.EditionCN},
		Cred: traectl.Credentials{
			Token:     "test-token",
			UserID:    "123456",
			Host:      fakeUpstream.URL,
			ExpiredAt: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		},
		DeviceID: "1234567890123456",
	}
	acct := &pool.Account{
		UID:   "trae-cn-test",
		Realm: "trae",
		Label: "测试账号",
		Store: st,
	}
	p.Add(acct)

	cfg := &config.Config{}

	s := &Server{
		cfg:       cfg,
		pool:      p,
		maxRotate: 1,
		channels: map[string]*Channel{
			"trae": {
				Realm:   "trae",
				Product: traectl.Product{Key: "trae-cn", Edition: traectl.EditionCN},
				Catalog: &traectl.Catalog{
					Models: []traectl.Model{
						{ID: "claude-3-5-sonnet", ContextWindow: 200000},
					},
				},
				client: &traectl.Client{Base: fakeUpstream.URL},
				accounts: map[string]*pool.Account{
					acct.UID: acct,
				},
			},
		},
	}
	h := s.Handler()

	body := `{
		"model": "claude-3-5-sonnet",
		"stream": true,
		"messages": [
			{"role": "user", "content": "hi"}
		]
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	respStr := w.Body.String()
	// 验证在流断开前发出了 event: error
	if !strings.Contains(respStr, "event: error") {
		t.Errorf("expected event: error in stream on midflight failure, got:\n%s", respStr)
	}
}
