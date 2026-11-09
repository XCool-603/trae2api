package traectl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetInviteCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathGetInviteCode {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"invite_code":"TESTCODE123","invite_url":"https://www.trae.cn/?code=TESTCODE123","reward_credits":200}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	st := testUGStore(srv.URL)
	info, err := st.GetInviteCode(context.Background())
	if err != nil {
		t.Fatalf("GetInviteCode failed: %v", err)
	}
	if info.InviteCode != "TESTCODE123" {
		t.Errorf("InviteCode = %q, want TESTCODE123", info.InviteCode)
	}
	if info.RewardCredits != 200 {
		t.Errorf("RewardCredits = %d, want 200", info.RewardCredits)
	}
}

func TestRedeemInviteCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathRedeemInviteCode {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"credits":200}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	st := testUGStore(srv.URL)
	res, err := st.RedeemInviteCode(context.Background(), "ABCDEF")
	if err != nil {
		t.Fatalf("RedeemInviteCode error: %v", err)
	}
	if !res.Success {
		t.Errorf("res.Success = false, message = %s", res.Message)
	}
	if res.Credits != 200 {
		t.Errorf("res.Credits = %d, want 200", res.Credits)
	}
}

func TestClaimNewbieBonus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathActivityAction {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	st := testUGStore(srv.URL)
	results, err := st.ClaimNewbieBonus(context.Background())
	if err != nil {
		t.Fatalf("ClaimNewbieBonus error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	for _, r := range results {
		if !r.Success {
			t.Errorf("activity %d failed: %s", r.ActivityID, r.Message)
		}
	}
}
