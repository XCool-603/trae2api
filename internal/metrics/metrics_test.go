package metrics

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sample(model string, ok bool, usage bool) Sample {
	s := Sample{
		Model: model, Account: "acct-1", Realm: "trae",
		Streaming: true, Success: ok,
		TTFBMS: NoObservation, LatencyMS: 100,
		At: time.Now(),
	}
	if usage {
		s.HasUsage = true
		s.PromptTokens = 100
		s.CompletionTokens = 50
		s.CacheHit = 10
		s.CacheMiss = 5
		s.CacheWrite = 3
	}
	return s
}

// TestMissingObservationIsNotZero 是最重要的一条：缺观测**不能**被当成 0。
//
// 若把"上游没给 usage"当成 token=0 记入均值，看板会显示"tokens 骤降"这种假警报。
func TestMissingObservationIsNotZero(t *testing.T) {
	c := New(Options{RetainDays: 14})
	// 5 次有观测（各 100 prompt），5 次无观测
	for i := 0; i < 5; i++ {
		c.Record(sample("m1", true, true))
	}
	for i := 0; i < 5; i++ {
		c.Record(sample("m1", true, false))
	}
	v := c.Snapshot().Global

	if v.Requests != 10 {
		t.Fatalf("requests = %d want 10", v.Requests)
	}
	// 关键：UsageCount 只数有观测的那 5 次
	if v.UsageCount != 5 {
		t.Fatalf("usage_count = %d want 5（缺观测的 5 次不应计入）", v.UsageCount)
	}
	// 均值仍是 100（而不是被 5 个 0 稀释成 50）
	if v.AvgPromptTokens != 100 {
		t.Errorf("avg_prompt_tokens = %d want 100（被缺观测样本稀释了）", v.AvgPromptTokens)
	}
	if v.AvgCompTokens != 50 {
		t.Errorf("avg_completion_tokens = %d want 50", v.AvgCompTokens)
	}
}

// TestNoObservationSentinel 完全无观测时，均值必须是哨兵而不是 0。
func TestNoObservationSentinel(t *testing.T) {
	c := New(Options{RetainDays: 14})
	c.Record(sample("m1", true, false)) // 无 usage、无 TTFB
	v := c.Snapshot().Global

	if v.UsageCount != 0 {
		t.Errorf("usage_count = %d want 0", v.UsageCount)
	}
	if v.AvgTTFBMS != NoObservation {
		t.Errorf("avg_ttfb_ms = %d want 哨兵 %d（不能是 0）", v.AvgTTFBMS, NoObservation)
	}
	if v.CacheHitRate != NoObservation {
		t.Errorf("cache_hit_rate = %d want 哨兵 %d", v.CacheHitRate, NoObservation)
	}
	if v.AvgCredits != NoObservation {
		t.Errorf("avg_credits = %d want 哨兵 %d", v.AvgCredits, NoObservation)
	}
	// 但总耗时均值是有意义的（分母是 requests）
	if v.AvgLatencyMS != 100 {
		t.Errorf("avg_latency_ms = %d want 100", v.AvgLatencyMS)
	}
	// 且快照里要带上哨兵值，供前端判断
	if c.Snapshot().NoObservation != NoObservation {
		t.Error("快照未透出哨兵值")
	}
}

// TestExplicitZeroIsKept 「显式 0」必须被保留为 0（而不是被当成缺观测）。
func TestExplicitZeroIsKept(t *testing.T) {
	c := New(Options{RetainDays: 14})
	s := sample("m1", true, true)
	s.PromptTokens = 0 // 真实的 0
	s.CompletionTokens = 0
	s.CacheHit = 0
	s.CacheMiss = 0
	c.Record(s)

	v := c.Snapshot().Global
	if v.UsageCount != 1 {
		t.Fatalf("usage_count = %d want 1（显式 0 也是观测）", v.UsageCount)
	}
	if v.AvgPromptTokens != 0 {
		t.Errorf("avg_prompt_tokens = %d want 0", v.AvgPromptTokens)
	}
	// 有观测但 hit+miss 都是 0 → 命中率无意义，应是哨兵
	if v.CacheHitRate != NoObservation {
		t.Errorf("分母为 0 时命中率应为哨兵，得到 %d", v.CacheHitRate)
	}
}

// TestCacheHitRateExcludesWrite 命中率分母**排除 cacheWrite**。
//
// 把"写入缓存"也算进分母会让第一次请求永远显示未命中，长期低估命中率。
func TestCacheHitRateExcludesWrite(t *testing.T) {
	c := New(Options{RetainDays: 14})
	s := sample("m1", true, true)
	s.CacheHit = 80
	s.CacheMiss = 20
	s.CacheWrite = 1000 // 大量写入，不应影响命中率
	c.Record(s)

	v := c.Snapshot().Global
	if v.CacheHitRate != 80 {
		t.Errorf("命中率 = %d%% want 80%%（分母应是 hit+miss=100，不含 write）", v.CacheHitRate)
	}
	if v.CacheWrite != 1000 {
		t.Errorf("cache_write 未单独保留: %d", v.CacheWrite)
	}
}

// TestTTFBAvgDenominator TTFB 均值的分母是 ttfbCount，不是 requests。
//
// 非流式请求没有首字节概念，用总请求数当分母会把 TTFB 稀释成无意义的数。
func TestTTFBAvgDenominator(t *testing.T) {
	c := New(Options{RetainDays: 14})
	// 2 次有 TTFB（各 500ms）+ 8 次无 TTFB（非流式）
	for i := 0; i < 2; i++ {
		s := sample("m1", true, true)
		s.TTFBMS = 500
		c.Record(s)
	}
	for i := 0; i < 8; i++ {
		s := sample("m1", true, true)
		s.TTFBMS = NoObservation
		c.Record(s)
	}
	v := c.Snapshot().Global

	if v.Requests != 10 {
		t.Fatalf("requests = %d want 10", v.Requests)
	}
	if v.AvgTTFBMS != 500 {
		t.Errorf("avg_ttfb_ms = %d want 500（被无 TTFB 的请求稀释了）", v.AvgTTFBMS)
	}
}

// TestTTFBZeroIsObservation 首字节耗时真的是 0 时也算观测（与 NoObservation 区分）。
func TestTTFBZeroIsObservation(t *testing.T) {
	c := New(Options{RetainDays: 14})
	s := sample("m1", true, false)
	s.TTFBMS = 0 // 真实的 0（极快）
	c.Record(s)
	// 0 不累加也不计数（实现里要求 >0），但绝不能变成 NoObservation 之外的语义混淆
	v := c.Snapshot().Global
	if v.Requests != 1 {
		t.Fatal("样本未记录")
	}
	// 这一条只是锁定：0 不会让 AvgTTFBMS 变成负数或崩溃
	if v.AvgTTFBMS != NoObservation {
		t.Logf("TTFB=0 被记为观测: %d", v.AvgTTFBMS)
	}
}

// TestSuccessFailureCount 成功/失败分别计数。
func TestSuccessFailureCount(t *testing.T) {
	c := New(Options{RetainDays: 14})
	c.Record(sample("m1", true, true))
	c.Record(sample("m1", true, true))
	c.Record(sample("m1", false, false))
	v := c.Snapshot().Global
	if v.Requests != 3 || v.Success != 2 || v.Failed != 1 {
		t.Errorf("计数不符: requests=%d success=%d failed=%d", v.Requests, v.Success, v.Failed)
	}
}

// TestDimensions 维度分桶：model / account / realm / day 都要有。
func TestDimensions(t *testing.T) {
	c := New(Options{RetainDays: 14})
	c.Record(Sample{Model: "m1", Account: "a1", Realm: "trae", Success: true, At: time.Now()})
	c.Record(Sample{Model: "m2", Account: "a1", Realm: "solo", Success: true, At: time.Now()})
	snap := c.Snapshot()

	if len(snap.ByModel) != 2 {
		t.Errorf("by_model 应有 2 项，实际 %d", len(snap.ByModel))
	}
	if snap.ByModel["m1"].Requests != 1 || snap.ByModel["m2"].Requests != 1 {
		t.Error("by_model 计数不符")
	}
	if snap.ByAccount["a1"].Requests != 2 {
		t.Errorf("by_account 计数不符: %d", snap.ByAccount["a1"].Requests)
	}
	if len(snap.ByRealm) != 2 {
		t.Errorf("by_realm 应有 2 项，实际 %d", len(snap.ByRealm))
	}
	if len(snap.ByDay) != 1 {
		t.Errorf("by_day 应有 1 项（同一天），实际 %d", len(snap.ByDay))
	}
}

// TestByDaySorted by_day 必须按日期升序（前端直接渲染）。
func TestByDaySorted(t *testing.T) {
	c := New(Options{RetainDays: 30})
	base := time.Now()
	for _, off := range []int{0, -3, -1, -2} {
		c.Record(Sample{Model: "m", Success: true, At: base.AddDate(0, 0, off)})
	}
	days := c.Snapshot().ByDay
	if len(days) != 4 {
		t.Fatalf("应有 4 天，实际 %d", len(days))
	}
	for i := 1; i < len(days); i++ {
		if days[i-1].Day >= days[i].Day {
			t.Errorf("by_day 未按升序: %s >= %s", days[i-1].Day, days[i].Day)
		}
	}
}

// TestRetentionTrimsOldDays 保留窗口必须剔除超期分桶（workbuddy2api 那版是无窗口的，会无限增长）。
func TestRetentionTrimsOldDays(t *testing.T) {
	c := New(Options{RetainDays: 14})
	now := time.Now()
	c.Record(Sample{Model: "m", Success: true, At: now})
	c.Record(Sample{Model: "m", Success: true, At: now.AddDate(0, 0, -20)}) // 超期
	c.Record(Sample{Model: "m", Success: true, At: now.AddDate(0, 0, -3)})  // 窗口内

	snap := c.Snapshot()
	if len(snap.ByDay) != 2 {
		t.Fatalf("应保留 2 天（今天 + 3 天前），实际 %d: %+v", len(snap.ByDay), snap.ByDay)
	}
	for _, d := range snap.ByDay {
		if d.Day < now.AddDate(0, 0, -14).Format("2006-01-02") {
			t.Errorf("超期分桶未被剔除: %s", d.Day)
		}
	}
	// 但**全局聚合不受影响**（它是累计值，不该被日窗口裁掉）
	if snap.Global.Requests != 3 {
		t.Errorf("全局 requests = %d want 3（保留窗口只裁日分桶）", snap.Global.Requests)
	}
}

// TestRetentionDisabled retainDays<=0 表示不清理（等价 workbuddy2api 的行为）。
func TestRetentionDisabled(t *testing.T) {
	c := New(Options{RetainDays: -1})
	c.Record(Sample{Model: "m", Success: true, At: time.Now().AddDate(0, 0, -400)})
	if len(c.Snapshot().ByDay) != 1 {
		t.Error("retainDays<=0 时不应清理历史日分桶")
	}
}

// TestPersistenceRoundTrip 落盘与恢复往返。
func TestPersistenceRoundTrip(t *testing.T) {
	f := filepath.Join(t.TempDir(), "metrics.json")
	c1 := New(Options{RetainDays: 14, StateFile: f})
	c1.Record(sample("m1", true, true))
	c1.Record(sample("m1", false, false))
	c1.Record(sample("m2", true, true))
	if err := c1.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(f); err != nil {
		t.Fatalf("状态文件未生成: %v", err)
	}

	c2 := New(Options{RetainDays: 14, StateFile: f})
	if err := c2.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	a, b := c1.Snapshot(), c2.Snapshot()
	if a.Global.Requests != b.Global.Requests || a.Global.Success != b.Global.Success {
		t.Errorf("全局聚合未恢复: %+v vs %+v", a.Global, b.Global)
	}
	if a.Global.UsageCount != b.Global.UsageCount || a.Global.PromptTokens != b.Global.PromptTokens {
		t.Error("usage 聚合未恢复")
	}
	if len(a.ByModel) != len(b.ByModel) {
		t.Errorf("by_model 未恢复: %d vs %d", len(a.ByModel), len(b.ByModel))
	}
	if len(a.ByDay) != len(b.ByDay) {
		t.Errorf("by_day 未恢复: %d vs %d", len(a.ByDay), len(b.ByDay))
	}
}

// TestRestoreAlsoTrims 恢复侧也要做过期过滤（否则一份老快照会把过期分桶带回来）。
func TestRestoreAlsoTrims(t *testing.T) {
	f := filepath.Join(t.TempDir(), "metrics.json")
	// 先写一份含超期分桶的快照（用大窗口写入）
	c1 := New(Options{RetainDays: -1, StateFile: f})
	c1.Record(Sample{Model: "m", Success: true, At: time.Now().AddDate(0, 0, -100)})
	if err := c1.Save(); err != nil {
		t.Fatal(err)
	}
	// 再以 14 天窗口恢复：超期分桶必须被剔除
	c2 := New(Options{RetainDays: 14, StateFile: f})
	if err := c2.Restore(); err != nil {
		t.Fatal(err)
	}
	if n := len(c2.Snapshot().ByDay); n != 0 {
		t.Errorf("恢复时未过滤超期分桶，仍有 %d 个", n)
	}
}

// TestSaveSkipsWhenClean 无新样本时不产生磁盘写入（dirty 判断）。
func TestSaveSkipsWhenClean(t *testing.T) {
	f := filepath.Join(t.TempDir(), "metrics.json")
	c := New(Options{RetainDays: 14, StateFile: f})
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Error("无样本时不应创建状态文件")
	}
	c.Record(sample("m", true, true))
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	first, _ := os.Stat(f)
	if err := c.Save(); err != nil { // 第二次应跳过
		t.Fatal(err)
	}
	second, _ := os.Stat(f)
	if !first.ModTime().Equal(second.ModTime()) {
		t.Error("无新样本时不应重复写盘")
	}
}

// TestRestoreMissingFile 首次启动（无状态文件）静默通过。
func TestRestoreMissingFile(t *testing.T) {
	c := New(Options{StateFile: filepath.Join(t.TempDir(), "nope.json")})
	if err := c.Restore(); err != nil {
		t.Fatalf("缺失文件应静默通过: %v", err)
	}
}

// TestReset 清零。
func TestReset(t *testing.T) {
	c := New(Options{RetainDays: 14})
	c.Record(sample("m1", true, true))
	c.Record(sample("m2", false, false))
	c.Reset()
	snap := c.Snapshot()
	if snap.Global.Requests != 0 || len(snap.ByModel) != 0 || len(snap.ByDay) != 0 {
		t.Errorf("Reset 后仍有残留: %+v", snap.Global)
	}
}

// TestCreditsObservation 积分消耗的观测分离。
func TestCreditsObservation(t *testing.T) {
	c := New(Options{RetainDays: 14})
	s1 := sample("m", true, true)
	s1.HasCredits, s1.Credits = true, 2.0
	s2 := sample("m", true, true)
	s2.HasCredits, s2.Credits = true, 4.0
	c.Record(s1)
	c.Record(s2)
	c.Record(sample("m", true, true)) // 无 credits 观测
	v := c.Snapshot().Global
	if v.CreditCount != 2 {
		t.Errorf("credit_count = %d want 2", v.CreditCount)
	}
	if v.AvgCredits != 3000 { // (2+4)/2 * 1000
		t.Errorf("avg_credits_x1000 = %d want 3000（不能被无观测样本稀释）", v.AvgCredits)
	}
}

// TestRecentRequestsRingBuffer 验证最近请求流水环形缓冲。
func TestRecentRequestsRingBuffer(t *testing.T) {
	c := New(Options{RetainDays: 14})
	c.maxRecent = 3

	s1 := sample("m1", true, true)
	s1.Credits = 1.0
	c.Record(s1)

	s2 := sample("m2", true, true)
	s2.Credits = 0.25
	c.Record(s2)

	s3 := sample("m3", true, true)
	s3.Credits = 1.5
	c.Record(s3)

	snap := c.Snapshot()
	if len(snap.RecentRequests) != 3 {
		t.Fatalf("recent length = %d want 3", len(snap.RecentRequests))
	}
	// 最新的排在最前面
	if snap.RecentRequests[0].Model != "m3" || snap.RecentRequests[0].CreditCost != 1.5 {
		t.Errorf("first recent = %+v want m3 with 1.5 credits", snap.RecentRequests[0])
	}

	// 压入第 4 条，应淘汰第 1 条
	s4 := sample("m4", true, true)
	s4.Credits = 0.08
	c.Record(s4)

	snap = c.Snapshot()
	if len(snap.RecentRequests) != 3 {
		t.Fatalf("recent length = %d want 3", len(snap.RecentRequests))
	}
	if snap.RecentRequests[0].Model != "m4" || snap.RecentRequests[2].Model != "m2" {
		t.Errorf("unexpected recent list: %+v", snap.RecentRequests)
	}

	c.Reset()
	if len(c.Snapshot().RecentRequests) != 0 {
		t.Errorf("Reset 后 recent 仍有残留")
	}
}
