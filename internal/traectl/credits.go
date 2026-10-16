package traectl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 本文件实现「用户成长 / 商业」类接口：签到、额度用量、付费状态。
//
// 这些接口与 chat 走**不同的鉴权头**：chat 用 Authorization + X-Cloudide-Token
// 且带全套指纹头；UG 接口只需 `Authorization: Cloud-IDE-JWT <token>` +
// `x-device-id: <数字设备ID>`（注意是**数字 ID**，不是 chat 用的 sha256 派生值）。
// 混用会 401。
//
// 全部端点在真实账号上实测可用（见 README「协议要点」）。
const (
	// pathCheckinStatus 签到状态查询。
	pathCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	// pathCheckinClaim 签到领取（固定 100 积分）。
	pathCheckinClaim = "/trae/api/v2/ug/checkin_credits/claim"
	// pathEntUsage 额度用量（余额、消耗比例、权益包明细）。
	pathEntUsage = "/trae/api/v2/pay/ide_user_ent_usage"
	// pathPayStatus 付费状态与可参与活动（CN 版）。
	pathPayStatus = "/trae/api/v2/pay/ide_user_pay_status"
	// pathPayStatusV1 付费状态（国际版，实测可用）。
	pathPayStatusV1 = "/trae/api/v1/pay/ide_user_pay_status"
)

// CheckinStatus 是签到状态。
type CheckinStatus struct {
	// CheckedIn 今日是否已签到。
	CheckedIn bool `json:"checked_in"`
	// DIDCheckedIn 设备维度是否已签到（多设备场景下与 CheckedIn 可能不同）。
	DIDCheckedIn bool `json:"did_checked_in"`
	// Enable 签到功能是否对该账号开放。
	Enable bool `json:"enable"`
	// Credits 本次签到可得积分。
	Credits int64 `json:"credits"`
	// ExtraCredits 额外奖励积分。
	ExtraCredits int64 `json:"extra_credits"`
	// Code 业务码，0 = 成功。
	Code int `json:"code"`
	// Message 业务消息。
	Message string `json:"message"`
}

// UsageSummary 是用量汇总。
type UsageSummary struct {
	// ConsumedAmount 已消耗积分。
	ConsumedAmount float64 `json:"consumed_amount"`
	// ConsumptionRatio 消耗比例（0~1）。
	ConsumptionRatio float64 `json:"consumption_ratio"`
	// TotalAmount 总额度。
	TotalAmount float64 `json:"total_amount"`
}

// Remaining 返回剩余积分（负数按 0 处理）。
func (u UsageSummary) Remaining() float64 {
	r := u.TotalAmount - u.ConsumedAmount
	if r < 0 {
		return 0
	}
	return r
}

// EntitlementPack 是一个权益包。
type EntitlementPack struct {
	DisplayDesc string `json:"display_desc"`
	BaseInfo    struct {
		EntitlementID string  `json:"entitlement_id"`
		EndTime       int64   `json:"end_time"`
		ChargeAmount  float64 `json:"charge_amount"`
		EntStatus     int     `json:"ent_status"`
	} `json:"entitlement_base_info"`
}

// EntUsage 是额度用量响应。
type EntUsage struct {
	IsCreditsBilling bool         `json:"is_credits_billing"`
	IsPayFreshman    bool         `json:"is_pay_freshman"`
	UsageSummary     UsageSummary `json:"usage_summary"`
	// Packs 权益包列表（原字段名 user_entitlement_pack_list）。
	Packs []EntitlementPack `json:"user_entitlement_pack_list"`
}

// SumExpiringCredits 汇总**即将过期**的额度量（`within` 内到期的权益包金额之和）。
//
// 用途：选号时优先消耗快过期的额度，避免白白作废（参考实现的三因子之一）。
//
// 实现说明与取舍：
//   - 上游的权益包里只有 `charge_amount`（该包的**面额**）与 `end_time`，
//     **没有"该包剩余多少"**。所以这里用的是"面额之和"而不是"剩余之和" ——
//     它是一个**上界**，用来表达"这个账号有多少额度正面临作废风险"。
//   - 对选号权重而言，上界已经够用：我们要的是账号之间的**相对**紧迫度，
//     不是精确的作废金额。
//   - `end_time` 为 0 或负值表示"无到期时间"（永久有效），不计入。
func (u *EntUsage) SumExpiringCredits(within time.Duration, now time.Time) int64 {
	if u == nil || within <= 0 {
		return 0
	}
	deadline := now.Add(within).Unix()
	var sum int64
	for _, p := range u.Packs {
		end := p.BaseInfo.EndTime
		if end <= 0 {
			continue // 无到期时间 = 永久，不构成作废风险
		}
		if end <= deadline {
			sum += int64(p.BaseInfo.ChargeAmount)
		}
	}
	return sum
}

// DefaultExpiringWindow 是"即将过期"的判定窗口。
//
// 取 7 天：太短（如 1 天）会让绝大多数账号都算出 0（因子失效）；
// 太长（如 30 天）会让"快过期"失去区分度（几乎所有账号都命中）。
const DefaultExpiringWindow = 7 * 24 * time.Hour

// CheckinResult 是签到领取结果。
type CheckinResult struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// UGError 是成长/商业类接口的业务错误。
type UGError struct {
	HTTPStatus int
	Code       int
	Message    string
	Body       string
	Path       string
}

func (e *UGError) Error() string {
	return fmt.Sprintf("traectl: %s HTTP %d code=%d: %s", e.Path, e.HTTPStatus, e.Code, e.Message)
}

// postUG 向成长/商业类端点发一次 POST（body 固定为 {}）。
//
// 鉴权头与 chat 不同：这里只需 Cloud-IDE-JWT + 数字设备 ID（见文件头注释）。
func (s *Store) postUG(ctx context.Context, path string, out any) error {
	s.mu.Lock()
	cred := s.Cred
	deviceID := s.DeviceID
	s.mu.Unlock()

	host := strings.TrimRight(cred.Host, "/")
	if host == "" {
		host = DefaultUGHost
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+path, bytes.NewReader([]byte("{}")))
	if err != nil {
		return fmt.Errorf("traectl: 构造 %s 请求: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+cred.Token)
	// 注意：UG 接口的 x-device-id 是**数字设备 ID**，不是 chat 用的 sha256 派生值。
	req.Header.Set("x-device-id", deviceID)
	// 与 chat 保持一致的 UA：上游按 UA 做机器人识别，Go 默认 UA 会被拦。
	req.Header.Set("User-Agent", defaultUserAgent)

	cl := s.httpClient()
	resp, err := cl.Do(req)
	if err != nil {
		closeIdleOnFailure(cl.Transport)
		return fmt.Errorf("traectl: %s 请求失败: %w", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &UGError{HTTPStatus: resp.StatusCode, Body: string(body), Path: path, Message: "非 2xx"}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &UGError{HTTPStatus: resp.StatusCode, Body: string(body), Path: path,
			Message: "响应不是合法 JSON"}
	}
	return nil
}

// isIntl 判断当前 Store 是否属于国际版。
func (s *Store) isIntl() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Product.Edition == EditionIntl || IsIntlHost(s.Cred.Host)
}

// CheckinStatus 查询签到状态。
// 国际版体系不支持签到（客户端直接判断 `if (providerCode !== Vn.CN) throw Checkin unavailable`），
// 国际版账号直接返回 Enable: false, CheckedIn: true，避免发请求产生 404 告警。
func (s *Store) CheckinStatus(ctx context.Context) (*CheckinStatus, error) {
	if s.isIntl() {
		return &CheckinStatus{
			Enable:    false,
			CheckedIn: true,
			Code:      0,
			Message:   "国际版无签到体系",
		}, nil
	}
	var st CheckinStatus
	if err := s.postUG(ctx, pathCheckinStatus, &st); err != nil {
		return nil, err
	}
	if st.Code != 0 {
		return &st, &UGError{HTTPStatus: 200, Code: st.Code, Message: st.Message, Path: pathCheckinStatus}
	}
	return &st, nil
}

// CheckinClaim 执行签到。
//
// 幂等性：上游对重复签到返回业务码非 0（而非 500），调用方应把"已签到"
// 视为成功而不是错误 —— 否则定时任务会在已签到的日子里刷满告警。
func (s *Store) CheckinClaim(ctx context.Context) (*CheckinResult, error) {
	if s.isIntl() {
		return &CheckinResult{Code: 0, Message: "国际版无签到体系"}, nil
	}
	var r CheckinResult
	if err := s.postUG(ctx, pathCheckinClaim, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// EntUsage 查询额度用量。
//
// 国际版没有点数计费（Credits Billing）及 ide_user_ent_usage 接口（请求必定 404）。
// 国际版账号通过调用 ide_user_pay_status(v1) 验证上游健康度，并合成无限额度结构（999999 / 0 消耗），
// 保证控制台与测速能正确展示且选号权重不为 0。
func (s *Store) EntUsage(ctx context.Context) (*EntUsage, error) {
	if s.isIntl() {
		pay, err := s.PayStatus(ctx)
		if err != nil {
			return nil, err
		}
		// 根据 pay_status detail 中的 fast_request_per 提取快速请求数（若有）
		total := 999999.0
		if detail, ok := pay["detail"].(map[string]any); ok {
			if fastReq, ok := detail["fast_request_per"].(float64); ok && fastReq > 0 {
				// 有专属配额标识，显示合理数值
				total = fastReq * 1000
			}
		}
		return &EntUsage{
			IsCreditsBilling: false,
			UsageSummary: UsageSummary{
				TotalAmount:      total,
				ConsumedAmount:   0,
				ConsumptionRatio: 0,
			},
		}, nil
	}
	var u EntUsage
	if err := s.postUG(ctx, pathEntUsage, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// PayStatus 查询付费状态（原始 JSON，字段随服务端演进，不做强类型建模）。
// CN 版走 v2，国际版走 v1（实测对齐客户端 out/main.js 实现）。
func (s *Store) PayStatus(ctx context.Context) (map[string]any, error) {
	path := pathPayStatus
	if s.isIntl() {
		path = pathPayStatusV1
	}
	var m map[string]any
	if err := s.postUG(ctx, path, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// CheckinAndReport 查询状态并按需签到，返回签到后的最新状态。
//
// 返回 (didCheckin, status, err)：didCheckin 表示本次是否真的执行了领取。
// 已签到时不报错 —— 这是定时任务每天都会走到的正常路径。
func (s *Store) CheckinAndReport(ctx context.Context) (bool, *CheckinStatus, error) {
	before, err := s.CheckinStatus(ctx)
	if err != nil {
		return false, nil, err
	}
	if before.CheckedIn {
		return false, before, nil
	}
	if !before.Enable {
		return false, before, nil // 该账号未开放签到
	}
	if _, err := s.CheckinClaim(ctx); err != nil {
		return false, before, err
	}
	after, err := s.CheckinStatus(ctx)
	if err != nil {
		// 领取已成功，只是复核失败：仍算签到成功，但把状态回落到 before
		return true, before, nil
	}
	return true, after, nil
}

// creditsTimeout 是成长/商业类接口的单次超时。
const creditsTimeout = 30 * time.Second

// WithCreditsTimeout 返回带超时的 ctx（供调用方统一超时口径）。
func WithCreditsTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, creditsTimeout)
}
