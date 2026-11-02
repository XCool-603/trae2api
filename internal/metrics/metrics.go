// Package metrics 采集与聚合网关的请求指标。
//
// 设计纪律（来自对 workbuddy2api 的复盘，这几条决定了统计可不可信）：
//
//  1. **「缺观测」与「显式 0」必须分离**。上游不一定每次都给 usage
//     （错误响应、非流式中断、上游省略 token_usage 事件）。若把"没拿到"当成 0，
//     平均值会被系统性拉低，看板会显示"tokens 骤降"这种假警报。
//     因此：Agg 里记 HasUsage **次数**，且暴露时对无观测的均值返回 -1 哨兵。
//
//  2. **缓存命中率的分母排除 cacheWrite**。命中率是 hit/(hit+miss)，
//     把"写入缓存"也算进分母会让第一次请求永远显示为未命中，长期低估。
//
//  3. **TTFB 均值的分母是 ttfbCount，不是 requests**。非流式请求没有首字节概念，
//     拿总请求数当分母会把 TTFB 均值稀释成无意义的数。
//
//  4. **byDay 按自然日分桶 + 保留窗口**。workbuddy2api 的 byDay 是无窗口的 map，
//     长期运行会无限增长（它自己也标注了"每次全量序列化，会变慢"）。
//     本项目加了保留天数，超期分桶在落盘与恢复时双向惰性剔除。
package metrics

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// NoObservation 是"此项无观测"的哨兵值（用于均值字段）。
//
// 用 -1 而不是 0：0 是一个合法的真实值（例如缓存命中 0 次），
// 混在一起会让前端无法区分。这与 workbuddy2api 的 toks:-1 哨兵同一意图。
const NoObservation int64 = -1

// Sample 是一次请求的观测样本。
type Sample struct {
	Model     string
	Account   string
	Realm     string
	Streaming bool
	Success   bool

	// TTFBMS 首字节耗时（毫秒）；**无观测时传 NoObservation**（非流式请求没有首字节）。
	TTFBMS int64
	// LatencyMS 总耗时。
	LatencyMS int64

	// Usage 三项；HasUsage=false 时这三个值被**忽略**（而不是当 0）。
	PromptTokens     int64
	CompletionTokens int64
	CacheHit         int64
	CacheMiss        int64
	CacheWrite       int64
	HasUsage         bool

	// Credits 上游返回的积分消耗；HasCredits=false 时忽略。
	Credits    float64
	HasCredits bool

	At time.Time
}

// RequestRecord 是一次调用的观测流水记录（保留最近 N 条，供控制台逐条呈现）。
type RequestRecord struct {
	Time             string  `json:"time"`
	Model            string  `json:"model"`
	Account          string  `json:"account"`
	Streaming        bool    `json:"streaming"`
	Success          bool    `json:"success"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CreditCost       float64 `json:"credit_cost"`
	LatencyMS        int64   `json:"latency_ms"`
	TTFBMS           int64   `json:"ttfb_ms"`
}

// Agg 是一组样本的聚合。
//
// 所有计数字段都是 int64，便于原子累加与 JSON 序列化。
type Agg struct {
	Requests  int64 `json:"requests"`
	Success   int64 `json:"success"`
	Failed    int64 `json:"failed"`
	Streaming int64 `json:"streaming"`

	// TTFB 只在**有观测**的请求上累加，分母是 TTFBCount（不是 Requests）。
	TTFBSumMS int64 `json:"ttfb_sum_ms"`
	TTFBCount int64 `json:"ttfb_count"`

	// 总耗时在全部请求上累加，分母是 Requests。
	LatencySumMS int64 `json:"latency_sum_ms"`

	// Usage 只在**有观测**的请求上累加，分母是 UsageCount（不是 Requests）。
	UsageCount       int64 `json:"usage_count"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	CacheHit         int64 `json:"cache_hit"`
	CacheMiss        int64 `json:"cache_miss"`
	CacheWrite       int64 `json:"cache_write"`

	CreditSum   float64 `json:"credit_sum"`
	CreditCount int64   `json:"credit_count"`

	LastSeen time.Time `json:"last_seen"`
}

// merge 把另一份聚合并入自身（用于恢复与快照合并）。
func (a *Agg) merge(b Agg) {
	a.Requests += b.Requests
	a.Success += b.Success
	a.Failed += b.Failed
	a.Streaming += b.Streaming
	a.TTFBSumMS += b.TTFBSumMS
	a.TTFBCount += b.TTFBCount
	a.LatencySumMS += b.LatencySumMS
	a.UsageCount += b.UsageCount
	a.PromptTokens += b.PromptTokens
	a.CompletionTokens += b.CompletionTokens
	a.CacheHit += b.CacheHit
	a.CacheMiss += b.CacheMiss
	a.CacheWrite += b.CacheWrite
	a.CreditSum += b.CreditSum
	a.CreditCount += b.CreditCount
	if b.LastSeen.After(a.LastSeen) {
		a.LastSeen = b.LastSeen
	}
}

// View 是暴露给前端的只读视图（含算好的均值与哨兵）。
type View struct {
	Requests  int64 `json:"requests"`
	Success   int64 `json:"success"`
	Failed    int64 `json:"failed"`
	Streaming int64 `json:"streaming"`

	// AvgTTFBMS 无观测时为 NoObservation（-1）。
	AvgTTFBMS int64 `json:"avg_ttfb_ms"`
	// AvgLatencyMS 始终有意义（分母是 Requests）。
	AvgLatencyMS int64 `json:"avg_latency_ms"`

	// UsageCount 是**有 usage 观测**的请求数 —— 与 Requests 的差额就是"缺观测"的量。
	// 前端应当据此判断均值可信度，而不是把 -1 当成 0 显示。
	UsageCount int64 `json:"usage_count"`
	// 原始总量（有观测的部分累加）—— 控制台既想看总量也想看均值。
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	AvgPromptTokens  int64 `json:"avg_prompt_tokens"`
	AvgCompTokens    int64 `json:"avg_completion_tokens"`
	CacheHit         int64 `json:"cache_hit"`
	CacheMiss        int64 `json:"cache_miss"`
	CacheWrite       int64 `json:"cache_write"`
	// CacheHitRate 无观测时为 NoObservation。
	CacheHitRate int64 `json:"cache_hit_rate_percent"`

	CreditSum   float64 `json:"credit_sum"`
	CreditCount int64   `json:"credit_count"`
	// AvgCredits 无观测时为 NoObservation。
	AvgCredits int64 `json:"avg_credits_x1000"`

	LastSeen string `json:"last_seen,omitempty"`
}

// View 把聚合算成只读视图（含均值与哨兵）。
func (a Agg) View() View {
	v := View{
		Requests: a.Requests, Success: a.Success, Failed: a.Failed, Streaming: a.Streaming,
		UsageCount:       a.UsageCount,
		PromptTokens:     a.PromptTokens,
		CompletionTokens: a.CompletionTokens,
		TotalTokens:      a.PromptTokens + a.CompletionTokens,
		CacheHit:         a.CacheHit, CacheMiss: a.CacheMiss, CacheWrite: a.CacheWrite,
		CreditSum: a.CreditSum, CreditCount: a.CreditCount,
	}
	v.AvgTTFBMS = NoObservation
	v.AvgLatencyMS = NoObservation
	if a.TTFBCount > 0 {
		v.AvgTTFBMS = a.TTFBSumMS / a.TTFBCount
	}
	if a.Requests > 0 {
		v.AvgLatencyMS = a.LatencySumMS / a.Requests
	}
	if a.UsageCount > 0 {
		v.AvgPromptTokens = a.PromptTokens / a.UsageCount
		v.AvgCompTokens = a.CompletionTokens / a.UsageCount
	}
	// 命中率分母**排除 cacheWrite**（见包注释第 2 条）
	v.CacheHitRate = NoObservation
	if d := a.CacheHit + a.CacheMiss; d > 0 {
		v.CacheHitRate = a.CacheHit * 100 / d
	}
	// AvgCredits 也必须默认哨兵：
	// Go 的零值是 0，而 0 是"真实观测到积分为 0"的合法值 ——
	// 不显式初始化就会让"无观测"看起来像"观测到 0"。
	// （这条正是本包要防的错误，我自己的第一版就在这里犯了一次，被测试抓出来。）
	if a.CreditCount > 0 {
		v.AvgCredits = int64(a.CreditSum / float64(a.CreditCount) * 1000)
	} else {
		v.AvgCredits = NoObservation
	}
	if !a.LastSeen.IsZero() {
		v.LastSeen = a.LastSeen.Format(time.RFC3339)
	}
	return v
}

// Collector 是指标收集器。并发安全。
type Collector struct {
	mu sync.RWMutex

	global    Agg
	byModel   map[string]*Agg
	byAccount map[string]*Agg
	byRealm   map[string]*Agg
	byDay     map[string]*Agg // 键为 "2006-01-02"（自然日，本地时区）

	// retainDays 是 byDay 的保留天数（<=0 表示不清理，等于 workbuddy2api 的行为）。
	retainDays int
	stateFile  string

	dirty    bool
	lastSave time.Time

	recent    []RequestRecord
	maxRecent int
}

// Options 构造参数。
type Options struct {
	// RetainDays byDay 保留天数（默认 14）。<=0 表示不清理。
	RetainDays int
	// StateFile 持久化路径；空 = 不落盘。
	StateFile string
}

// New 构造收集器。
func New(opt Options) *Collector {
	if opt.RetainDays == 0 {
		opt.RetainDays = 14
	}
	return &Collector{
		byModel:    map[string]*Agg{},
		byAccount:  map[string]*Agg{},
		byRealm:    map[string]*Agg{},
		byDay:      map[string]*Agg{},
		retainDays: opt.RetainDays,
		stateFile:  opt.StateFile,
		maxRecent:  100,
	}
}

// dayKey 返回自然日键（本地时区 —— 用户看的是本地日期，不是 UTC）。
func dayKey(t time.Time) string { return t.Format("2006-01-02") }

// Record 记录一个样本。
func (c *Collector) Record(s Sample) {
	if s.At.IsZero() {
		s.At = time.Now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	apply := func(a *Agg) {
		a.Requests++
		if s.Success {
			a.Success++
		} else {
			a.Failed++
		}
		if s.Streaming {
			a.Streaming++
		}
		if s.TTFBMS != NoObservation && s.TTFBMS > 0 {
			a.TTFBSumMS += s.TTFBMS
			a.TTFBCount++
		}
		if s.LatencyMS > 0 {
			a.LatencySumMS += s.LatencyMS
		}
		// 关键：无 usage 观测时**什么都不加**（UsageCount 也不加）——
		// 这样 Requests 与 UsageCount 的差额就是"缺观测"的量。
		if s.HasUsage {
			a.UsageCount++
			a.PromptTokens += s.PromptTokens
			a.CompletionTokens += s.CompletionTokens
			a.CacheHit += s.CacheHit
			a.CacheMiss += s.CacheMiss
			a.CacheWrite += s.CacheWrite
		}
		if s.HasCredits {
			a.CreditSum += s.Credits
			a.CreditCount++
		}
		if s.At.After(a.LastSeen) {
			a.LastSeen = s.At
		}
	}

	apply(&c.global)
	if s.Model != "" {
		apply(c.bucket(c.byModel, s.Model))
	}
	if s.Account != "" {
		apply(c.bucket(c.byAccount, s.Account))
	}
	if s.Realm != "" {
		apply(c.bucket(c.byRealm, s.Realm))
	}
	apply(c.bucket(c.byDay, dayKey(s.At)))

	c.dirty = true

	if s.Model != "" || s.Account != "" {
		if c.maxRecent <= 0 {
			c.maxRecent = 100
		}
		rec := RequestRecord{
			Time:             s.At.Format("15:04:05"),
			Model:            s.Model,
			Account:          s.Account,
			Streaming:        s.Streaming,
			Success:          s.Success,
			PromptTokens:     s.PromptTokens,
			CompletionTokens: s.CompletionTokens,
			TotalTokens:      s.PromptTokens + s.CompletionTokens,
			CreditCost:       s.Credits,
			LatencyMS:        s.LatencyMS,
			TTFBMS:           s.TTFBMS,
		}
		if len(c.recent) >= c.maxRecent {
			c.recent = append(c.recent[1:], rec)
		} else {
			c.recent = append(c.recent, rec)
		}
	}
}

// bucket 取或建一个分桶（调用方持锁）。
func (c *Collector) bucket(m map[string]*Agg, k string) *Agg {
	a, ok := m[k]
	if !ok {
		a = &Agg{}
		m[k] = a
	}
	return a
}

// Snapshot 返回全部聚合的只读快照。
type Snapshot struct {
	Global    View            `json:"global"`
	ByModel   map[string]View `json:"by_model"`
	ByAccount map[string]View `json:"by_account"`
	ByRealm   map[string]View `json:"by_realm"`
	// ByDay 按日期升序排列（前端直接渲染）。
	ByDay      []DayView `json:"by_day"`
	RetainDays int       `json:"retain_days"`
	// NoObservation 是哨兵值，供前端判断"此项无观测"。
	NoObservation int64 `json:"no_observation_sentinel"`
	// RecentRequests 最近请求调用流水（最新的在前）。
	RecentRequests []RequestRecord `json:"recent_requests"`
}

// DayView 是一天的聚合（带日期键）。
type DayView struct {
	Day  string `json:"day"`
	View View   `json:"view"`
}

// Snapshot 取当前快照（会顺带执行 byDay 过期清理）。
func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	c.trimLocked(time.Now())
	c.mu.Unlock()

	c.mu.RLock()
	defer c.mu.RUnlock()

	s := Snapshot{
		Global:        c.global.View(),
		ByModel:       map[string]View{},
		ByAccount:     map[string]View{},
		ByRealm:       map[string]View{},
		RetainDays:    c.retainDays,
		NoObservation: NoObservation,
	}
	for k, a := range c.byModel {
		s.ByModel[k] = a.View()
	}
	for k, a := range c.byAccount {
		s.ByAccount[k] = a.View()
	}
	for k, a := range c.byRealm {
		s.ByRealm[k] = a.View()
	}
	days := make([]string, 0, len(c.byDay))
	for k := range c.byDay {
		days = append(days, k)
	}
	sort.Strings(days)
	for _, d := range days {
		s.ByDay = append(s.ByDay, DayView{Day: d, View: c.byDay[d].View()})
	}
	recentCopy := make([]RequestRecord, len(c.recent))
	for i := range c.recent {
		recentCopy[len(c.recent)-1-i] = c.recent[i]
	}
	s.RecentRequests = recentCopy
	return s
}

// trimLocked 剔除超出保留窗口的自然日分桶（调用方持锁）。
//
// 惰性执行：不做后台定时器，在 Snapshot / Save / Restore 时顺手清理即可 ——
// 指标不是关键路径，没必要为它引入一个 goroutine。
func (c *Collector) trimLocked(now time.Time) {
	if c.retainDays <= 0 || len(c.byDay) == 0 {
		return
	}
	cutoff := now.AddDate(0, 0, -c.retainDays).Format("2006-01-02")
	for k := range c.byDay {
		if k < cutoff { // 日期字符串可直接字典序比较
			delete(c.byDay, k)
		}
	}
}

// Reset 清零全部指标。
func (c *Collector) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.global = Agg{}
	c.byModel = map[string]*Agg{}
	c.byAccount = map[string]*Agg{}
	c.byRealm = map[string]*Agg{}
	c.byDay = map[string]*Agg{}
	c.recent = nil
	c.dirty = true
}

// ---- 持久化 ----

// persisted 是落盘结构。
type persisted struct {
	Version   int            `json:"version"`
	SavedAt   string         `json:"saved_at"`
	Global    Agg            `json:"global"`
	ByModel   map[string]Agg `json:"by_model"`
	ByAccount map[string]Agg `json:"by_account"`
	ByRealm   map[string]Agg `json:"by_realm"`
	ByDay     map[string]Agg `json:"by_day"`
}

const persistVersion = 1

// Save 落盘（若自上次落盘以来无变化则跳过）。
//
// 用 tmp+rename 原子替换。**刻意不做 fsync** ——
// 指标是"可丢的观测数据"，为它付 fsync 的代价不值得（丢一次快照无实质影响）；
// 而账号凭据那种"丢了要重登"的数据才需要 fsync。
func (c *Collector) Save() error {
	if c.stateFile == "" {
		return nil
	}
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return nil
	}
	c.trimLocked(time.Now())
	snap := persisted{
		Version: persistVersion, SavedAt: time.Now().Format(time.RFC3339),
		Global:    c.global,
		ByModel:   map[string]Agg{},
		ByAccount: map[string]Agg{},
		ByRealm:   map[string]Agg{},
		ByDay:     map[string]Agg{},
	}
	for k, a := range c.byModel {
		snap.ByModel[k] = *a
	}
	for k, a := range c.byAccount {
		snap.ByAccount[k] = *a
	}
	for k, a := range c.byRealm {
		snap.ByRealm[k] = *a
	}
	for k, a := range c.byDay {
		snap.ByDay[k] = *a
	}
	c.dirty = false
	c.lastSave = time.Now()
	c.mu.Unlock()

	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("metrics: 序列化: %w", err)
	}
	dir := filepath.Dir(c.stateFile)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("metrics: 创建目录: %w", err)
		}
	}
	tmp, err := os.CreateTemp(dir, ".metrics-*.tmp")
	if err != nil {
		return fmt.Errorf("metrics: 创建临时文件: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("metrics: 写临时文件: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("metrics: 关闭临时文件: %w", err)
	}
	if err := os.Rename(name, c.stateFile); err != nil {
		os.Remove(c.stateFile)
		if err2 := os.Rename(name, c.stateFile); err2 != nil {
			os.Remove(name)
			return fmt.Errorf("metrics: 替换 %s: %w", c.stateFile, err2)
		}
	}
	return nil
}

// Restore 从落盘恢复（文件不存在静默通过）。
//
// **双向惰性过期过滤**：恢复时也要 trim 一次 —— 否则一份很久以前的快照会把
// 早已过期的日分桶带回来，让保留窗口形同虚设。
func (c *Collector) Restore() error {
	if c.stateFile == "" {
		return nil
	}
	raw, err := os.ReadFile(c.stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("metrics: 读取 %s: %w", c.stateFile, err)
	}
	var snap persisted
	if err := json.Unmarshal(raw, &snap); err != nil {
		return fmt.Errorf("metrics: 解析 %s: %w", c.stateFile, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.global = snap.Global
	for k, a := range snap.ByModel {
		v := a
		c.byModel[k] = &v
	}
	for k, a := range snap.ByAccount {
		v := a
		c.byAccount[k] = &v
	}
	for k, a := range snap.ByRealm {
		v := a
		c.byRealm[k] = &v
	}
	for k, a := range snap.ByDay {
		v := a
		c.byDay[k] = &v
	}
	c.trimLocked(time.Now()) // 恢复侧也过滤（见函数注释）
	c.dirty = false
	return nil
}

// LastSave 返回最近一次落盘时间（供诊断）。
func (c *Collector) LastSave() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastSave
}
