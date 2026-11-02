// Command stats 是网关指标的终端仪表盘。
//
// 用法：
//
//	stats                       # 交互式仪表盘（q 退出，r 立即刷新）
//	stats -once                 # 只渲染一帧就退出（便于脚本/日志）
//	stats -interval 5s          # 自定义刷新间隔
//	stats -models 15            # 模型表显示多少行
//
// 设计要点（来自对 workbuddy2api cmd/stats 的复盘）：
//
//  1. **三层防御渲染**：帧裁进窗口 → 裁完仍不 fit 就退回滚动模式（不清屏）→
//     尺寸变化时清屏重建基线。只做第一层的话，窗口一缩小就会出现
//     "半截表格 + 光标乱跳"。
//  2. **清屏用 ED(0) 而非 ED(2)**：Windows conhost 收到 2J 会把屏幕内容滚入
//     scrollback，重绘几次就叠好几份表（见 term_windows.go）。
//  3. **非 TTY 时只渲染一帧**：被重定向到文件时不能进入交互循环，
//     否则脚本会永远挂住。
package stats

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"trae2api/internal/cli"
)

// Run 执行 stats 子命令，返回退出码（由总入口 os.Exit）。
func Run(args []string) int {
	return cli.RunArgs(func() error { return run(args) })
}

type options struct {
	base     string
	key      string
	interval time.Duration
	once     bool
	maxModel int
	maxDays  int
}

func run(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	base, key := cli.GatewayFlags(fs)
	interval := fs.Duration("interval", 2*time.Second, "刷新间隔")
	once := fs.Bool("once", false, "只渲染一帧就退出")
	maxModel := fs.Int("models", 12, "模型表最多显示几行")
	maxDays := fs.Int("days", 7, "按日趋势最多显示几行")
	if err := fs.Parse(args); err != nil {
		return cli.Usagef("%v", err)
	}
	opt := options{base: *base, key: *key, interval: *interval, once: *once,
		maxModel: *maxModel, maxDays: *maxDays}

	isTTY := isTerminal()
	if opt.once || !isTTY {
		// 非交互：渲染一帧纯文本（不带 ANSI，便于写进日志）
		frame, err := buildFrame(opt, 0)
		if err != nil {
			return err
		}
		fmt.Println(strings.Join(frame, "\n"))
		return nil
	}
	return loop(opt)
}

// isTerminal 判断 stdout 是否是终端。
func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// loop 是交互主循环。
func loop(opt options) error {
	enableVT()
	// 按键监听：q 退出，r 立即刷新
	keys := make(chan byte, 8)
	go func() {
		r := bufio.NewReader(os.Stdin)
		for {
			b, err := r.ReadByte()
			if err != nil {
				close(keys)
				return
			}
			keys <- b
		}
	}()

	ticker := time.NewTicker(opt.interval)
	defer ticker.Stop()

	lastCols, lastRows := -1, -1
	firstPaint := true
	refresh := make(chan struct{}, 1)

	for {
		cols, rows, ok := termSize()
		if !ok {
			cols, rows = 100, 30 // 取不到就假定一个合理值
		}
		sizeChanged := cols != lastCols || rows != lastRows

		frame, err := buildFrame(opt, cols)
		if err != nil {
			// 网关暂时不可达时不要把屏幕清成空白：保留上一帧，
			// 只在底部提示，让用户看到"最后一次成功的数据 + 当前错误"。
			frame = []string{
				fmt.Sprintf("⚠️  %v", err),
				"",
				"（保留上一帧；网关恢复后会自动继续刷新。Ctrl+C 退出）",
			}
		}

		// 第 1 层：把帧裁进窗口
		fit := len(frame) <= rows
		if fit {
			// 第 3 层：尺寸变化时清屏重建基线（否则旧内容残留在右侧/下方）
			if sizeChanged && !firstPaint {
				clearScreen()
			}
			if firstPaint {
				clearScreen()
			}
			// 第 1 层续：行内容按列宽截断（宽字符按 2 列算，见 cli.DisplayWidth）
			out := make([]string, len(frame))
			for i, ln := range frame {
				out[i] = cli.Truncate(ln, cols)
			}
			print("\x1b[H") // 光标归位
			fmt.Print(strings.Join(out, "\n"))
			// 清掉本帧之后的残留行（用 EL 清行，不动 scrollback）
			fmt.Print("\x1b[0J")
			firstPaint = false
		} else {
			// 第 2 层：帧放不下 → 退回滚动模式（不清屏、不定位光标）
			// 这样内容会自然向上滚，用户仍能滚动回看，而不是看到被切一半的表。
			fmt.Printf("\n--- %s（窗口 %dx%d 放不下 %d 行，已切滚动模式）---\n",
				time.Now().Format("15:04:05"), cols, rows, len(frame))
			fmt.Println(strings.Join(frame, "\n"))
			firstPaint = true // 下次能放下时重新清屏
		}

		lastCols, lastRows = cols, rows

		select {
		case b, ok := <-keys:
			if !ok {
				return nil
			}
			switch b {
			case 'q', 'Q', 3: // 3 = Ctrl+C
				fmt.Print("\x1b[0J")
				fmt.Println("已退出。")
				return nil
			case 'r', 'R':
				// 立即刷新
			}
		case <-refresh:
		case <-ticker.C:
		}
	}
}

// ---- 数据获取与帧构建 ----

// statsResp 对应 /v1/stats。
type statsResp struct {
	Stats struct {
		Global struct {
			Requests     int64   `json:"requests"`
			Success      int64   `json:"success"`
			Failed       int64   `json:"failed"`
			Streaming    int64   `json:"streaming"`
			AvgTTFBMS    int64   `json:"avg_ttfb_ms"`
			AvgLatencyMS int64   `json:"avg_latency_ms"`
			UsageCount   int64   `json:"usage_count"`
			TotalTokens  int64   `json:"total_tokens"`
			CacheHit     int64   `json:"cache_hit"`
			CacheMiss    int64   `json:"cache_miss"`
			CacheHitRate int64   `json:"cache_hit_rate_percent"`
			CreditSum    float64 `json:"credit_sum"`
		} `json:"global"`
		ByModel map[string]modelView `json:"by_model"`
		ByRealm map[string]modelView `json:"by_realm"`
		ByDay   []struct {
			Day  string    `json:"day"`
			View modelView `json:"view"`
		} `json:"by_day"`
		RetainDays    int   `json:"retain_days"`
		NoObservation int64 `json:"no_observation_sentinel"`
	} `json:"stats"`
	Pool struct {
		Total int `json:"total"`
		// Servable 是布尔（"有没有可调度的账号"），ServableCount 才是数量。
		// ⚠️ 这里曾经写成 `Servable int`，而 API 返回的是 bool ——
		// JSON 解析直接失败，整个仪表盘打不开。接口契约要靠测试锁住。
		Servable      bool `json:"servable"`
		ServableCount int  `json:"servable_count"`
	} `json:"pool"`
}

type modelView struct {
	Requests     int64   `json:"requests"`
	Success      int64   `json:"success"`
	Failed       int64   `json:"failed"`
	Streaming    int64   `json:"streaming"`
	AvgTTFBMS    int64   `json:"avg_ttfb_ms"`
	AvgLatencyMS int64   `json:"avg_latency_ms"`
	UsageCount   int64   `json:"usage_count"`
	TotalTokens  int64   `json:"total_tokens"`
	CacheHitRate int64   `json:"cache_hit_rate_percent"`
	CreditSum    float64 `json:"credit_sum"`
}

// statusResp 对应 /status（只取仪表盘要显示的字段）。
type statusResp struct {
	Version     string `json:"version"`
	UptimeSec   int    `json:"uptime_seconds"`
	Total       int    `json:"total"`
	Healthy     int    `json:"healthy"`
	Cooling     int    `json:"cooling"`
	Disabled    int    `json:"disabled"`
	Users       int    `json:"users"`
	CheckinOn   bool   `json:"checkin_enabled"`
	CheckinHour []int  `json:"checkin_hours"`
	WAFGate     struct {
		Active       bool   `json:"active"`
		DistinctUIDs int    `json:"distinct_uids"`
		MinDistinct  int    `json:"min_distinct_uids"`
		Activations  int64  `json:"activations_total"`
		LastReason   string `json:"last_reason"`
	} `json:"waf_ip_gate"`
}

// noObs 是"无观测"哨兵（-1）。渲染成 "—" 而不是 0。
const noObs = -1

func buildFrame(opt options, cols int) ([]string, error) {
	c := cli.NewClient(opt.base, opt.key)
	var st statsResp
	if err := c.Get("/v1/stats", &st); err != nil {
		return nil, err
	}
	var status statusResp
	// /status 失败不算致命（指标仍可显示）
	_ = c.Get("/status", &status)

	var out []string
	now := time.Now().Format("2006-01-02 15:04:05")

	// ---- 头部 ----
	out = append(out, strings.Repeat("=", 72))
	out = append(out, fmt.Sprintf("  Trae2Api %s   指标仪表盘   %s", status.Version, now))
	out = append(out, fmt.Sprintf("  网关 %s   运行 %s   刷新间隔 %s",
		opt.base, cli.Duration(status.UptimeSec), opt.interval))
	out = append(out, strings.Repeat("=", 72))

	// ---- 账号池 ----
	poolLine := fmt.Sprintf("  账号 %d（可调度 %d）   健康 %d   冷却 %d   停用 %d   用户 %d",
		status.Total, st.Pool.ServableCount, status.Healthy, status.Cooling, status.Disabled, status.Users)
	out = append(out, poolLine)
	if status.CheckinOn {
		out = append(out, fmt.Sprintf("  自动签到 已启用（每日 %v 点）", status.CheckinHour))
	}
	if status.WAFGate.Active {
		out = append(out, fmt.Sprintf("  ⚠️ WAF IP 级闸【已激活】不同账号 %d/%d   累计 %d 次   %s",
			status.WAFGate.DistinctUIDs, status.WAFGate.MinDistinct,
			status.WAFGate.Activations, cli.Truncate(status.WAFGate.LastReason, 30)))
	} else {
		out = append(out, fmt.Sprintf("  WAF IP 级闸 未激活（窗口内不同账号 %d/%d）",
			status.WAFGate.DistinctUIDs, status.WAFGate.MinDistinct))
	}
	out = append(out, "")

	// ---- 全局指标 ----
	g := st.Stats.Global
	out = append(out, "  ── 全局指标 ──")
	out = append(out, fmt.Sprintf("  请求 %d（成功 %d / 失败 %d，流式 %d）",
		g.Requests, g.Success, g.Failed, g.Streaming))
	// 关键：usage_count 与 requests 的差额就是"缺观测"的量。
	// 把它显式打出来，用户才知道均值可不可信。
	missNote := ""
	if g.Requests > g.UsageCount {
		missNote = fmt.Sprintf("（其中 %d 次上游未返回 usage）", g.Requests-g.UsageCount)
	}
	out = append(out, fmt.Sprintf("  有 usage 观测 %d%s", g.UsageCount, missNote))
	out = append(out, fmt.Sprintf("  TTFB 均值 %s   耗时均值 %s",
		fmtSentinel(g.AvgTTFBMS, "ms"), fmtSentinel(g.AvgLatencyMS, "ms")))
	out = append(out, fmt.Sprintf("  累计 token %d   缓存命中率 %s（hit %d / miss %d）",
		g.TotalTokens, fmtSentinel(g.CacheHitRate, "%"), g.CacheHit, g.CacheMiss))
	out = append(out, fmt.Sprintf("  累计积分消耗 %.2f", g.CreditSum))
	out = append(out, "")

	// ---- 分模型 ----
	if len(st.Stats.ByModel) > 0 {
		out = append(out, "  ── 分模型 ──")
		type kv struct {
			k string
			v modelView
		}
		var list []kv
		for k, v := range st.Stats.ByModel {
			list = append(list, kv{k, v})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].v.Requests != list[j].v.Requests {
				return list[i].v.Requests > list[j].v.Requests
			}
			return list[i].k < list[j].k
		})
		t := &cli.Table{Headers: []string{"模型", "积分", "请求", "成功/失败", "TTFB", "耗时", "token", "命中率"}}
		for i, e := range list {
			if opt.maxModel > 0 && i >= opt.maxModel {
				out = append(out, fmt.Sprintf("  … 另有 %d 个模型未显示（-models 调整）", len(list)-i))
				break
			}
			t.Add(e.k, fmt.Sprintf("%.2f", e.v.CreditSum),
				fmt.Sprintf("%d", e.v.Requests),
				fmt.Sprintf("%d/%d", e.v.Success, e.v.Failed),
				fmtSentinel(e.v.AvgTTFBMS, "ms"),
				fmtSentinel(e.v.AvgLatencyMS, "ms"),
				fmt.Sprintf("%d", e.v.TotalTokens),
				fmtSentinel(e.v.CacheHitRate, "%"))
		}
		for _, ln := range strings.Split(strings.TrimRight(t.Render(), "\n"), "\n") {
			out = append(out, "  "+ln)
		}
		out = append(out, "")
	}

	// ---- 按日趋势 ----
	if len(st.Stats.ByDay) > 0 {
		out = append(out, fmt.Sprintf("  ── 按日趋势（保留 %d 天）──", st.Stats.RetainDays))
		days := st.Stats.ByDay
		if opt.maxDays > 0 && len(days) > opt.maxDays {
			days = days[len(days)-opt.maxDays:]
		}
		t := &cli.Table{Headers: []string{"日期", "积分", "请求", "成功", "失败", "耗时", "token", "命中率"}}
		for _, d := range days {
			t.Add(d.Day, fmt.Sprintf("%.2f", d.View.CreditSum),
				fmt.Sprintf("%d", d.View.Requests),
				fmt.Sprintf("%d", d.View.Success), fmt.Sprintf("%d", d.View.Failed),
				fmtSentinel(d.View.AvgLatencyMS, "ms"),
				fmt.Sprintf("%d", d.View.TotalTokens),
				fmtSentinel(d.View.CacheHitRate, "%"))
		}
		for _, ln := range strings.Split(strings.TrimRight(t.Render(), "\n"), "\n") {
			out = append(out, "  "+ln)
		}
		out = append(out, "")
	}

	// ---- 底部 ----
	out = append(out, strings.Repeat("-", 72))
	out = append(out, "  q 退出   r 立即刷新   「—」= 该项无观测（不是 0）")
	return out, nil
}

// fmtSentinel 把哨兵值渲染成 "—"。
//
// 这是本仪表盘最重要的一条显示纪律：**"没采到"与"是 0"必须能区分**。
// 直接打印 -1ms 会让人困惑，打印 0ms 则是撒谎。
func fmtSentinel(v int64, unit string) string {
	if v == noObs || v < 0 {
		return "—"
	}
	return fmt.Sprintf("%d%s", v, unit)
}
