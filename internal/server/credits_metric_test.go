package server

import (
	"testing"
	"time"

	"trae2api/internal/metrics"
	"trae2api/internal/traectl"
)

func TestModelCreditCost(t *testing.T) {
	prod := traectl.Product{Key: "trae-cn", Label: "Trae CN"}
	catalog := &traectl.Catalog{
		Product: prod,
		Models: []traectl.Model{
			{ID: "claude-3-7-sonnet", ConsumptionRate: 1.0},
			{ID: "deepseek-v3", ConsumptionRate: 0.2},
			{ID: "qwen3.8-max", ConsumptionRate: 1.5},
			{ID: "old-model", FeeModelLevel: 2},
		},
	}
	ch := &Channel{Catalog: catalog, Product: prod}
	s := &Server{}

	tests := []struct {
		model string
		want  float64
	}{
		{"claude-3-7-sonnet", 1.0},
		{"deepseek-v3", 0.2},
		{"qwen3.8-max", 1.5},
		{"old-model", 2.0},
		{"unknown-model", 1.0}, // 保底默认 1.0
	}

	for _, tt := range tests {
		got := s.modelCreditCost(ch, tt.model)
		if got != tt.want {
			t.Errorf("modelCreditCost(%q) = %v, want %v", tt.model, got, tt.want)
		}
	}
}

func TestUsageToMapWithCredits(t *testing.T) {
	u := &traectl.Usage{
		PromptTokens:             100,
		CompletionTokens:         50,
		TotalTokens:              150,
		CacheReadInputTokens:     20,
		CacheCreationInputTokens: 10,
		ReasoningTokens:          5,
	}

	m := usageToMap(u, 0.25)
	if m["credits"] != 0.25 {
		t.Errorf("usage.credits = %v, want 0.25", m["credits"])
	}
	if m["credit_cost"] != 0.25 {
		t.Errorf("usage.credit_cost = %v, want 0.25", m["credit_cost"])
	}
	if m["total_tokens"] != 150 {
		t.Errorf("usage.total_tokens = %v, want 150", m["total_tokens"])
	}
}

func TestChatStatCreditsRecording(t *testing.T) {
	mc := metrics.New(metrics.Options{RetainDays: 14})
	now := time.Now()

	// 1. 成功请求消耗 0.2 积分
	st1 := newChatStat(now, "deepseek-v3", true)
	st1.collector = mc
	st1.setAccount("acct-1", "Label 1")
	st1.setRealm("trae")
	st1.setCredits(0.2)
	st1.recordUsage(&traectl.Usage{PromptTokens: 80, CompletionTokens: 40, TotalTokens: 120})
	st1.done()

	snap := mc.Snapshot()
	if snap.Global.CreditSum != 0.2 {
		t.Errorf("Global.CreditSum = %v, want 0.2", snap.Global.CreditSum)
	}
	if len(snap.RecentRequests) != 1 {
		t.Fatalf("RecentRequests count = %d, want 1", len(snap.RecentRequests))
	}
	if snap.RecentRequests[0].CreditCost != 0.2 {
		t.Errorf("RecentRequests[0].CreditCost = %v, want 0.2", snap.RecentRequests[0].CreditCost)
	}

	// 2. 失败请求不应累加积分
	st2 := newChatStat(now, "claude-3-7-sonnet", false)
	st2.collector = mc
	st2.setAccount("acct-1", "Label 1")
	st2.setRealm("trae")
	st2.setCredits(1.0)
	st2.markFailed()
	st2.done()

	snap2 := mc.Snapshot()
	if snap2.Global.CreditSum != 0.2 {
		t.Errorf("After failed request, Global.CreditSum = %v, want 0.2 (不应累加)", snap2.Global.CreditSum)
	}
	if len(snap2.RecentRequests) != 2 {
		t.Fatalf("RecentRequests count = %d, want 2", len(snap2.RecentRequests))
	}
	// 最新一条是失败的 claude 请求
	if snap2.RecentRequests[0].Success {
		t.Errorf("RecentRequests[0] should be failed")
	}

	// 3. 验证按日聚合包含 CreditSum
	if len(snap2.ByDay) == 0 {
		t.Fatalf("ByDay should not be empty")
	}
	if snap2.ByDay[0].View.CreditSum != 0.2 {
		t.Errorf("ByDay[0].View.CreditSum = %v, want 0.2", snap2.ByDay[0].View.CreditSum)
	}
}
