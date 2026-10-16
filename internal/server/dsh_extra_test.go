package server

// 本文件是 dsh 扩展端点（install / reset / export）与系统更新检测的测试。
//
// 设计原则与 dsh_test.go 一致：**不依赖开发机上装没装 DeepSeek Harness / Node**。
// 需要"能启动"的场景注入假 dsh（withFakeDSH），需要"能不能启动"的判定直接测纯函数
// （dshInstallVerdict）。于是这些测试在任何机器上都有意义，而不是"只有作者机器能过"。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"trae2api/internal/config"
)

// dshExtraAdminKey 是"配了全局 Key"场景下用来通过管理员鉴权的值。
const dshExtraAdminKey = "secret-key"

// ---- 路由存在性 ----

// TestDSHExtraRoutesRegistered B2 的**路由级**回归防线。
//
// 上一轮验收抓到的问题正是"端点不存在"：POST /admin/dsh/install → 405、
// GET /admin/dsh/export → 404。这个测试把"端点必须存在且方法正确"钉在测试里，
// 而不是靠人去数 server.go 的行。
func TestDSHExtraRoutesRegistered(t *testing.T) {
	s := newDSHTestServer(t)
	h := s.Handler()

	// 1) 契约端点必须命中（不能是 404 / 405 = 路由没注册）
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/admin/dsh/install"},
		{http.MethodPost, "/admin/dsh/reset"},
		{http.MethodGet, "/admin/dsh/export"},
		{http.MethodGet, "/admin/system/update/check"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		if w.Code == http.StatusNotFound || w.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s %s 未注册（%d）—— 契约端点是前端面板的命根子",
				c.method, c.path, w.Code)
		}
		if w.Code != http.StatusOK {
			t.Errorf("%s %s = %d want 200：%s", c.method, c.path, w.Code, w.Body.String())
		}
	}

	// 2) 方法必须**精确**注册：用错方法绝不能命中 handler。
	//
	//    注意 GET 的落点是 404 而不是 405：mux 里注册了 `GET /`（公开门户，见
	//    server.go），它会把所有没有单独注册的 GET 路径接走，门户自己对未知路径
	//    回 404（见 TestHandlerUnknownRoute）。所以判据是"不是 200、且响应里没有
	//    该端点的契约字段"，而不是死盯 405 —— 后者会把门户的兜底误判成漏洞。
	for _, c := range []struct {
		method, path string
		marker       string // 该端点真的被执行时才会出现的字段
	}{
		{http.MethodGet, "/admin/dsh/install", "auto_download"},
		{http.MethodGet, "/admin/dsh/reset", "cleared"},
		{http.MethodPost, "/admin/dsh/export", "attachment"},
		{http.MethodPost, "/admin/system/update/check", "supported"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		if w.Code == http.StatusOK {
			t.Errorf("%s %s = 200 —— 方法不匹配却命中了 handler", c.method, c.path)
		}
		if w.Code != http.StatusMethodNotAllowed && w.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d want 404/405", c.method, c.path, w.Code)
		}
		if strings.Contains(w.Body.String(), c.marker) {
			t.Errorf("%s %s 用错方法却执行了 handler：%s", c.method, c.path, w.Body.String())
		}
	}
}

// ---- install ----

// TestDSHInstallDetectsButNeverDownloads 是 install 的核心断言。
//
// "不下载 Node"这件事没法直接测（它是一次**不发生**的网络行为），
// 所以测两件能观测的事：
//  1. install **不会起任何进程**（m.run 仍为 nil、成功启动计数仍为 0）——
//     "下载 + 运行可执行文件"的第一步就是起进程，进程数没变说明什么也没跑；
//  2. 响应里显式声明 auto_download.supported = false，并给出指引 ——
//     能力没做就写清楚（与 status 里 chat_forwarding 的写法一致）。
func TestDSHInstallDetectsButNeverDownloads(t *testing.T) {
	// 即使环境**完全就绪**（解析得出启动方式），install 也不许把 dsh 拉起来。
	withFakeDSH(t, "ok")
	s := newDSHTestServer(t)
	h := s.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/dsh/install", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("install = %d want 200：%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("install 响应不是合法 JSON: %v", err)
	}

	// 契约字段一个都不能少
	for _, k := range []string{"ok", "node", "desktop", "state", "hint"} {
		if _, ok := resp[k]; !ok {
			t.Errorf("install 缺少契约字段 %q：%v", k, resp)
		}
	}
	if resp["ok"] != true {
		t.Errorf("ok = %v want true（ok 表示「检测完成」，不表示环境齐全）", resp["ok"])
	}
	node, ok := resp["node"].(map[string]any)
	if !ok {
		t.Fatalf("node 结构不对：%v", resp["node"])
	}
	if _, ok := node["found"].(bool); !ok {
		t.Errorf("node.found 必须是布尔：%v", node)
	}
	desktop, ok := resp["desktop"].(map[string]any)
	if !ok {
		t.Fatalf("desktop 结构不对：%v", resp["desktop"])
	}
	if _, ok := desktop["found"].(bool); !ok {
		t.Errorf("desktop.found 必须是布尔：%v", desktop)
	}

	// state 必须是契约里的三个值之一，且与 launchable 自洽
	state, _ := resp["state"].(string)
	switch state {
	case dshEnvReady, dshEnvNodeMissing, dshEnvDesktopMissing:
	default:
		t.Errorf("state = %q 不在契约集合 {ready,node_missing,desktop_missing} 里", state)
	}
	launchable, _ := resp["launchable"].(bool)
	if launchable != (state == dshEnvReady) {
		t.Errorf("launchable=%v 与 state=%q 不自洽（能启动就必须是 ready）", launchable, state)
	}
	if state == dshEnvNodeMissing && node["found"] == true {
		t.Errorf("报了 node_missing 但 node.found=true：%v", node)
	}
	if hint, _ := resp["hint"].(string); strings.TrimSpace(hint) == "" {
		t.Error("hint 不能为空：用户看到「缺东西」时必须知道下一步做什么")
	}

	// auto_download 必须显式为 false
	ad, ok := resp["auto_download"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 auto_download 声明：%v", resp)
	}
	if ad["supported"] != false {
		t.Errorf("auto_download.supported 必须是 false，实际 %v", ad["supported"])
	}

	// 关键：没有起任何进程
	m := s.dshAgent()
	m.mu.Lock()
	run, starts := m.run, m.starts
	m.mu.Unlock()
	if run != nil {
		t.Errorf("install 把 dsh 进程拉起来了（pid=%d）—— 它必须只检测", run.pid)
	}
	if starts != 0 {
		t.Errorf("install 触发了 %d 次启动，必须为 0", starts)
	}
}

// TestDSHInstallVerdictStates 判定逻辑的纯函数测试（不依赖本机环境）。
func TestDSHInstallVerdictStates(t *testing.T) {
	nodeOK := dshNodeInfo{Found: true, Path: `C:\nvm4w\nodejs\node.exe`, Version: "v26.8.1"}
	nodeNo := dshNodeInfo{Found: false, Error: "PATH 上没有找到 node"}
	deskOK := dshDesktopInfo{Found: true, Path: `C:\x\DeepSeek Harness.exe`}
	deskNo := dshDesktopInfo{Error: "已检查 4 个常见安装目录，均未找到：..."}

	cases := []struct {
		name             string
		node             dshNodeInfo
		desktop          dshDesktopInfo
		launchable       bool
		launcher         string
		wantState        string
		wantHintContains []string
	}{
		{
			name: "node+桌面版都在 → ready", node: nodeOK, desktop: deskOK,
			launchable: true, launcher: "desktop",
			wantState: dshEnvReady, wantHintContains: []string{"环境就绪", "桌面版"},
		},
		{
			name: "都没有 → node_missing（node 是最底层那一环）", node: nodeNo, desktop: deskNo,
			launchable: false,
			wantState:  dshEnvNodeMissing,
			// 指引必须真的可执行：给出安装途径，而不是一句"缺 node"
			wantHintContains: []string{"nodejs.org", "不下载"},
		},
		{
			name: "有 node 但解析不出启动方式 → desktop_missing", node: nodeOK, desktop: deskNo,
			launchable:       false,
			wantState:        dshEnvDesktopMissing,
			wantHintContains: []string{"桌面版", "TRAE2API_DSH_CMD"},
		},
		{
			// 桌面版自带运行时：此时报 node_missing 会让人去装一个他不需要的 node
			name: "没有 node 但有桌面版 → ready", node: nodeNo, desktop: deskOK,
			launchable: true, launcher: "desktop",
			wantState: dshEnvReady, wantHintContains: []string{"自带运行时", "无需单独安装 node"},
		},
		{
			name: "环境变量指定 → ready", node: nodeNo, desktop: deskNo,
			launchable: true, launcher: "env",
			wantState: dshEnvReady, wantHintContains: []string{"TRAE2API_DSH_CMD"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state, hint := dshInstallVerdict(c.node, c.desktop, c.launchable, c.launcher, "")
			if state != c.wantState {
				t.Errorf("state = %q want %q（hint=%s）", state, c.wantState, hint)
			}
			for _, want := range c.wantHintContains {
				if !strings.Contains(hint, want) {
					t.Errorf("hint 缺少 %q：%s", want, hint)
				}
			}
			if !strings.Contains(hint, "不下载") {
				t.Errorf("每条指引都要说清「不下载 Node」，实际：%s", hint)
			}
		})
	}
}

// TestDSHInstallDescriptorReportsFoundPath 探测到的路径必须如实透出。
//
// 用真实文件系统造一个"桌面版安装"（exe + app.asar 两个普通文件），
// 断言 detectDesktop 认得出它、并且报的是 **exe 路径**（用户能据此确认装在哪）。
func TestDSHInstallDescriptorReportsFoundPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("桌面版自动探测仅支持 Windows 平台（基于 LOCALAPPDATA 布局）")
	}
	root := t.TempDir()
	install := filepath.Join(root, "Programs", "DeepSeek Harness")
	if err := os.MkdirAll(filepath.Join(install, "resources"), 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(install, "DeepSeek Harness.exe")
	for _, f := range []string{exe, filepath.Join(install, "resources", "app.asar")} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LOCALAPPDATA", root)

	info := detectDesktop()
	if !info.Found {
		t.Fatalf("造出来的桌面版安装必须被探测到，实际：%+v", info)
	}
	if info.Path != exe {
		t.Errorf("desktop.path = %q want %q", info.Path, exe)
	}
}

// ---- reset ----

// TestDSHResetStopsProcessAndClearsLog C11 里 reset 的正面断言。
func TestDSHResetStopsProcessAndClearsLog(t *testing.T) {
	withFakeDSH(t, "ok")
	s := newDSHTestServer(t)
	m := s.dshAgent()

	if _, err := m.Start(); err != nil {
		t.Fatalf("准备阶段启动假 dsh 失败: %v", err)
	}
	// 真实 dsh 启动时本来就会打印一行带 token 的访问地址（假 dsh 也照这个契约来）
	m.appendLog("这是我自己加的一行，用来证明 reset 真的清日志")
	m.logMu.Lock()
	before := len(m.log)
	m.logMu.Unlock()
	if before < 2 {
		t.Fatalf("准备阶段日志应有 ≥2 行，实际 %d", before)
	}

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/dsh/reset", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("reset = %d want 200：%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("reset 响应不是合法 JSON: %v", err)
	}
	if resp["ok"] != true {
		t.Errorf("ok = %v want true：%v", resp["ok"], resp)
	}
	if resp["state"] != dshStateStopped {
		t.Errorf("state = %v want %v", resp["state"], dshStateStopped)
	}
	cleared, ok := resp["cleared"].(map[string]any)
	if !ok {
		t.Fatalf("cleared 结构不对：%v", resp["cleared"])
	}
	if cleared["log"] != true || cleared["run"] != true {
		t.Errorf("cleared 必须是 {log:true,run:true}：%v", cleared)
	}

	// 内部状态：进程没了、日志没了、错误清空
	m.mu.Lock()
	run, state, last := m.run, m.state, m.last
	m.mu.Unlock()
	if run != nil {
		t.Errorf("reset 后仍持有进程（pid=%d）", run.pid)
	}
	if state != dshStateStopped {
		t.Errorf("reset 后 state = %q want %q", state, dshStateStopped)
	}
	if last != "" {
		t.Errorf("reset 后 last_error 应被清空，实际 %q", last)
	}
	m.logMu.Lock()
	logLen := len(m.log)
	m.logMu.Unlock()
	if logLen != 0 {
		t.Errorf("reset 后日志应为空，仍有 %d 行", logLen)
	}

	// 之后再查 status：必须是干净的 stopped，且不再带 pid/port
	w2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/admin/dsh/status", nil))
	var st map[string]any
	if err := json.Unmarshal(w2.Body.Bytes(), &st); err != nil {
		t.Fatalf("status 不是合法 JSON: %v", err)
	}
	if st["running"] != false || st["state"] != dshStateStopped {
		t.Errorf("reset 后 status 应为 stopped/not running：%v", st)
	}
	for _, k := range []string{"pid", "port", "url"} {
		if _, ok := st[k]; ok {
			t.Errorf("reset 后 status 不该再有 %q：%v", k, st[k])
		}
	}
}

// TestDSHResetDoesNotTouchUserData reset 的边界：**只清运行痕迹，不删用户数据**。
//
// 这条测试拿一个真实的 data 目录（里面有账号快照与用户文件）当靶子：
// 一次 reset 之后它们必须逐字节还在。前端按钮旁边写的是"不删你的账号数据"，
// 这句话必须由代码保证，而不是靠人记得别顺手 os.RemoveAll。
func TestDSHResetDoesNotTouchUserData(t *testing.T) {
	dataDir := t.TempDir()
	// 造出典型的用户数据布局（账号快照 + 用户文件 + 指标）
	files := map[string]string{
		filepath.Join(dataDir, "users.json"):                         `{"users":[{"id":"u1"}]}`,
		filepath.Join(dataDir, "metrics.json"):                       `{"days":{}}`,
		filepath.Join(dataDir, "accounts", "trae-cn-916348159.json"): `{"uid":"trae-cn-916348159"}`,
		filepath.Join(dataDir, "accounts", "trae-intl-abc.json"):     `{"uid":"trae-intl-abc"}`,
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "accounts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, content := range files {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s := newDSHTestServer(t)
	s.cfg = &config.Config{APIKey: "", DataDir: dataDir}

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/dsh/reset", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("reset = %d want 200：%s", w.Code, w.Body.String())
	}

	for p, content := range files {
		got, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("reset 之后用户数据不见了：%s（%v）", p, err)
			continue
		}
		if string(got) != content {
			t.Errorf("reset 改动了用户数据 %s：%q → %q", p, content, got)
		}
	}
}

// TestDSHResetIsIdempotent 连点重置不能坏事（控制台按钮会被连点）。
func TestDSHResetIsIdempotent(t *testing.T) {
	s := newDSHTestServer(t)
	h := s.Handler()
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/dsh/reset", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 次 reset = %d want 200：%s", i+1, w.Code, w.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("第 %d 次 reset 响应不是 JSON: %v", i+1, err)
		}
		if resp["state"] != dshStateStopped || resp["ok"] != true {
			t.Errorf("第 %d 次 reset 结果不对：%v", i+1, resp)
		}
	}
}

// ---- export ----

// TestDSHExportIsSafeAttachment 导出端点：附件头 + 安全文件名 + 内容。
func TestDSHExportIsSafeAttachment(t *testing.T) {
	withFakeDSH(t, "ok")
	s := newDSHTestServer(t)
	m := s.dshAgent()
	if _, err := m.Start(); err != nil {
		t.Fatalf("准备阶段启动假 dsh 失败: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Stop() })

	h := s.Handler()

	// 先确认 status 里**确实**带着 token —— 否则下面的"导出里没有 token"
	// 可能是因为压根就没有 token，测试就成了假绿。
	wStatus := httptest.NewRecorder()
	h.ServeHTTP(wStatus, httptest.NewRequest(http.MethodGet, "/admin/dsh/status", nil))
	if !strings.Contains(wStatus.Body.String(), "fake-token-abc123") {
		t.Fatalf("status 里没有 token，导出脱敏测试失去意义：%s", wStatus.Body.String())
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/dsh/export", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("export = %d want 200：%s", w.Code, w.Body.String())
	}

	// 1) 附件头
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q want text/plain", ct)
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment;") {
		t.Fatalf("Content-Disposition = %q，必须以 attachment; 开头", cd)
	}
	name := attachmentFileName(t, cd)
	if strings.TrimSpace(name) == "" {
		t.Error("附件文件名不能为空")
	}
	for _, bad := range []string{`"`, "\r", "\n", "/", "\\"} {
		if strings.Contains(name, bad) {
			t.Errorf("文件名含危险字符 %q：%q", bad, name)
		}
	}
	if !strings.HasSuffix(name, ".txt") {
		t.Errorf("文件名应以 .txt 结尾（text/plain 附件）：%q", name)
	}

	body := w.Body.String()
	// 2) 内容 = 状态 + 日志尾部
	for _, want := range []string{"dsh.status", "运行日志尾部", "# 生成时间:"} {
		if !strings.Contains(body, want) {
			t.Errorf("导出正文缺少 %q：\n%s", want, body)
		}
	}
	// 3) 脱敏：状态字段与日志里的 token 都不能出现在导出文件里
	if strings.Contains(body, "fake-token-abc123") {
		t.Errorf("导出文件泄漏了 dsh 访问 token（日志里那行也必须脱敏）：\n%s", body)
	}
	if !strings.Contains(body, dshExportTokenMask) {
		t.Errorf("导出文件里应出现脱敏掩码 %q 作为证据：\n%s", dshExportTokenMask, body)
	}
}

// attachmentFileName 从 Content-Disposition 里取出文件名（顺带校验格式合法）。
func attachmentFileName(t *testing.T, cd string) string {
	t.Helper()
	const prefix = `attachment; filename="`
	if !strings.HasPrefix(cd, prefix) {
		t.Fatalf("Content-Disposition 格式不符：%q", cd)
	}
	rest := strings.TrimPrefix(cd, prefix)
	if !strings.HasSuffix(rest, `"`) {
		t.Fatalf("filename 没有正确闭合：%q（可能被注入了额外内容）", cd)
	}
	return strings.TrimSuffix(rest, `"`)
}

// TestDSHExportFileNameCannotInjectHeaders 是文件名防注入的**线级**证明。
//
// 不止断言"字符被删掉了"，而是把最终的响应头真的序列化一遍，数一数有几行：
// 只要出现第二行（或出现 Set-Cookie 之类），就说明响应头注入成立。
func TestDSHExportFileNameCannotInjectHeaders(t *testing.T) {
	evil := []string{
		"a\"\r\nSet-Cookie: pwned=1\r\n\r\n<html>pwned</html>",
		"../../../../etc/passwd",
		`C:\Windows\System32\evil.txt`,
		"a\nb\rc.txt",
		"",
	}
	for _, in := range evil {
		got := sanitizeAttachmentName(in)
		if strings.TrimSpace(got) == "" {
			t.Errorf("输入 %q 过滤成了空文件名（必须兜底）", in)
			continue
		}
		for _, bad := range []string{`"`, "\r", "\n", "/", "\\", ";", ":"} {
			if strings.Contains(got, bad) {
				t.Errorf("输入 %q 过滤后仍含 %q：%q", in, bad, got)
			}
		}
		// 线级校验：把响应头真正写出来。注入成功的判据是"多出一行"，
		// 所以这里数行数，而不是看值里有没有可疑子串（引号内的子串是死的）。
		hdr := http.Header{}
		hdr.Set("Content-Disposition", `attachment; filename="`+got+`"`)
		var buf bytes.Buffer
		if err := hdr.Write(&buf); err != nil {
			t.Fatalf("写响应头失败: %v", err)
		}
		lines := strings.Split(strings.TrimRight(buf.String(), "\r\n"), "\r\n")
		if len(lines) != 1 {
			t.Errorf("输入 %q 造成了 %d 行响应头（注入成立）：%q", in, len(lines), buf.String())
		}
		if !strings.HasPrefix(lines[0], "Content-Disposition: ") {
			t.Errorf("输入 %q 让首行不再是 Content-Disposition：%q", in, lines[0])
		}
		if strings.Count(lines[0], "filename=") != 1 {
			t.Errorf("输入 %q 让 filename 参数出现了多次：%q", in, lines[0])
		}
	}
	// 空文件名必须兜底（否则浏览器会拿路径的末段当文件名）
	if sanitizeAttachmentName("") != dshExportFallbackName {
		t.Errorf("空文件名应兜底为 %q", dshExportFallbackName)
	}
	if sanitizeAttachmentName("///") != dshExportFallbackName {
		t.Error("全是被过滤字符时也应兜底")
	}
}

// TestDSHExportRedactsLogTailEvenWithoutStatusToken 只脱敏 status、忘了日志 = 没脱敏。
//
// 这条测试直接盯日志尾巴：dsh 把带 token 的地址打印到 stdout，日志里必然有它。
func TestDSHExportRedactsLogTailEvenWithoutStatusToken(t *testing.T) {
	body := buildDSHExportBody(
		map[string]any{"object": "dsh.status", "state": dshStateStopped},
		[]string{
			"dsh web: http://127.0.0.1:52368/?token=yYm8JXsUNyqNieU1sBz_S0g5cT8_83mXZY9nPeu-ZTs",
			`{"token":"another-secret"}`,
		},
		time.Now(),
	)
	for _, leaked := range []string{"yYm8JXsUNyqNieU1sBz_S0g5cT8_83mXZY9nPeu-ZTs", "another-secret"} {
		if strings.Contains(body, leaked) {
			t.Errorf("导出正文泄漏 %q：\n%s", leaked, body)
		}
	}
	if !strings.Contains(body, "dsh web: http://127.0.0.1:52368/?token="+dshExportTokenMask) {
		t.Errorf("日志行应保留可读结构、只把 token 换掉：\n%s", body)
	}
	// 状态 JSON 必须还在（可读、可贴进 issue）
	if !strings.Contains(body, `"object": "dsh.status"`) {
		t.Errorf("导出正文缺少状态 JSON：\n%s", body)
	}
}

// ---- 鉴权 ----

// TestDSHExtraEndpointsRequireAdmin install/reset/export 都必须要求管理员。
//
// update/check 是**例外且有意为之**（公开只读常量），单独在下面断言 ——
// 把例外写进测试，免得将来有人"统一"成一致行为时把它改坏。
func TestDSHExtraEndpointsRequireAdmin(t *testing.T) {
	s := newDSHTestServer(t)
	s.cfg = &config.Config{APIKey: dshExtraAdminKey}
	h := s.Handler()

	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/admin/dsh/install"},
		{http.MethodPost, "/admin/dsh/reset"},
		{http.MethodGet, "/admin/dsh/export"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("无凭证 %s %s = %d want 401", c.method, c.path, w.Code)
		}
		// 带上正确的 Key 必须过（否则"要鉴权"就退化成"永远打不开"）
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("Authorization", "Bearer "+dshExtraAdminKey)
		w2 := httptest.NewRecorder()
		h.ServeHTTP(w2, req)
		if w2.Code != http.StatusOK {
			t.Errorf("带管理员 Key %s %s = %d want 200：%s", c.method, c.path, w2.Code, w2.Body.String())
		}
	}

	// update/check 刻意公开：控制台在还没有凭证时就会调它
	for _, key := range []string{"", dshExtraAdminKey} {
		req := httptest.NewRequest(http.MethodGet, "/admin/system/update/check", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("update/check（Key=%q）= %d want 200（它是公开只读元数据）", key, w.Code)
		}
	}
}

// ---- 系统更新 ----

// TestUpdateCheckDisabled 测试禁用自动更新时的行为。
func TestUpdateCheckDisabled(t *testing.T) {
	s := newDSHTestServer(t)
	s.cfg.Update.Enabled = false
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/system/update/check", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("update/check = %d want 200：%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if resp["supported"] != false {
		t.Errorf("supported = %v want false（已禁用自动更新）", resp["supported"])
	}
	if resp["current"] != Version {
		t.Errorf("current = %v want %v", resp["current"], Version)
	}
	reason, _ := resp["reason"].(string)
	if !strings.Contains(reason, "禁用") {
		t.Errorf("reason 应说明已禁用：%q", reason)
	}
}
