package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"trae2api/internal/traectl"
)

// handleAccountInviteCode GET /admin/accounts/{uid}/invite-code —— 查询单个账号的邀请码
func (s *Server) handleAccountInviteCode(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	uid := r.PathValue("uid")
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少账号 uid")
		return
	}
	a := s.pool.Get(uid)
	if a == nil || a.Store == nil {
		writeOpenAIError(w, http.StatusNotFound, "account_not_found", "账号不存在: "+uid)
		return
	}

	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	info, err := a.Store.GetInviteCode(ctx)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invite_code_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"uid":            uid,
		"label":          a.Label,
		"invite_code":    info.InviteCode,
		"invite_url":     info.InviteURL,
		"reward_credits": info.RewardCredits,
		"message":        info.Message,
	})
}

// handleAccountRedeemInvite POST /admin/accounts/{uid}/redeem-invite —— 为账号兑换邀请码
func (s *Server) handleAccountRedeemInvite(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	uid := r.PathValue("uid")
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少账号 uid")
		return
	}
	a := s.pool.Get(uid)
	if a == nil || a.Store == nil {
		writeOpenAIError(w, http.StatusNotFound, "account_not_found", "账号不存在: "+uid)
		return
	}

	var req struct {
		InviteCode string `json:"invite_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", "请求体不是合法 JSON")
		return
	}
	code := strings.TrimSpace(req.InviteCode)
	if code == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_code", "邀请码不能为空")
		return
	}

	ctx, cancel := contextWithTimeout(r, 20*time.Second)
	defer cancel()

	res, err := a.Store.RedeemInviteCode(ctx, code)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "redeem_error", err.Error())
		return
	}

	// 兑换成功后顺手刷新一下额度
	if res.Success {
		s.refreshAccountCredits(ctx, a)
		_ = s.pool.Save()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      res.Success,
		"uid":     uid,
		"credits": res.Credits,
		"message": res.Message,
	})
}

// handleAccountClaimNewbie POST /admin/accounts/{uid}/claim-newbie —— 为单个账号领取新手/活动福利
func (s *Server) handleAccountClaimNewbie(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	uid := r.PathValue("uid")
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少账号 uid")
		return
	}
	a := s.pool.Get(uid)
	if a == nil || a.Store == nil {
		writeOpenAIError(w, http.StatusNotFound, "account_not_found", "账号不存在: "+uid)
		return
	}

	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()

	results, err := a.Store.ClaimNewbieBonus(ctx)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "claim_error", err.Error())
		return
	}

	s.refreshAccountCredits(ctx, a)
	_ = s.pool.Save()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"uid":     uid,
		"results": results,
	})
}

// handleAccountsClaimAllNewbie POST /admin/accounts/claim-all-newbie —— 为全部国内账号批量领取新手福利
func (s *Server) handleAccountsClaimAllNewbie(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	ctx, cancel := contextWithTimeout(r, 120*time.Second)
	defer cancel()

	type accountNewbieResult struct {
		UID     string                        `json:"uid"`
		Label   string                        `json:"label"`
		Results []traectl.ActivityBonusResult `json:"results"`
		Error   string                        `json:"error,omitempty"`
	}

	out := make([]accountNewbieResult, 0)
	for _, a := range s.pool.All() {
		if a.Store == nil || a.Store.SkipCheckin() {
			continue
		}
		res, err := a.Store.ClaimNewbieBonus(ctx)
		item := accountNewbieResult{
			UID:   a.UID,
			Label: a.Label,
		}
		if err != nil {
			item.Error = err.Error()
		} else {
			item.Results = res
		}
		out = append(out, item)
		s.refreshAccountCredits(ctx, a)
	}
	_ = s.pool.Save()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"results": out,
		"count":   len(out),
	})
}
