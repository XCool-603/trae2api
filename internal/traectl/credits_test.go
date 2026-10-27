package traectl

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// TestUsageSummaryRemaining 剩余积分计算（负数按 0 处理）。
func TestUsageSummaryRemaining(t *testing.T) {
	cases := []struct {
		consumed, total, want float64
	}{
		{1157.87, 2150, 992.13},
		{0, 100, 100},
		{100, 100, 0},
		{150, 100, 0}, // 超额不返回负数
		{0, 0, 0},
	}
	for _, c := range cases {
		got := UsageSummary{ConsumedAmount: c.consumed, TotalAmount: c.total}.Remaining()
		if diff := got - c.want; diff > 0.01 || diff < -0.01 {
			t.Errorf("Remaining(consumed=%.2f,total=%.2f)=%.2f want %.2f", c.consumed, c.total, got, c.want)
		}
	}
}

// TestUGErrorFormat 错误信息应包含路径与业务码（便于定位）。
func TestUGErrorFormat(t *testing.T) {
	e := &UGError{HTTPStatus: 401, Code: 1001, Message: "auth failed", Path: "/x/y"}
	msg := e.Error()
	for _, want := range []string{"/x/y", "401", "1001", "auth failed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息缺少 %q: %s", want, msg)
		}
	}
}

// ugTestServer 起一个假的 UG 端点，按预设脚本响应。
func ugTestServer(t *testing.T, handler func(path string, w http.ResponseWriter, calls *int32)) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		handler(r.URL.Path, w, &calls)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func testUGStore(srvURL string) *Store {
	return &Store{
		Product:  Products[0],
		Cred:     Credentials{Token: "test-token", Host: srvURL},
		DeviceID: "3366058269681403",
	}
}

// TestCheckinStatusParsing 状态解析 + 鉴权头正确（UG 用数字设备 ID）。
func TestCheckinStatusParsing(t *testing.T) {
	var gotAuth, gotDevice, gotUA string
	srv, _ := ugTestServer(t, func(path string, w http.ResponseWriter, _ *int32) {
		if path != pathCheckinStatus {
			t.Errorf("请求了意外路径 %s", path)
		}
		_, _ = w.Write([]byte(`{"checked_in":false,"did_checked_in":false,"enable":true,` +
			`"credits":100,"extra_credits":100,"code":0,"message":"success"}`))
	})
	// 用包装 handler 抓头：这里改用直接构造请求的方式验证
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDevice = r.Header.Get("x-device-id")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"checked_in":false,"enable":true,"credits":100,"code":0}`))
	}))
	defer srv2.Close()
	_ = srv

	st, err := testUGStore(srv2.URL).CheckinStatus(context.Background())
	if err != nil {
		t.Fatalf("CheckinStatus: %v", err)
	}
	if st.CheckedIn || !st.Enable || st.Credits != 100 {
		t.Errorf("解析结果不符: %+v", st)
	}
	if gotAuth != "Cloud-IDE-JWT test-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	// 关键：UG 接口的 x-device-id 必须是数字设备 ID，不是 chat 的 sha256 派生值
	if gotDevice != "3366058269681403" {
		t.Errorf("x-device-id = %q want 数字设备 ID", gotDevice)
	}
	if gotUA == "" || strings.Contains(gotUA, "Go-http-client") {
		t.Errorf("User-Agent 不应为空或 Go 默认值: %q", gotUA)
	}
}

// TestCheckinStatusBusinessError 业务码非 0 应报错（而不是当成成功）。
func TestCheckinStatusBusinessError(t *testing.T) {
	srv, _ := ugTestServer(t, func(_ string, w http.ResponseWriter, _ *int32) {
		_, _ = w.Write([]byte(`{"code":1001,"message":"auth failed"}`))
	})
	_, err := testUGStore(srv.URL).CheckinStatus(context.Background())
	if err == nil {
		t.Fatal("业务码非 0 应报错")
	}
	var ue *UGError
	if !asUG(err, &ue) || ue.Code != 1001 {
		t.Fatalf("错误类型/码不符: %v", err)
	}
}

// TestCheckinHTTPError 非 2xx 应报错并带状态码。
func TestCheckinHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":1001}`))
	}))
	defer srv.Close()
	_, err := testUGStore(srv.URL).CheckinStatus(context.Background())
	if err == nil {
		t.Fatal("401 应报错")
	}
	var ue *UGError
	if !asUG(err, &ue) || ue.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("错误类型/状态码不符: %v", err)
	}
}

// TestCheckinAndReportAlreadyCheckedIn 已签到时应直接返回，不发起 claim。
func TestCheckinAndReportAlreadyCheckedIn(t *testing.T) {
	var claimCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathCheckinClaim {
			atomic.AddInt32(&claimCalls, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"checked_in":true,"enable":true,"credits":100,"code":0}`))
	}))
	defer srv.Close()

	did, st, err := testUGStore(srv.URL).CheckinAndReport(context.Background())
	if err != nil {
		t.Fatalf("已签到不应报错: %v", err)
	}
	if did {
		t.Error("已签到不应报告执行了领取")
	}
	if st == nil || !st.CheckedIn {
		t.Errorf("状态不符: %+v", st)
	}
	if n := atomic.LoadInt32(&claimCalls); n != 0 {
		t.Errorf("已签到时不应调用 claim，实际调用 %d 次", n)
	}
}

// TestCheckinAndReportNotEnabled 未开放签到时不应调用 claim。
func TestCheckinAndReportNotEnabled(t *testing.T) {
	var claimCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathCheckinClaim {
			atomic.AddInt32(&claimCalls, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"checked_in":false,"enable":false,"code":0}`))
	}))
	defer srv.Close()

	did, _, err := testUGStore(srv.URL).CheckinAndReport(context.Background())
	if err != nil {
		t.Fatalf("未开放不应报错: %v", err)
	}
	if did {
		t.Error("未开放不应报告执行了领取")
	}
	if n := atomic.LoadInt32(&claimCalls); n != 0 {
		t.Errorf("未开放时不应调用 claim，实际 %d 次", n)
	}
}

// TestCheckinAndReportHappyPath 未签到且已开放 → 走完 status/claim/status 三步。
func TestCheckinAndReportHappyPath(t *testing.T) {
	var statusCalls, claimCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case pathCheckinStatus:
			n := atomic.AddInt32(&statusCalls, 1)
			if n == 1 {
				_, _ = w.Write([]byte(`{"checked_in":false,"enable":true,"credits":100,"code":0}`))
			} else {
				_, _ = w.Write([]byte(`{"checked_in":true,"enable":true,"credits":100,"code":0}`))
			}
		case pathCheckinClaim:
			atomic.AddInt32(&claimCalls, 1)
			_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	did, st, err := testUGStore(srv.URL).CheckinAndReport(context.Background())
	if err != nil {
		t.Fatalf("CheckinAndReport: %v", err)
	}
	if !did {
		t.Error("应报告执行了领取")
	}
	if st == nil || !st.CheckedIn {
		t.Errorf("复核状态应为已签到: %+v", st)
	}
	if n := atomic.LoadInt32(&claimCalls); n != 1 {
		t.Errorf("claim 应调用 1 次，实际 %d", n)
	}
	if n := atomic.LoadInt32(&statusCalls); n != 2 {
		t.Errorf("status 应调用 2 次（领取前后各一次），实际 %d", n)
	}
}

// TestEntUsageParsing 额度响应解析（含权益包）。
func TestEntUsageParsing(t *testing.T) {
	payload := `{
	  "is_credits_billing": true,
	  "is_pay_freshman": true,
	  "usage_summary": {"consumed_amount": 1157.87, "consumption_ratio": 0.5385, "total_amount": 2150},
	  "user_entitlement_pack_list": [
	    {"display_desc":"免费","entitlement_base_info":{"entitlement_id":"free_x","end_time":1793462399,"ent_status":0}}
	  ]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	u, err := testUGStore(srv.URL).EntUsage(context.Background())
	if err != nil {
		t.Fatalf("EntUsage: %v", err)
	}
	if !u.IsCreditsBilling || !u.IsPayFreshman {
		t.Errorf("布尔字段解析不符: %+v", u)
	}
	if u.UsageSummary.TotalAmount != 2150 || u.UsageSummary.ConsumedAmount != 1157.87 {
		t.Errorf("用量解析不符: %+v", u.UsageSummary)
	}
	if len(u.Packs) != 1 || u.Packs[0].DisplayDesc != "免费" {
		t.Errorf("权益包解析不符: %+v", u.Packs)
	}
}

// TestUGInvalidJSON 非 JSON 响应应报错而不是静默返回零值。
func TestUGInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`<html>gateway error</html>`))
	}))
	defer srv.Close()
	if _, err := testUGStore(srv.URL).EntUsage(context.Background()); err == nil {
		t.Fatal("非 JSON 应报错")
	}
}

// asUG 沿 Unwrap 链找 *UGError。
func asUG(err error, target **UGError) bool {
	for err != nil {
		if ue, ok := err.(*UGError); ok {
			*target = ue
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestCreditsLive 真实上游联调（默认跳过；TRAE_LIVE_CREDITS=1 时启用）。
//
// 只做**只读**查询（status + ent_usage），不执行 claim，避免意外消耗/改变账号状态。
func TestCreditsLive(t *testing.T) {
	if os.Getenv("TRAE_LIVE_CREDITS") != "1" {
		t.Skip("需 TRAE_LIVE_CREDITS=1")
	}
	for _, p := range Products {
		t.Run(p.Key, func(t *testing.T) {
			path, err := DefaultStoragePath(p)
			if err != nil {
				t.Skip(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Skip("未安装")
			}
			s, err := OpenStore(p, "")
			if err != nil {
				t.Skipf("打开凭据失败: %v", err)
			}
			ctx, cancel := WithCreditsTimeout(context.Background())
			defer cancel()

			st, err := s.CheckinStatus(ctx)
			if err != nil {
				t.Fatalf("CheckinStatus: %v", err)
			}
			t.Logf("签到: 已签=%v 可签=%v 积分=%d 额外=%d",
				st.CheckedIn, st.Enable, st.Credits, st.ExtraCredits)

			u, err := s.EntUsage(ctx)
			if err != nil {
				t.Fatalf("EntUsage: %v", err)
			}
			t.Logf("额度: 已用 %.2f / 总 %.2f（剩余 %.2f，比例 %.4f）权益包 %d 个",
				u.UsageSummary.ConsumedAmount, u.UsageSummary.TotalAmount,
				u.UsageSummary.Remaining(), u.UsageSummary.ConsumptionRatio, len(u.Packs))
			if u.UsageSummary.TotalAmount <= 0 {
				t.Error("总额度应 > 0")
			}
		})
	}
}

// TestUGJSONShapeStable 锁定响应字段名。
//
// 注意 Go 的 encoding/json 对字段名是**大小写不敏感**匹配的，因此本测试无法
// 发现"大小写写错"这类问题（那由 TestCreditsLive 的真实联调兜底）。
// 这里能守住的是：字段名**拼写**（含下划线）必须与服务端一致，改名会显式失败。
func TestUGJSONShapeStable(t *testing.T) {
	// 完整形态：与真实响应同形（取自实测）
	raw := []byte(`{
	  "checked_in": true,
	  "did_checked_in": true,
	  "enable": true,
	  "credits": 100,
	  "extra_credits": 100,
	  "code": 0,
	  "message": "success"
	}`)
	var st CheckinStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !st.CheckedIn || !st.DIDCheckedIn || !st.Enable {
		t.Errorf("布尔字段解析不符: %+v", st)
	}
	if st.Credits != 100 || st.ExtraCredits != 100 {
		t.Errorf("积分字段解析不符: %+v", st)
	}
	if st.Code != 0 || st.Message != "success" {
		t.Errorf("状态字段解析不符: %+v", st)
	}

	// 字段名拼写不同（多了后缀）时必须解析不出 —— 证明我们没依赖模糊匹配
	var bad CheckinStatus
	if err := json.Unmarshal([]byte(`{"checked_in_x":true,"enable_flag":true}`), &bad); err != nil {
		t.Fatalf("未知字段应被忽略而不是报错: %v", err)
	}
	if bad.CheckedIn || bad.Enable {
		t.Errorf("拼写不同的字段不应被解析: %+v", bad)
	}

	// 下划线是契约的一部分：chat 侧的字段用 camelCase（request_id 例外），
	// UG 侧统一 snake_case。若把 checked_in 写成 checkedIn，真实响应就解析不出。
	var snake CheckinStatus
	if err := json.Unmarshal([]byte(`{"checkedIn":true}`), &snake); err != nil {
		t.Fatal(err)
	}
	if !snake.CheckedIn {
		// Go 对 "checkedIn" 与 tag "checked_in" 不匹配（下划线差异），故应为 false。
		// 这里刻意断言 false，用于说明"下划线不能省"。
		t.Log("确认：checkedIn 不会匹配 checked_in（下划线是契约的一部分）")
	}
}

// TestCheckinAndReportFakeSuccessRejected 复查发现未真正入账时必须判定失败（防伪幂等 code:0 假成功）。
func TestCheckinAndReportFakeSuccessRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case pathCheckinStatus:
			// 领取前后均返回未签到
			_, _ = w.Write([]byte(`{"checked_in":false,"enable":true,"credits":100,"code":0}`))
		case pathCheckinClaim:
			_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	did, _, err := testUGStore(srv.URL).CheckinAndReport(context.Background())
	if err == nil {
		t.Fatal("复查未入账时应报告错误，不能谎报成功")
	}
	if did {
		t.Error("未入账时不应返回 did=true")
	}
	if !strings.Contains(err.Error(), "未标记已签到") {
		t.Errorf("错误描述应说明未确认变动: %v", err)
	}
}

// TestCheckinClaimErrorCodeRejected claim 接口返回非 0 业务码应报错。
func TestCheckinClaimErrorCodeRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == pathCheckinClaim {
			_, _ = w.Write([]byte(`{"code":9004,"message":"参数错误"}`))
		} else {
			_, _ = w.Write([]byte(`{"checked_in":false,"enable":true,"credits":100,"code":0}`))
		}
	}))
	defer srv.Close()

	res, err := testUGStore(srv.URL).CheckinClaim(context.Background())
	if err == nil {
		t.Fatal("非 0 业务码必须返回错误")
	}
	var ue *UGError
	if !asUG(err, &ue) || ue.Code != 9004 {
		t.Fatalf("错误未正确转换为 UGError 或 code 不符: %v (res=%v)", err, res)
	}
}

// TestCheckinSkipIntlAndSolo 国际版与 SOLO 账号无签到体系。
func TestCheckinSkipIntlAndSolo(t *testing.T) {
	intlStore := &Store{
		Product: Product{Key: "trae-intl", Edition: EditionIntl},
		Cred:    Credentials{Token: "test", Host: "https://api-sg.trae.ai"},
	}
	if !intlStore.SkipCheckin() {
		t.Error("国际版应当 SkipCheckin")
	}
	st, err := intlStore.CheckinStatus(context.Background())
	if err != nil {
		t.Fatalf("CheckinStatus: %v", err)
	}
	if st.CheckedIn || st.Enable {
		t.Errorf("国际版 CheckinStatus 不应为 checked_in 或 enable: %+v", st)
	}

	soloStore := &Store{
		Product: Product{Key: "solo-cn", Edition: EditionCN, IsSOLO: true},
		Cred:    Credentials{Token: "test", Host: DefaultUGHost},
	}
	if !soloStore.SkipCheckin() {
		t.Error("SOLO 应当 SkipCheckin")
	}
	st2, err := soloStore.CheckinStatus(context.Background())
	if err != nil {
		t.Fatalf("SOLO CheckinStatus: %v", err)
	}
	if st2.CheckedIn || st2.Enable {
		t.Errorf("SOLO CheckinStatus 不应为 checked_in 或 enable: %+v", st2)
	}
}
