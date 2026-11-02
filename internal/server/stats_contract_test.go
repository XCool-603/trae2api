package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"trae2api/internal/metrics"
	"trae2api/internal/pool"
)

// TestStatsResponseShape 锁住 /v1/stats 的字段名与类型。
//
// 为什么需要它：这个接口有**独立的消费者**（cmd/stats 仪表盘、外部脚本），
// 它们各自声明结构体。字段名或类型一变，消费者那边就是 JSON 解析失败 ——
// 而 Go 的类型系统在**跨进程**边界上帮不上忙。
//
// 实际踩过：服务端 `pool.servable` 返回 bool，而 cmd/stats 里声明成 `int`，
// 结果整个仪表盘打不开，报的是"cannot unmarshal bool into ... of type int"。
// 所以这里把每个字段的**类型**也断言一遍，而不只是断言存在。
func TestStatsResponseShape(t *testing.T) {
	p := pool.New(pool.Options{MaxInFlight: 1})
	p.Add(&pool.Account{UID: "a1", Realm: "trae", Label: "L", Owner: "public"})
	p.Add(&pool.Account{UID: "a2", Realm: "solo", Label: "L2", Owner: "public"})

	mc := metrics.New(metrics.Options{RetainDays: 14})
	mc.Record(metrics.Sample{
		Model: "trae:glm-5.2", Account: "a1", Realm: "trae",
		Streaming: true, Success: true,
		TTFBMS: 120, LatencyMS: 900, HasUsage: true,
		PromptTokens: 100, CompletionTokens: 50,
		CacheHit: 10, CacheMiss: 5, CacheWrite: 2,
	})

	// handleStats 不需要 cfg/users/sessions —— 只依赖 pool 与 metrics
	s := &Server{pool: p, metrics: mc, channels: map[string]*Channel{}}

	req := httptest.NewRequest(http.MethodGet, "/v1/stats", nil)
	w := httptest.NewRecorder()
	s.handleStats(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}

	// 用 map[string]any 解，才能检查每个字段的**实际 JSON 类型**
	var top map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
	for _, k := range []string{"service", "version", "stats", "pool"} {
		if _, ok := top[k]; !ok {
			t.Errorf("顶层缺字段 %q", k)
		}
	}

	// ---- pool 子对象 ----
	poolObj, ok := top["pool"].(map[string]any)
	if !ok {
		t.Fatalf("pool 不是对象: %T", top["pool"])
	}
	// 这两个字段的类型是最容易搞错的地方，显式断言
	if _, ok := poolObj["total"].(float64); !ok {
		t.Errorf("pool.total 应为数字，实际 %T", poolObj["total"])
	}
	if _, ok := poolObj["servable"].(bool); !ok {
		t.Errorf("pool.servable 应为布尔，实际 %T（消费者若声明成 int 会解析失败）", poolObj["servable"])
	}
	if _, ok := poolObj["servable_count"].(float64); !ok {
		t.Errorf("pool.servable_count 应为数字，实际 %T", poolObj["servable_count"])
	}

	// ---- stats.global ----
	statsObj, ok := top["stats"].(map[string]any)
	if !ok {
		t.Fatalf("stats 不是对象: %T", top["stats"])
	}
	glob, ok := statsObj["global"].(map[string]any)
	if !ok {
		t.Fatalf("stats.global 不是对象")
	}
	numFields := []string{
		"requests", "success", "failed", "streaming",
		"avg_ttfb_ms", "avg_latency_ms", "usage_count",
		"prompt_tokens", "completion_tokens", "total_tokens",
		"cache_hit", "cache_miss", "cache_write", "cache_hit_rate_percent",
		"credit_count", "avg_credits_x1000",
	}
	for _, k := range numFields {
		v, ok := glob[k]
		if !ok {
			t.Errorf("stats.global 缺字段 %q", k)
			continue
		}
		if _, ok := v.(float64); !ok {
			t.Errorf("stats.global.%s 应为数字，实际 %T", k, v)
		}
	}
	if _, ok := glob["credit_sum"].(float64); !ok {
		t.Errorf("stats.global.credit_sum 应为数字，实际 %T", glob["credit_sum"])
	}
	if _, ok := glob["last_seen"].(string); !ok {
		t.Errorf("stats.global.last_seen 应为字符串，实际 %T", glob["last_seen"])
	}

	// 哨兵必须存在且为 -1（消费者靠它区分"无观测"与"是 0"）
	sentinel, ok := statsObj["no_observation_sentinel"].(float64)
	if !ok || int64(sentinel) != metrics.NoObservation {
		t.Errorf("no_observation_sentinel 应为 %d，实际 %v", metrics.NoObservation, statsObj["no_observation_sentinel"])
	}

	// ---- by_model 的每个 View 也要形状一致 ----
	byModel, ok := statsObj["by_model"].(map[string]any)
	if !ok {
		t.Fatalf("stats.by_model 不是对象")
	}
	if len(byModel) == 0 {
		t.Fatal("by_model 为空（应该记了一条）")
	}
	for name, v := range byModel {
		view, ok := v.(map[string]any)
		if !ok {
			t.Errorf("by_model[%s] 不是对象", name)
			continue
		}
		for _, k := range []string{"requests", "success", "failed", "avg_ttfb_ms",
			"avg_latency_ms", "usage_count", "total_tokens", "cache_hit_rate_percent"} {
			if _, ok := view[k].(float64); !ok {
				t.Errorf("by_model[%s].%s 应为数字，实际 %T", name, k, view[k])
			}
		}
	}

	// ---- by_day 是数组，每项含 day 与 view ----
	byDay, ok := statsObj["by_day"].([]any)
	if !ok {
		t.Fatalf("stats.by_day 不是数组: %T", statsObj["by_day"])
	}
	if len(byDay) == 0 {
		t.Fatal("by_day 为空")
	}
	first, _ := byDay[0].(map[string]any)
	if _, ok := first["day"].(string); !ok {
		t.Errorf("by_day[].day 应为字符串，实际 %T", first["day"])
	}
	if _, ok := first["view"].(map[string]any); !ok {
		t.Errorf("by_day[].view 应为对象，实际 %T", first["view"])
	}

	// ---- recent_requests 是数组 ----
	recent, ok := statsObj["recent_requests"].([]any)
	if !ok {
		t.Fatalf("stats.recent_requests 不是数组: %T", statsObj["recent_requests"])
	}
	if len(recent) == 0 {
		t.Fatal("recent_requests 为空")
	}
}

// TestStatsDisabledMetrics 指标关闭时 /v1/stats 应给出明确错误而不是空对象。
func TestStatsDisabledMetrics(t *testing.T) {
	s := &Server{pool: pool.New(pool.Options{MaxInFlight: 1}), metrics: nil,
		channels: map[string]*Channel{}}
	req := httptest.NewRequest(http.MethodGet, "/v1/stats", nil)
	w := httptest.NewRecorder()
	s.handleStats(w, req)
	if w.Code == http.StatusOK {
		t.Error("指标为 nil 时不应返回 200")
	}
	var env map[string]any
	json.Unmarshal(w.Body.Bytes(), &env)
	if _, ok := env["error"]; !ok {
		t.Errorf("应返回错误信封: %s", w.Body.String())
	}
}
