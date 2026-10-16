package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"trae2api/internal/traectl"
)

// 本文件提供额度与签到相关的 HTTP 端点。
//
// 鉴权：全部走 withAuth（与 /v1/* 同一套 API Key）。管理类端点也复用同一个 key ——
// 本项目目前是单租户，等接入用户表后再按角色细分（届时 admin 端点要求 RoleAdmin）。

// handleCredits GET /v1/credits —— 实时额度明细（只读）。
//
// 注意：每次调用都会打上游（每账号 2 次请求），不建议高频轮询。
// 低开销的展示请用 /status 里的 accounts[].credits（后台 30 分钟刷新一次的缓存值）。
func (s *Server) handleCredits(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()
	items := s.CreditsSnapshot(ctx)
	totalRemain := 0.0
	for _, it := range items {
		totalRemain += it.Remaining
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":          "list",
		"data":            items,
		"total_remaining": totalRemain,
		"count":           len(items),
	})
}

// handleCheckin POST /admin/checkin —— 立即执行一次全账号签到（幂等）。
func (s *Server) handleCheckin(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	ctx, cancel := contextWithTimeout(r, 120*time.Second)
	defer cancel()
	results := s.CheckinAll(ctx)
	checked, already, failed, disabled := 0, 0, 0, 0
	for _, it := range results {
		switch it.Status {
		case "checked":
			checked++
		case "already":
			already++
		case "failed":
			failed++
		case "disabled":
			disabled++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results":  results,
		"checked":  checked,
		"already":  already,
		"failed":   failed,
		"disabled": disabled,
	})
}

// handleRefreshCredits POST /admin/credits/refresh —— 手动刷新额度缓存。
func (s *Server) handleRefreshCredits(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()
	n := s.RefreshCredits(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"refreshed": n, "accounts": len(s.pool.List())})
}

// handleStats GET /v1/stats —— 指标聚合快照。
//
// 返回结构与 workbuddy2api 对齐（global / by_model / by_account / by_realm / by_day），
// 但多了两个**可信度**字段：
//   - usage_count：有 usage 观测的请求数。它与 requests 的差额就是"缺观测"的量。
//     前端应据此判断均值可信度，而不是把 -1 哨兵当 0 显示。
//   - no_observation_sentinel：哨兵值（-1），供前端识别"此项无观测"。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if s.metrics == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "metrics_disabled",
			"指标采集已关闭（config metrics.enabled=false）")
		return
	}
	snap := s.metrics.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"service": ServiceName,
		"version": Version,
		"stats":   snap,
		// 池内实时状态一并给出，前端就不必再调一次 /status
		"pool": map[string]any{
			"total": len(s.pool.List()),
			// servable 是"有没有可调度的账号"，servable_count 是"有几个"。
			// 两个都给：前者适合做告警判断，后者适合做展示。
			"servable":       s.pool.Servable(""),
			"servable_count": s.pool.ServableCount(""),
		},
		"metrics_last_save": s.metrics.LastSave().Format(time.RFC3339),
	})
}

// handleStatsReset POST /v1/stats/reset —— 清零指标（需管理员）。
func (s *Server) handleStatsReset(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if s.metrics == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "metrics_disabled", "指标采集已关闭")
		return
	}
	s.metrics.Reset()
	if err := s.metrics.Save(); err != nil {
		s.logf("WARN: 指标清零后落盘失败: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"reset": true})
}

// handleWAFReset POST /admin/waf/reset —— 手动解除 IP 级闸。
//
// 使用场景：确认上游限流已过（或换了出口 IP），想立刻恢复轮转而不等窗口自然过期。
func (s *Server) handleWAFReset(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if s.wafIP == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "unavailable", "IP 级闸未启用")
		return
	}
	before := s.wafIP.Status()
	s.wafIP.Reset()
	s.logf("INFO: 已手动解除 WAF IP 级闸（此前: %v）", before["last_reason"])
	writeJSON(w, http.StatusOK, map[string]any{
		"reset": true, "before": before, "after": s.wafIP.Status(),
	})
}

// handleDegradeReset POST /admin/degrade/reset —— 手动解除提示词降级。
//
// 使用场景：用户改了 system 提示词、或确认上游已不再拦截，想立刻恢复正常提示词
// 而不等次日零点。
func (s *Server) handleDegradeReset(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if s.degrade == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "unavailable", "降级闸未启用")
		return
	}
	before := s.degrade.Status()
	s.degrade.Reset()
	s.logf("INFO: 已手动解除提示词降级（此前: %v）", before["until"])
	writeJSON(w, http.StatusOK, map[string]any{
		"reset": true, "before": before, "after": s.degrade.Status(),
	})
}

// handleClearCooldown POST /admin/accounts/{uid}/clear-cooldown —— 解除账号冷却。
func (s *Server) handleClearCooldown(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	uid := r.PathValue("uid")
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少 uid")
		return
	}
	if s.pool.Get(uid) == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "账号不存在: "+uid)
		return
	}
	changed := s.pool.ClearCooldown(uid)
	_ = s.pool.Save()
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "changed": changed})
}

// handleSetDisabled POST /admin/accounts/{uid}/disable|enable —— 启停账号。
func (s *Server) handleSetDisabled(disabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		uid := r.PathValue("uid")
		if uid == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少 uid")
			return
		}
		if s.pool.Get(uid) == nil {
			writeOpenAIError(w, http.StatusNotFound, "not_found", "账号不存在: "+uid)
			return
		}
		reason := ""
		if disabled {
			reason = "控制台手动禁用"
		}
		s.pool.SetDisabled(uid, disabled, reason)
		_ = s.pool.Save()
		writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "disabled": disabled})
	}
}

// handleAccountTest POST /admin/accounts/{uid}/test —— 测定单账号连通性与实时额度/延迟。
func (s *Server) handleAccountTest(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	uid := r.PathValue("uid")
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少账号 uid")
		return
	}
	a := s.pool.Get(uid)
	if a == nil {
		writeOpenAIError(w, http.StatusNotFound, "account_not_found", "账号不存在: "+uid)
		return
	}
	if a.Store == nil {
		writeOpenAIError(w, http.StatusBadRequest, "no_store", "账号未绑定凭据存储")
		return
	}

	start := time.Now()
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	remain, good := s.refreshAccountCredits(ctx, a)
	latency := time.Since(start).Milliseconds()

	if !good {
		if err := a.Store.Refresh(ctx); err == nil {
			remain, good = s.refreshAccountCredits(ctx, a)
			latency = time.Since(start).Milliseconds()
		}
	}

	if !good {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         false,
			"uid":        uid,
			"latency_ms": latency,
			"error":      "上游额度与连通性验证失败",
		})
		return
	}

	s.pool.NoteSuccess(uid)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"uid":        uid,
		"label":      a.Label,
		"latency_ms": latency,
		"credits":    remain,
		"message":    "连通正常，额度已同步",
	})
}

// handleAccountProxy POST /admin/accounts/{uid}/proxy —— 动态设置账号代理。
func (s *Server) handleAccountProxy(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	uid := r.PathValue("uid")
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少账号 uid")
		return
	}
	a := s.pool.Get(uid)
	if a == nil {
		writeOpenAIError(w, http.StatusNotFound, "account_not_found", "账号不存在: "+uid)
		return
	}
	var req struct {
		Proxy string `json:"proxy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", "请求体不是合法 JSON")
		return
	}
	proxyURL := strings.TrimSpace(req.Proxy)
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") || u.Host == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_proxy", "代理 URL 非法（仅支持 http/https/socks5，需含主机名）")
			return
		}
	}
	s.pool.SetProxy(uid, proxyURL)
	if a.Store != nil && a.Store.Mode() == traectl.ModeSnapshot {
		_ = a.Store.Save()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"uid":     uid,
		"proxy":   proxyURL,
		"message": "账号代理已更新",
	})
}

