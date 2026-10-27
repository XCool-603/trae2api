package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trae2api/internal/config"
	"trae2api/internal/pool"
	"trae2api/internal/usermgr"
)

// TestWebAuthDisabledWhenNoPassword 未配置密码时免密。
func TestWebAuthDisabledWhenNoPassword(t *testing.T) {
	for _, pw := range []string{"", "   ", "\t"} {
		w := newWebAuth(pw)
		if w.Enabled() {
			t.Errorf("密码 %q 不该启用闸", pw)
		}
		if !w.Check("anything") {
			t.Error("未启用时应恒通过")
		}
		if !w.ValidSession("") {
			t.Error("未启用时任意会话都应有效")
		}
	}
}

// TestWebAuthCheck 密码校验。
func TestWebAuthCheck(t *testing.T) {
	w := newWebAuth("s3cret-pw")
	if !w.Enabled() {
		t.Fatal("应启用")
	}
	if !w.Check("s3cret-pw") {
		t.Error("正确密码应通过")
	}
	for _, bad := range []string{"", "wrong", "s3cret-p", "s3cret-pww", "S3CRET-PW", " s3cret-pw"} {
		if w.Check(bad) {
			t.Errorf("错误密码 %q 不该通过", bad)
		}
	}
	// 前后空白应被 trim（配置里常带空格）
	if !newWebAuth("  pw  ").Check("pw") {
		t.Error("配置里的空白应被 trim")
	}
}

// TestWebAuthConstantTimeCompare 必须用常数时间比较。
//
// ⚠️ 这条测不出时序（单元测试做不到），但能**锁住实现方式**：
// 如果有人把它改回 `==`，这个测试不会失败 —— 所以真正的价值在注释与代码审查。
// 这里做的是**行为等价性**验证：确认各种长度的输入都得到正确结果
// （ConstantTimeCompare 对长度不同的输入直接返回 0，这也是对的）。
func TestWebAuthConstantTimeCompare(t *testing.T) {
	w := newWebAuth("abcdefgh")
	// 长度不同的输入
	for _, in := range []string{"a", "ab", "abcdefg", "abcdefghi", "abcdefgh" + strings.Repeat("x", 100)} {
		want := in == "abcdefgh"
		if got := w.Check(in); got != want {
			t.Errorf("Check(%q) = %v want %v", in, got, want)
		}
	}
}

// TestWebAuthSessionLifecycle 会话签发/校验/注销。
func TestWebAuthSessionLifecycle(t *testing.T) {
	w := newWebAuth("pw")
	tok := w.NewSession()
	if tok == "" {
		t.Fatal("token 为空")
	}
	if len(tok) != 64 {
		t.Errorf("token 应为 32 字节 hex（64 字符），实际 %d", len(tok))
	}
	if !w.ValidSession(tok) {
		t.Error("刚签发的会话应有效")
	}
	if w.ValidSession("not-a-token") {
		t.Error("伪造 token 不该有效")
	}
	if w.ValidSession("") {
		t.Error("空 token 不该有效")
	}
	w.DropSession(tok)
	if w.ValidSession(tok) {
		t.Error("注销后不该有效")
	}
	// 每次签发都不同
	a, b := w.NewSession(), w.NewSession()
	if a == b {
		t.Error("两次签发的 token 不该相同")
	}
}

// TestWebAuthSessionExpiry 会话过期。
func TestWebAuthSessionExpiry(t *testing.T) {
	w := newWebAuth("pw")
	tok := w.NewSession()
	// 手动把它改成已过期
	w.mu.Lock()
	w.sessions[tok] = time.Now().Add(-time.Second)
	w.mu.Unlock()
	if w.ValidSession(tok) {
		t.Error("过期会话不该有效")
	}
	// 过期项应被清掉
	w.mu.Lock()
	_, still := w.sessions[tok]
	w.mu.Unlock()
	if still {
		t.Error("过期会话应被惰性清理")
	}
}

// TestWebAuthReapDoesNotLeak 惰性清理不该无限增长。
func TestWebAuthReapDoesNotLeak(t *testing.T) {
	w := newWebAuth("pw")
	for i := 0; i < 50; i++ {
		tok := w.NewSession()
		w.mu.Lock()
		w.sessions[tok] = time.Now().Add(-time.Second) // 立刻过期
		w.mu.Unlock()
	}
	if n := w.SessionCount(); n != 0 {
		t.Errorf("过期会话应被清空，实际剩 %d", n)
	}
}

// TestConsoleGateBlocksWithoutCookie 未登录时控制台应返回登录页。
func TestConsoleGateBlocksWithoutCookie(t *testing.T) {
	s := &Server{webAuth: newWebAuth("pw")}
	req := httptest.NewRequest(http.MethodGet, "/console", nil)
	w := httptest.NewRecorder()
	s.handleConsole(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("未登录应 401，实际 %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "控制台密码") {
		t.Error("应返回登录页")
	}
	// 绝不能泄露控制台内容
	if strings.Contains(body, "Trae2Api 控制台</title>") && !strings.Contains(body, "需要密码") {
		t.Error("泄露了控制台页面")
	}
	if !strings.Contains(body, "no-store") {
		// Cache-Control 在 header 里
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("登录页不该被缓存，Cache-Control = %q", cc)
	}
}

// TestConsoleGateAllowsWithCookie 登录后放行。
func TestConsoleGateAllowsWithCookie(t *testing.T) {
	wa := newWebAuth("pw")
	s := &Server{webAuth: wa, loginLimit: newLoginLimiter()}
	tok := wa.NewSession()

	req := httptest.NewRequest(http.MethodGet, "/console", nil)
	req.AddCookie(&http.Cookie{Name: webAuthCookie, Value: tok})
	w := httptest.NewRecorder()
	s.handleConsole(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("已登录应 200，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Trae2Api 控制台") {
		t.Error("应返回控制台页面")
	}
}

// TestConsoleGateDisabledWithoutPassword 未配置密码时不该挡。
func TestConsoleGateDisabledWithoutPassword(t *testing.T) {
	s := &Server{webAuth: newWebAuth("")}
	req := httptest.NewRequest(http.MethodGet, "/console", nil)
	w := httptest.NewRecorder()
	s.handleConsole(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("免密时应 200，实际 %d", w.Code)
	}
}

// TestConsoleLoginSuccess 正确密码应签发 Cookie 并跳转。
func TestConsoleLoginSuccess(t *testing.T) {
	wa := newWebAuth("pw")
	s := &Server{webAuth: wa, loginLimit: newLoginLimiter()}

	form := url.Values{"password": {"pw"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleConsoleLogin(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("应 302 跳转，实际 %d", w.Code)
	}
	var found *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == webAuthCookie {
			found = c
		}
	}
	if found == nil {
		t.Fatal("未签发会话 Cookie")
	}
	// 安全属性
	if !found.HttpOnly {
		t.Error("Cookie 必须 HttpOnly（否则 XSS 能偷走会话）")
	}
	if found.SameSite != http.SameSiteLaxMode {
		t.Error("Cookie 应设 SameSite")
	}
	if found.MaxAge <= 0 {
		t.Error("Cookie 应有有效期")
	}
	if !wa.ValidSession(found.Value) {
		t.Error("签发的会话应有效")
	}
}

// TestConsoleLoginWrongPassword 错误密码不该签发任何东西。
func TestConsoleLoginWrongPassword(t *testing.T) {
	wa := newWebAuth("pw")
	s := &Server{webAuth: wa, loginLimit: newLoginLimiter()}

	form := url.Values{"password": {"wrong"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleConsoleLogin(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("应 401，实际 %d", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == webAuthCookie && c.Value != "" {
			t.Error("密码错误不该签发会话")
		}
	}
	if wa.SessionCount() != 0 {
		t.Error("密码错误不该产生会话")
	}
	// 错误提示不该区分"密码为空"与"密码错误"
	if strings.Contains(w.Body.String(), "空") {
		t.Error("不该泄露额外信息")
	}
}

// TestConsoleLogout 注销应使会话失效。
func TestConsoleLogout(t *testing.T) {
	wa := newWebAuth("pw")
	s := &Server{webAuth: wa, loginLimit: newLoginLimiter()}
	tok := wa.NewSession()

	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(&http.Cookie{Name: webAuthCookie, Value: tok})
	w := httptest.NewRecorder()
	s.handleConsoleLogout(w, req)

	if w.Code != http.StatusFound {
		t.Errorf("应 302，实际 %d", w.Code)
	}
	if wa.ValidSession(tok) {
		t.Error("注销后会话应失效")
	}
}

// TestHTMLEscape 登录页的转义（防注入）。
func TestHTMLEscape(t *testing.T) {
	got := htmlEscape(`<script>alert("x")&'</script>`)
	for _, bad := range []string{"<script>", `"`, "'"} {
		if strings.Contains(got, bad) {
			t.Errorf("未转义 %q: %s", bad, got)
		}
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Errorf("应转义尖括号: %s", got)
	}
}

// TestConsoleLoginRegularUser 普通用户也能通过控制台门禁登录并查看控制台与状态
func TestConsoleLoginRegularUser(t *testing.T) {
	um := usermgr.New(filepath.Join(t.TempDir(), "users.json"))
	if _, err := um.CreateWithPassword("admin", usermgr.RoleAdmin, "admin-pw-123456", "首个管理员"); err != nil {
		t.Fatalf("创建管理员失败: %v", err)
	}
	u, err := um.CreateWithPassword("bob", usermgr.RoleUser, "bob-pw-123456", "测试普通用户")
	if err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}

	wa := newWebAuth("admin-secret-password")
	p := pool.New(pool.Options{})
	s := &Server{
		cfg:        &config.Config{APIKey: "sk-global-admin"},
		users:      um,
		webAuth:    wa,
		loginLimit: newLoginLimiter(),
		pool:       p,
	}

	// 1. 未登录访问 /console 应被拦
	req0 := httptest.NewRequest(http.MethodGet, "/console", nil)
	w0 := httptest.NewRecorder()
	s.handleConsole(w0, req0)
	if w0.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，实际 %d", w0.Code)
	}

	// 2. 普通用户以用户名+密码登录
	form := url.Values{
		"username": {"bob"},
		"password": {"bob-pw-123456"},
	}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleConsoleLogin(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("应 302 跳转，实际 %d", w.Code)
	}
	var sessCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			sessCookie = c
		}
	}
	if sessCookie == nil {
		t.Fatal("未签发 sessionCookie")
	}

	// 3. 携带普通用户 Cookie 访问 /console 应放行
	req2 := httptest.NewRequest(http.MethodGet, "/console", nil)
	req2.AddCookie(sessCookie)
	w2 := httptest.NewRecorder()
	s.handleConsole(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("普通用户登录后应 200 放行，实际 %d", w2.Code)
	}

	// 4. 访问 /status，应在 current_user 返回普通用户身份，且 isAdmin 为 false
	req3 := httptest.NewRequest(http.MethodGet, "/status", nil)
	req3.AddCookie(sessCookie)
	w3 := httptest.NewRecorder()
	s.withAuth(s.handleStatus).ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("/status 应 200，实际 %d", w3.Code)
	}
	var statusResp map[string]any
	if err := json.Unmarshal(w3.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("解析 status JSON 失败: %v", err)
	}
	cu, ok := statusResp["current_user"].(map[string]any)
	if !ok {
		t.Fatalf("statusResp 缺少 current_user: %v", statusResp)
	}
	if cu["username"] != "bob" || cu["role"] != "user" || cu["id"] != u.ID {
		t.Errorf("current_user 不匹配: got %v", cu)
	}

	// 5. 普通用户尝试访问管理接口（如 /admin/checkin）应 403
	reqAdmin := httptest.NewRequest(http.MethodPost, "/admin/checkin", nil)
	reqAdmin.AddCookie(sessCookie)
	wAdmin := httptest.NewRecorder()
	s.withAuth(s.handleCheckin).ServeHTTP(wAdmin, reqAdmin)
	if wAdmin.Code != http.StatusForbidden {
		t.Errorf("普通用户访问管理端点应 403，实际 %d", wAdmin.Code)
	}
}
