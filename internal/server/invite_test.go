package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"trae2api/internal/config"
	"trae2api/internal/pool"
	"trae2api/internal/traectl"
)

func TestInviteRoutes(t *testing.T) {
	p := pool.New(pool.Options{})
	fakeUG := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case "/trae/api/v1/pay/get_invite_code":
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"invite_code":"MYCODE888","invite_url":"https://trae.cn/?code=MYCODE888"}}`))
		case "/trae/api/v1/pay/redeem_invite_code":
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"credits":200}}`))
		case "/trae/api/v2/ug/activity/action":
			_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
		default:
			_, _ = w.Write([]byte(`{"code":0}`))
		}
	}))
	defer fakeUG.Close()

	st := &traectl.Store{
		Product: traectl.Product{Key: "trae-cn", Edition: traectl.EditionCN},
		Cred: traectl.Credentials{
			Token:  "test-token",
			UserID: "123456",
			Host:   fakeUG.URL,
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

	s := &Server{
		cfg:  &config.Config{},
		pool: p,
	}
	h := s.Handler()

	// 1. 测试 GET /admin/accounts/{uid}/invite-code
	req1 := httptest.NewRequest(http.MethodGet, "/admin/accounts/trae-cn-test/invite-code", nil)
	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("GET invite-code failed: %d, body: %s", w1.Code, w1.Body.String())
	}
	if !strings.Contains(w1.Body.String(), "MYCODE888") {
		t.Errorf("Response should contain MYCODE888, got: %s", w1.Body.String())
	}

	// 2. 测试 POST /admin/accounts/{uid}/redeem-invite
	req2 := httptest.NewRequest(http.MethodPost, "/admin/accounts/trae-cn-test/redeem-invite", strings.NewReader(`{"invite_code":"FRIEND123"}`))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("POST redeem-invite failed: %d, body: %s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), "兑换成功") {
		t.Errorf("Response should indicate success, got: %s", w2.Body.String())
	}

	// 3. 测试 POST /admin/accounts/{uid}/claim-newbie
	req3 := httptest.NewRequest(http.MethodPost, "/admin/accounts/trae-cn-test/claim-newbie", nil)
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("POST claim-newbie failed: %d, body: %s", w3.Code, w3.Body.String())
	}
	if !strings.Contains(w3.Body.String(), "新用户积分") {
		t.Errorf("Response should contain newbie activity info, got: %s", w3.Body.String())
	}

	_ = context.Background()
}
