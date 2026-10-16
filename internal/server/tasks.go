package server

import (
	"context"
	"log"
	"time"

	"trae2api/internal/pool"
	"trae2api/internal/traectl"
)

// 本文件实现后台任务：额度刷新与每日自动签到。
//
// 设计取舍：
//   - 额度刷新是**低频只读**操作（默认 30 分钟），用于给选号权重提供余额因子，
//     以及让控制台能看到余量。刷新失败不惩罚账号（只读接口失败多半是网络抖动，
//     不该让一个健康的账号因为查不到余额而被冷却）。
//   - 签到是**按账号幂等**的：已签到直接跳过，不把"今天已签"当失败。
//     签到失败按账号记一次失败（走熔断计数），但不影响其他账号。

// CheckinOutcome 是单个账号的签到结果。
type CheckinOutcome struct {
	UID     string `json:"uid"`
	Label   string `json:"label"`
	Status  string `json:"status"` // checked / already / disabled / failed
	Detail  string `json:"detail,omitempty"`
	Credits int64  `json:"credits,omitempty"`
}

// CheckinAll 对全部账号执行签到（幂等：已签到跳过）。
//
// 返回每个账号的结果，便于控制台/接口展示"谁签到了、谁失败了、为什么"。
func (s *Server) CheckinAll(ctx context.Context) []CheckinOutcome {
	out := make([]CheckinOutcome, 0)
	for _, ch := range s.channels {
		for _, a := range ch.Accounts() {
			oc := CheckinOutcome{UID: a.UID, Label: a.Label}
			st := a.Status(time.Now())
			if st.Disabled {
				oc.Status = "disabled"
				out = append(out, oc)
				continue
			}
			cctx, cancel := traectl.WithCreditsTimeout(ctx)
			did, cs, err := a.Store.CheckinAndReport(cctx)
			cancel()
			if err != nil {
				oc.Status = "failed"
				oc.Detail = err.Error()
				s.pool.NoteError(a.UID)
				log.Printf("WARN: 签到失败 %s: %v", a.UID, err)
				out = append(out, oc)
				continue
			}
			if cs != nil {
				oc.Credits = cs.Credits + cs.ExtraCredits
			}
			if did {
				oc.Status = "checked"
				s.pool.NoteSuccess(a.UID)
				log.Printf("INFO: 签到成功 %s（+%d 积分）", a.UID, oc.Credits)
			} else {
				oc.Status = "already"
			}
			// 签到后顺手刷新额度，让控制台立刻看到新余额
			s.refreshAccountCredits(ctx, a)
			out = append(out, oc)
		}
	}
	_ = s.pool.Save()
	return out
}

// refreshAccountCredits 刷新单账号额度并写入池（只读；失败不惩罚账号）。
func (s *Server) refreshAccountCredits(ctx context.Context, a *pool.Account) (float64, bool) {
	cctx, cancel := traectl.WithCreditsTimeout(ctx)
	defer cancel()
	u, err := a.Store.EntUsage(cctx)
	if err != nil {
		// 只读接口失败不冷却账号：查不到余额不等于账号不可用。
		log.Printf("WARN: 额度查询失败 %s: %v", a.UID, err)
		return 0, false
	}
	remain := u.UsageSummary.Remaining()
	// 同时记总量与"即将过期量"：
	//   - 总量让权重能用"剩余占比"（1000/2000 与 1000/100000 不该等价看待）
	//   - 过期量让权重能"优先烧快作废的额度"
	expiring := u.SumExpiringCredits(traectl.DefaultExpiringWindow, time.Now())
	s.pool.SetCreditsDetailed(a.UID, int64(remain), int64(u.UsageSummary.TotalAmount), expiring)
	return remain, true
}

// RefreshCredits 刷新全部账号额度，返回成功数。
func (s *Server) RefreshCredits(ctx context.Context) int {
	ok := 0
	for _, ch := range s.channels {
		for _, a := range ch.Accounts() {
			if _, good := s.refreshAccountCredits(ctx, a); good {
				ok++
			}
		}
	}
	if ok > 0 {
		_ = s.pool.Save()
	}
	return ok
}

// CreditsSummary 汇总各账号额度（供 /status 与控制台）。
type CreditsSummary struct {
	UID       string  `json:"uid"`
	Label     string  `json:"label"`
	Realm     string  `json:"realm"`
	Remaining float64 `json:"remaining"`
	Total     float64 `json:"total"`
	Consumed  float64 `json:"consumed"`
	Ratio     float64 `json:"consumption_ratio"`
	Packs     int     `json:"entitlement_packs"`
	// PackDetails 权益包明细（面额与到期时间；end_time=0 表示永久）。
	PackDetails []PackDetail `json:"pack_details,omitempty"`
	CheckedIn   bool         `json:"checked_in_today"`
	Error       string       `json:"error,omitempty"`
}

// PackDetail 单个权益包的展示明细。
type PackDetail struct {
	Desc      string  `json:"desc"`
	Amount    float64 `json:"amount"`
	ExpiresAt string  `json:"expires_at,omitempty"` // MM-DD [HH:mm]
}

// CreditsSnapshot 查询全部账号的额度明细（**只读、实时**，不缓存）。
//
// 控制台与 /status 都走这里：每次调用都打上游，因此不建议高频轮询。
// 若需要低开销的展示，用 /status 里的 accounts[].credits（那是后台刷新写入的缓存值）。
func (s *Server) CreditsSnapshot(ctx context.Context) []CreditsSummary {
	out := make([]CreditsSummary, 0)
	for _, ch := range s.channels {
		for _, a := range ch.Accounts() {
			cs := CreditsSummary{UID: a.UID, Label: a.Label, Realm: a.Realm}
			cctx, cancel := traectl.WithCreditsTimeout(ctx)
			u, err := a.Store.EntUsage(cctx)
			if err != nil {
				cs.Error = err.Error()
				cancel()
				out = append(out, cs)
				continue
			}
			cs.Remaining = u.UsageSummary.Remaining()
			cs.Total = u.UsageSummary.TotalAmount
			cs.Consumed = u.UsageSummary.ConsumedAmount
			cs.Ratio = u.UsageSummary.ConsumptionRatio
			cs.Packs = len(u.Packs)
			for _, p := range u.Packs {
				pd := PackDetail{Desc: p.DisplayDesc, Amount: p.BaseInfo.ChargeAmount}
				if p.BaseInfo.EndTime > 0 {
					pd.ExpiresAt = time.Unix(p.BaseInfo.EndTime, 0).Local().Format("01-02 15:04")
				}
				cs.PackDetails = append(cs.PackDetails, pd)
			}
			if st, serr := a.Store.CheckinStatus(cctx); serr == nil && st != nil {
				cs.CheckedIn = st.CheckedIn
			}
			cancel()
			out = append(out, cs)
		}
	}
	return out
}

// RunBackground 启动后台任务：额度刷新 + 每日签到。
//
// 阻塞直到 ctx 取消。由 main 以 goroutine 方式调用。
func (s *Server) RunBackground(ctx context.Context) {
	// 启动时先刷一次额度（让控制台立刻有数据）
	go func() {
		// 稍等片刻，避免和启动日志抢输出
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		if n := s.RefreshCredits(ctx); n > 0 {
			log.Printf("INFO: 启动额度刷新完成（%d 个账号）", n)
		}
	}()

	creditTicker := time.NewTicker(s.creditsInterval())
	defer creditTicker.Stop()

	// 指标落盘：比池状态更长的间隔（指标是可丢的观测数据，不必频繁写盘）。
	// Collector.Save 内部有 dirty 判断，无新请求时不产生磁盘 IO。
	metricsTicker := time.NewTicker(2 * time.Minute)
	defer metricsTicker.Stop()

	checkinTimer := time.NewTimer(s.nextCheckinDelay())
	defer checkinTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = s.pool.Save()
			if s.metrics != nil {
				_ = s.metrics.Save()
			}
			return
		case <-metricsTicker.C:
			if s.metrics != nil {
				if err := s.metrics.Save(); err != nil {
					log.Printf("WARN: 指标落盘失败: %v", err)
				}
			}
		case <-creditTicker.C:
			if n := s.RefreshCredits(ctx); n > 0 {
				log.Printf("INFO: 定期额度刷新完成（%d 个账号）", n)
			}
		case <-checkinTimer.C:
			if s.cfg.Schedule.CheckinEnabled {
				results := s.CheckinAll(ctx)
				checked, already, failed := 0, 0, 0
				for _, r := range results {
					switch r.Status {
					case "checked":
						checked++
					case "already":
						already++
					case "failed":
						failed++
					}
				}
				log.Printf("INFO: 每日签到完成：新签 %d，已签 %d，失败 %d", checked, already, failed)
			}
			checkinTimer.Reset(s.nextCheckinDelay())
		}
	}
}

// creditsInterval 额度刷新间隔（默认 30 分钟）。
func (s *Server) creditsInterval() time.Duration {
	return 30 * time.Minute
}

// nextCheckinDelay 计算距下一个签到时点的时长。
//
// 时点来自 config.schedule.checkin_hours（默认 [9, 21]）。若今天剩下的时点都过了，
// 就落到明天的第一个时点。始终返回正时长（否则 ticker 会立刻再次触发）。
func (s *Server) nextCheckinDelay() time.Duration {
	hours := s.cfg.Schedule.CheckinHours
	if len(hours) == 0 {
		hours = []int{9, 21}
	}
	now := time.Now()
	var best time.Time
	for _, h := range hours {
		if h < 0 || h > 23 {
			continue
		}
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if best.IsZero() || t.Before(best) {
			best = t
		}
	}
	if best.IsZero() {
		best = now.Add(12 * time.Hour)
	}
	d := time.Until(best)
	if d < time.Second {
		d = time.Second
	}
	return d
}
