package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"trae2api/internal/usermgr"
)

// 本文件实现**控制台 Web 密码闸**（config 里的 `web_password`）。
//
// ## 为什么必须有这个文件
//
// `web_password` 这个配置项**早就声明了**，注释写着"控制台密码；空 = 免密"，
// 但**代码里从来没读过它**。那是最糟的一种情况：用户设了密码、以为控制台受保护，
// 实际上一点作用都没有 —— 一个"看起来是安全控制"的死配置，比没有这个配置危险得多。
//
// ## 定位：它保护什么、不保护什么
//
// - **保护**：控制台**页面本身**（挡掉随手打开 http://host:port/ 的人）。
// - **不保护**：API 端点。那些由 API Key / RBAC 负责（见 withAuth）。
//
// 两者是**正交**的：密码闸挡的是"浏览器访问 UI"，API Key 挡的是"程序访问接口"。
// 所以即使设了密码，用 curl 调 /v1/chat/completions 仍然只需要 API Key。
//
// ## 会话怎么维持
//
// 登录成功后发一个**随机 token** 存进程内（带 TTL），放进 HttpOnly Cookie。
// 不做签名 Cookie：那需要引入 HMAC 与密钥管理，而这里的威胁模型只是
// "挡住随手访问"，进程内 token 足够且更简单（重启即失效，符合预期）。
type webAuth struct {
	// password 为空的闸是"关闭"状态（免密）。
	password string

	mu       sync.Mutex
	sessions map[string]time.Time // token → 过期时刻
}

// webSessionTTL 控制台登录态有效期。
//
// 取 12 小时：覆盖一个工作日的使用，又不至于让一次登录永久有效。
const webSessionTTL = 12 * time.Hour

// MinPasswordLen 是控制台密码的最短长度。
//
// 设 6 位而不是更长：这是**本地自托管工具**的门禁，过长的强制要求会让用户
// 干脆不设密码（那就完全没有保护了）。真正的防线是"设了密码 + 登录限流"，
// 不是密码强度本身。
const MinPasswordLen = 6

func newWebAuth(password string) *webAuth {
	return &webAuth{password: strings.TrimSpace(password), sessions: map[string]time.Time{}}
}

// ValidatePassword 校验密码是否符合要求（供配置校验与将来的改密端点用）。
//
// 空串是合法的（= 免密）；非空则必须 >= MinPasswordLen。
func ValidatePassword(pw string) error {
	pw = strings.TrimSpace(pw)
	if pw == "" {
		return nil // 免密
	}
	// 按**字符数**而不是字节数：中文密码按字节算会莫名其妙地"够长"
	if len([]rune(pw)) < MinPasswordLen {
		return fmt.Errorf("控制台密码至少需要 %d 位（当前 %d 位）；留空则恢复单机免密",
			MinPasswordLen, len([]rune(pw)))
	}
	return nil
}

// Enabled 报告密码闸是否生效（未配置密码时为 false = 免密）。
func (w *webAuth) Enabled() bool {
	return w != nil && w.password != ""
}

// Check 用常数时间比较密码。
//
// ⚠️ 必须用 subtle.ConstantTimeCompare 而不是 `==`：
// 字符串比较会在第一个不同的字节处提前返回，攻击者可以通过测量响应时间
// 逐字节猜出密码（时序侧信道）。这是"看起来能用但实际有洞"的典型。
func (w *webAuth) Check(input string) bool {
	if !w.Enabled() {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(input), []byte(w.password)) == 1
}

// NewSession 登录成功后签发一个会话 token。
func (w *webAuth) NewSession() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败是严重问题，不该静默降级到弱随机
		panic("webauth: crypto/rand 不可用: " + err.Error())
	}
	token := hex.EncodeToString(b)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reapLocked()
	w.sessions[token] = time.Now().Add(webSessionTTL)
	return token
}

// ValidSession 校验会话 token。
func (w *webAuth) ValidSession(token string) bool {
	if !w.Enabled() {
		return true
	}
	if token == "" {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	until, ok := w.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(w.sessions, token)
		return false
	}
	return true
}

// DropSession 注销。
func (w *webAuth) DropSession(token string) {
	if w == nil || token == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.sessions, token)
}

// reapLocked 惰性清理过期会话（不另起 goroutine）。
func (w *webAuth) reapLocked() {
	now := time.Now()
	for t, until := range w.sessions {
		if now.After(until) {
			delete(w.sessions, t)
		}
	}
}

// SessionCount 返回有效会话数（诊断用）。
func (w *webAuth) SessionCount() int {
	if !w.Enabled() {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reapLocked()
	return len(w.sessions)
}

// ---- HTTP 层 ----

const webAuthCookie = "trae2api_console"

// consoleAuthed 判断当前请求是否有权看控制台页面。
//
// 未配置密码 → 恒 true（免密）。
func (s *Server) consoleAuthed(r *http.Request) bool {
	if s.webAuth == nil || !s.webAuth.Enabled() {
		return true
	}
	if c, err := r.Cookie(webAuthCookie); err == nil && s.webAuth.ValidSession(c.Value) {
		return true
	}
	// 如果用户体系启用了，任何有效且未禁用的登录用户（含普通用户与管理员），放行访问控制台 UI
	if s.users != nil {
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			if id, ok := loginSessions.userID(c.Value); ok {
				if u, ok := s.users.ByID(id); ok && !u.Disabled {
					return true
				}
			}
		}
	}
	return false
}

// trustProxyHeaders 安全地读取配置（cfg 为 nil 时按 false 处理）。
//
// 为什么容错：cfg 为 nil 是"构造不完整"，不该让**登录处理**崩掉 ——
// 那会把一个配置问题放大成"控制台完全打不开"。默认 false 也是更安全的取值。
func (s *Server) trustProxyHeaders() bool {
	return s.cfg != nil && s.cfg.TrustProxyHeaders
}

// handleConsoleLogin POST /login —— 校验密码并签发会话。
func (s *Server) handleConsoleLogin(w http.ResponseWriter, r *http.Request) {
	if s.webAuth == nil || !s.webAuth.Enabled() {
		http.Redirect(w, r, "/console", http.StatusFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}
	// 防暴力破解：按来源 IP 限流（见 security.go 的 loginLimiter）。
	ip := clientIP(r, s.trustProxyHeaders())
	now := time.Now()
	if ok, remain := s.loginLimit.Allow(ip, now); !ok {
		s.logf("WARN: 控制台登录已被限流（IP %s，还需 %.0f 秒）", ip, remain.Seconds())
		writeLoginHTML(w, http.StatusTooManyRequests,
			fmt.Sprintf("尝试次数过多，请在 %d 分钟后再试", int(remain.Minutes())+1))
		return
	}

	uname := strings.TrimSpace(r.FormValue("username"))
	pw := r.FormValue("password")

	var authenticatedUser *usermgr.User
	authed := false

	if uname != "" && s.users != nil {
		// 指定用户名登录（支持管理员和普通用户）
		if u, ok := s.users.ByUsername(uname); ok && !u.Disabled {
			if s.users.CheckPassword(u.ID, pw) {
				authed = true
				authenticatedUser = u
			}
		}
	} else {
		// 未指定用户名时：优先匹配全局 web_password
		if s.webAuth.Check(pw) {
			authed = true
		} else if s.users != nil {
			// 容错：如果用户输入的是可用用户的密码（优先管理员）
			for _, u := range s.users.List() {
				if !u.Disabled && u.PasswordHash != "" {
					if s.users.CheckPassword(u.ID, pw) {
						authed = true
						authenticatedUser, _ = s.users.ByID(u.ID)
						break
					}
				}
			}
		}
	}

	if !authed {
		locked, lockFor := s.loginLimit.NoteFailure(ip, now)
		if locked {
			s.logf("WARN: 控制台登录连续失败达阈值，已锁定 IP %s（%s）", ip, lockFor)
			writeLoginHTML(w, http.StatusTooManyRequests,
				fmt.Sprintf("尝试次数过多，请在 %d 分钟后再试", int(lockFor.Minutes())))
			return
		}
		s.logf("WARN: 控制台登录失败（密码错误）来自 %s", ip)
		// 不区分"密码错误"与"密码为空"，避免给攻击者额外信息
		writeLoginHTML(w, http.StatusUnauthorized, "用户名或密码不正确")
		return
	}
	s.loginLimit.NoteSuccess(ip)

	if authenticatedUser != nil {
		token, err := loginSessions.new(authenticatedUser.ID)
		if err == nil {
			setSessionCookie(w, token)
		}
		// 如果是管理员，同时签发 webAuthCookie 保证对传统管理员路由的兼容
		if authenticatedUser.Role == usermgr.RoleAdmin || !s.webAuth.Enabled() {
			adminToken := s.webAuth.NewSession()
			http.SetCookie(w, &http.Cookie{
				Name:     webAuthCookie,
				Value:    adminToken,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
				MaxAge:   int(webSessionTTL.Seconds()),
			})
		}
	} else {
		token := s.webAuth.NewSession()
		http.SetCookie(w, &http.Cookie{
			Name:     webAuthCookie,
			Value:    token,
			Path:     "/",
			HttpOnly: true, // 禁止 JS 读取，降低 XSS 窃取风险
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(webSessionTTL.Seconds()),
		})
	}

	s.logf("INFO: 控制台登录成功，来自 %s", r.RemoteAddr)
	http.Redirect(w, r, "/console", http.StatusFound)
}

// handleConsoleLogout POST /logout
func (s *Server) handleConsoleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(webAuthCookie); err == nil {
		s.webAuth.DropSession(c.Value)
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		loginSessions.drop(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: webAuthCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
	})
	clearSessionCookie(w)
	http.Redirect(w, r, "/console", http.StatusFound)
}

// writeLoginHTML 渲染控制台登录页（100% 对齐控制台与门户的赛博暗黑极客风格）。
func writeLoginHTML(w http.ResponseWriter, status int, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	msg := ""
	if errMsg != "" {
		msg = `<div class="err err-box"><svg fill="none" stroke="currentColor" stroke-width="2" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z"/></svg><span>` + htmlEscape(errMsg) + `</span></div>`
	}
	_, _ = w.Write([]byte(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Trae2Api 控制台登录 · 需要密码</title><style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,sans-serif;background-color:#030712;color:#f1f5f9;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:20px;position:relative;overflow:hidden}
.cyber-grid{position:fixed;inset:0;background-size:40px 40px;background-image:linear-gradient(to right,rgba(255,255,255,0.03) 1px,transparent 1px),linear-gradient(to bottom,rgba(255,255,255,0.03) 1px,transparent 1px);pointer-events:none;z-index:0}
.orb{position:fixed;border-radius:9999px;filter:blur(100px);pointer-events:none;z-index:0}
.orb-1{top:-10%;left:20%;width:450px;height:450px;background:radial-gradient(circle,rgba(99,102,241,0.22) 0%,transparent 70%)}
.orb-2{bottom:-10%;right:20%;width:500px;height:500px;background:radial-gradient(circle,rgba(168,85,247,0.18) 0%,transparent 70%)}
.login-card{position:relative;z-index:10;width:100%;max-width:390px;padding:38px 32px 30px;background:rgba(11,17,32,0.72);backdrop-filter:blur(20px);-webkit-backdrop-filter:blur(20px);border:1px solid rgba(255,255,255,0.09);border-radius:24px;box-shadow:0 25px 50px -12px rgba(0,0,0,0.65),0 0 35px -5px rgba(99,102,241,0.2);text-align:center}
.icon-box{width:56px;height:56px;margin:0 auto 16px;border-radius:18px;background:linear-gradient(135deg,#6366f1 0%,#a855f7 50%,#ec4899 100%);display:flex;align-items:center;justify-content:center;box-shadow:0 10px 25px -4px rgba(99,102,241,0.45)}
.icon-box svg{width:28px;height:28px;stroke:#fff}
.brand-title{font-size:22px;font-weight:800;letter-spacing:-0.025em;color:#fff;display:flex;align-items:center;justify-content:center;gap:8px;margin-bottom:6px}
.brand-title .accent{color:#818cf8}
.pro-badge{font-size:10px;font-weight:700;letter-spacing:0.05em;padding:2px 7px;border-radius:9999px;background:linear-gradient(135deg,rgba(99,102,241,0.25),rgba(168,85,247,0.25));color:#c7d2fe;border:1px solid rgba(99,102,241,0.35)}
.sub-title{color:#94a3b8;font-size:13px;margin-bottom:24px}
.err-box{background:rgba(244,63,94,0.12);border:1px solid rgba(244,63,94,0.35);color:#fda4af;border-radius:12px;font-size:13px;padding:10px 14px;margin-bottom:18px;display:flex;align-items:center;gap:8px;text-align:left}
.err-box svg{width:16px;height:16px;flex-shrink:0;stroke:#fb7185}
.form-group{margin-bottom:16px}
.input-pw{width:100%;padding:12px 16px;background:rgba(8,12,22,0.9);color:#f8fafc;border:1px solid rgba(51,65,85,0.8);border-radius:14px;font-size:14px;font-family:ui-monospace,"JetBrains Mono",Consolas,monospace;text-align:center;letter-spacing:2px;transition:all .2s ease}
.input-pw::placeholder{color:#64748b;letter-spacing:normal;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;font-size:13px}
.input-pw:focus{outline:none;border-color:#6366f1;box-shadow:0 0 0 3px rgba(99,102,241,0.25);background:rgba(11,17,32,0.95)}
.btn-submit{width:100%;padding:12px;background:linear-gradient(135deg,#4f46e5 0%,#7c3aed 50%,#6366f1 100%);color:#fff;border:none;border-radius:14px;font-size:14px;font-weight:700;cursor:pointer;box-shadow:0 8px 24px -4px rgba(79,70,229,0.45);transition:all .2s ease}
.btn-submit:hover{filter:brightness(1.1);transform:translateY(-1px);box-shadow:0 10px 28px -4px rgba(79,70,229,0.6)}
.btn-submit:active{transform:translateY(0)}
.footer-links{margin-top:22px;padding-top:16px;border-top:1px solid rgba(255,255,255,0.06);display:flex;align-items:center;justify-content:space-between;font-size:11px;color:#64748b}
.footer-links a{color:#818cf8;text-decoration:none;transition:color .15s ease}
.footer-links a:hover{color:#a5b4fc;text-decoration:underline}
</style></head><body>
<div class="cyber-grid"></div>
<div class="orb orb-1"></div>
<div class="orb orb-2"></div>
<div class="login-card">
  <div class="icon-box">
    <svg fill="none" stroke="currentColor" stroke-width="2" viewBox="0 0 24 24">
      <path stroke-linecap="round" stroke-linejoin="round" d="M12 15v2m-6 4h12a2 2 0 002-2v-6a2 2 0 00-2-2H6a2 2 0 00-2 2v6a2 2 0 002 2zm10-10V7a4 4 0 00-8 0v4h8z"/>
    </svg>
  </div>
  <div class="brand-title">
    Trae<span class="accent">2Api</span>
    <span class="pro-badge">PRO</span>
  </div>
  <p class="sub-title">控制台安全门禁 · 需要密码</p>
  ` + msg + `
  <form method="POST" action="/login">
    <div class="form-group" style="margin-bottom:12px">
      <input type="text" name="username" class="input-pw" style="text-align:left;letter-spacing:normal" placeholder="用户名（选填，仅管理员可不填）" autocomplete="username">
    </div>
    <div class="form-group">
      <input type="password" name="password" class="input-pw" style="text-align:left;letter-spacing:normal" placeholder="请输入控制台密码" autofocus autocomplete="current-password" required>
    </div>
    <button type="submit" class="btn-submit">登 录 进 入</button>
  </form>
  <div class="footer-links">
    <a href="/">← 返回公开门户</a>
    <span>Trae 订阅额度 → API</span>
  </div>
</div></body></html>`))
}

// htmlEscape 是给登录页用的最小转义（只用于把固定文案与错误提示放进 HTML）。
func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}
