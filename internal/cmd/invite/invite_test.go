package invite

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInviteCommand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/status":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"accounts": []map[string]any{
					{"uid": "trae-1", "label": "Account 1", "realm": "china", "disabled": false},
				},
			})
		case r.URL.Path == "/admin/accounts/trae-1/invite-code":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":             true,
				"uid":            "trae-1",
				"label":          "Account 1",
				"invite_code":    "INVITE123",
				"invite_url":     "https://trae.ai/invite?code=INVITE123",
				"reward_credits": 1000,
			})
		case r.URL.Path == "/admin/accounts/trae-1/redeem-invite":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"uid":     "trae-1",
				"credits": 500,
				"message": "兑换成功",
			})
		case r.URL.Path == "/admin/accounts/claim-all-newbie":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"uid":   "trae-1",
					"label": "Account 1",
					"results": []map[string]any{
						{"activity_id": 102, "name": "新用户福利", "success": true, "credits": 500, "message": "领取成功"},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// 1. 测试查询邀请码
	if code := Run([]string{"-url", srv.URL, "-json"}); code != 0 {
		t.Fatalf("Run invite list failed with exit code: %d", code)
	}

	// 2. 测试兑换邀请码
	if code := Run([]string{"-url", srv.URL, "-redeem", "INVITE123", "-json"}); code != 0 {
		t.Fatalf("Run invite redeem failed with exit code: %d", code)
	}

	// 3. 测试领取新人福利
	if code := Run([]string{"-url", srv.URL, "-newbie", "-json"}); code != 0 {
		t.Fatalf("Run invite newbie failed with exit code: %d", code)
	}
}
