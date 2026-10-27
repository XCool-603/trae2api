package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"trae2api/internal/accountstore"
	"trae2api/internal/config"
	"trae2api/internal/pool"
	"trae2api/internal/usermgr"
)

// 本文件把控制台的验收标准（C3–C8）写成可执行的断言。
//
// 为什么用测试而不是只写一份 verify.txt：控制台是"整页替换"，最容易在
// 后续改动里悄悄退化的正是这几条（引回一个 CDN、删掉一个标签页、
// 把端点名改错）。写进测试才能在 CI/本地一键发现。

// TestConsolePageAssembly 资源必须真的注入进页面。
func TestConsolePageAssembly(t *testing.T) {
	if strings.Contains(consoleHTML, consoleCSSPlaceholder) ||
		strings.Contains(consoleHTML, consoleJSPlaceholder) {
		t.Fatalf("页面里仍有未替换的占位符（CSS/JS 未注入）")
	}
	for _, want := range []string{"<!DOCTYPE html>", "</html>", ".glass-panel", "function switchMainTab"} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("控制台页面缺少 %q（资源注入不完整？）", want)
		}
	}
}

// TestConsoleNoExternalResources C3：零外部资源。
//
// 为什么这条必须有：参考页面用的是 `cdn.tailwindcss.com` + Google Fonts，
// 而我们的 CSP 是 `default-src 'self'` —— 照抄的后果是"页面加载了但样式全无、
// JS 不执行"，而且控制台自己不报错，极难排查。
func TestConsoleNoExternalResources(t *testing.T) {
	for _, banned := range []string{"cdn.", "fonts.googleapis", "fonts.gstatic", "unpkg.com", "jsdelivr"} {
		if strings.Contains(consoleHTML, banned) {
			t.Errorf("控制台页面引用了外域资源 %q（必须自包含、离线可用）", banned)
		}
	}
	if m := regexp.MustCompile(`https?://`).FindString(consoleHTML); m != "" {
		t.Errorf("控制台页面里出现绝对 URL %q —— 页面必须零外域引用", m)
	}
	if m := regexp.MustCompile(`(?i)(src|href)\s*=\s*["']?\s*//`).FindString(consoleHTML); m != "" {
		t.Errorf("控制台页面里有协议相对的 src/href %q（= 外域）", m)
	}
}

// TestConsoleSixMainTabs C4：6 个标签页都在。
func TestConsoleSixMainTabs(t *testing.T) {
	for _, tab := range []string{"accounts", "access", "analytics", "playground", "dsh", "users"} {
		if !strings.Contains(consoleHTML, `id="mainTabBtn-`+tab+`"`) {
			t.Errorf("缺少标签按钮 mainTabBtn-%s", tab)
		}
		if !strings.Contains(consoleHTML, `id="tabContent-`+tab+`"`) {
			t.Errorf("缺少标签容器 tabContent-%s", tab)
		}
	}
	// 切换函数与全部标签 id 都要在页面里（否则点了没反应）
	for _, fn := range []string{"function switchMainTab(", "function switchAuthTab("} {
		if !strings.Contains(consoleHTML, fn) {
			t.Errorf("缺少 %s", fn)
		}
	}
}

// TestConsoleAccountPoolStats C5：账号池 6 项统计。
func TestConsoleAccountPoolStats(t *testing.T) {
	for _, id := range []string{"statTotal", "statHealthy", "statCooling", "statDisabled", "statSticky", "statInFlight"} {
		if !strings.Contains(consoleHTML, `id="`+id+`"`) {
			t.Errorf("缺少账号池统计项 %s", id)
		}
	}
	// 批量动作（契约 A）也要在页面上有入口
	for _, fn := range []string{"batchRevive", "batchControl", "batchDelete", "reviveAll", "toggleSelectAllAccounts"} {
		if !strings.Contains(consoleHTML, fn) {
			t.Errorf("缺少批量动作 %s", fn)
		}
	}
}

// TestConsoleAnalyticsPanel C6：用量分析可渲染 token 与分模型 / 按日表。
func TestConsoleAnalyticsPanel(t *testing.T) {
	for _, want := range []string{"function renderTokenAnalytics", `id="tokenModelsTable"`, `id="dailyHistoryTable"`, `id="metricsDateSelector"`, "function selectDateFromHistory"} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("分析页缺少 %q", want)
		}
	}
}

// TestConsolePlaygroundAttachments C7：演练场支持附件。
func TestConsolePlaygroundAttachments(t *testing.T) {
	for _, want := range []string{`id="chatFileInput"`, `id="chatPrompt"`, `id="chatImagePreview"`, "function sendChatMessage", "function handleChatFileSelect", "image_url"} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("演练场缺少 %q", want)
		}
	}
}

// TestConsoleUserManagement C8：用户管理页的 7 个能力都有入口。
func TestConsoleUserManagement(t *testing.T) {
	for _, want := range []string{
		`id="adminUsersTableBody"`, `id="userSearchInput"`, `id="adminCreateUserForm"`,
		"function filterAdminUsers", "function submitAdminCreateUser", "function toggleUserRole",
		"function toggleUserDisabled", "function adminResetKey", "function adminResetPassword",
		"function deleteUser", "function toggleAllowRegister",
	} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("用户管理页缺少 %q", want)
		}
	}
}

// TestConsoleAuthAndDshPanels 认证与 dsh 面板的入口存在。
func TestConsoleAuthAndDshPanels(t *testing.T) {
	for _, want := range []string{
		`id="loginOverlay"`, "function doLogin", "function doRegister", "function doLogout",
		`id="changePasswordModal"`, `id="passwordTarget"`, "function submitChangePassword",
		`id="oauthModal"`, "function openOAuthModal", "function checkOAuthPoll",
		`id="importModal"`, "function submitImport", "function switchImportTab",
		`id="updateModal"`, "function checkAppUpdate",
		`id="dshStatusBadge"`, `id="dshIframe"`, "function fetchDshStatus", "function installDshEnv",
		"function resetDshEnv", "function exportDshEnv",
	} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("控制台缺少 %q", want)
		}
	}
}

// ---------------------------------------------------------------------------
// 端点契约闸（第 1 轮独立验收 B3 的返工）
//
// ## 上一版为什么是假绿
//
// 上一版（原 TestConsoleOnlyUsesKnownEndpoints）拿着一份**手写**的 known 列表，
// 判据是"前端字符串 ∈ 手写列表"。那份列表里躺着 7 个 Go 代码里**根本不存在**的
// 端点（admin/accounts/revive|control|delete、admin/dsh/install|reset|export、
// admin/system/update/check），于是 `go test ./...` 全绿与"这 7 个端点整块缺失"
// 可以同时成立 —— 测试本身成了缺口的逃逸机制。
//
// ## 现在的判据
//
// 判定权**全部**交给真实路由表（server.go 里注册的那个 *http.ServeMux）：
//
//	TestConsoleFrontendEndpointsAreRouted   正向：页面每个调用点都要在真实路由表上命中
//	TestConsoleFrozenContractEndpointsExist 反向：冻结契约里每个端点都必须存在
//	TestConsoleFrontendEndpointsAreInContract 归属：前端命中的路由必须属于契约
//
// 手写数据只剩两类，**都不参与判定**：
//   - frozenContract：契约原文，它是"被检验的声明"而不是判据；
//   - consoleDynamicOverrides：把调用模板具体化成探测路径时才需要的枚举值。
//
// 页面里的调用点是**从 app.js 自动抽取**的：谁在页面上加一个 fetch，闸门立刻
// 覆盖它。动态段先按占位值探测；只有"动态段其实是枚举值"（占位值探不中任何真路由）
// 才会报错并要求在 consoleDynamicOverrides 里补上取值 —— 不允许"抽不出来就跳过"。
// ---------------------------------------------------------------------------

// portalCatchAll 是 server.go 注册的门户兜底模式。
//
// "GET /" 会匹配**所有**路径（Go 1.22 ServeMux 的子树语义），但对 "/" 之外的
// 路径 portal.go 一律返回 404。如果不把它排除，"GET 端点根本不存在"的探测
// 会命中它、拿到一个非空 pattern，从而被误判成"已路由" —— 这正是上一轮
// `GET /admin/dsh/export` / `GET /admin/system/update/check` 的伪装方式：
// 它们返回的 404 来自门户兜底，而不是来自路由表。
const portalCatchAll = "GET /"

// routeProbe 是一条待验证的端点声明。path 已把 {uid} 之类的通配段填成具体值。
type routeProbe struct {
	method string
	path   string // 不含前导 '/'，可带 query
	from   string // 来源，失败时报出来
}

// frozenContract 是冻结的接口契约：plan.md 的 A/B/C 三节 + 控制台绑定的既有端点。
//
// 它是**被检验的声明**：TestConsoleFrozenContractEndpointsExist 会拿它逐条去
// 真实路由表上核对，少一条就失败。所以这里的任何一条都必须真能命中。
var frozenContract = []routeProbe{
	// ---- 既有端点（第 1 轮之前就在 server.go 路由表里）----
	{"GET", "healthz", "既有"},
	{"GET", "status", "既有"},
	{"GET", "v1/models", "既有"},
	{"GET", "v1/credits", "既有"},
	{"GET", "v1/stats?date=2026-01-01", "既有"},
	{"POST", "v1/chat/completions", "既有"},
	{"POST", "admin/checkin", "既有"},
	{"POST", "admin/credits/refresh", "既有"},
	{"POST", "admin/waf/reset", "既有"},
	{"POST", "admin/degrade/reset", "既有"},
	{"POST", "admin/accounts/probe-uid/clear-cooldown", "既有"},
	{"POST", "admin/accounts/probe-uid/enable", "既有"},
	{"POST", "admin/accounts/probe-uid/disable", "既有"},
	{"POST", "admin/accounts/probe-uid/test", "既有"},
	{"POST", "admin/accounts/probe-uid/proxy", "既有"},
	{"DELETE", "admin/accounts/probe-name", "既有"},
	{"GET", "admin/accounts", "既有"},
	{"POST", "admin/accounts/import", "既有"},
	{"GET", "admin/accounts/export", "既有"},
	{"GET", "admin/accounts/import/status?job_id=probe", "既有"},
	{"POST", "admin/login/start", "既有"},
	{"GET", "admin/login/status?session_id=probe", "既有"},
	{"POST", "admin/login/cancel?session_id=probe", "既有"},
	{"GET", "admin/login/sessions", "既有"},
	{"POST", "admin/login/callback", "既有"},
	{"GET", "admin/users", "既有"},
	{"POST", "admin/users", "既有"},
	{"DELETE", "admin/users/probe-id", "既有"},
	{"POST", "admin/users/probe-id/reset-key", "既有"},
	{"POST", "admin/users/probe-id/role", "既有"},
	{"POST", "admin/users/probe-id/disable", "既有"},
	{"POST", "admin/users/probe-id/enable", "既有"},
	{"POST", "admin/users/probe-id/quota", "既有"},
	{"POST", "admin/users/probe-id/whitelist", "既有"},
	{"GET", "admin/password", "既有"},
	{"POST", "admin/password", "既有"},
	{"POST", "admin/dsh/start", "既有"},
	{"POST", "admin/dsh/stop", "既有"},
	{"GET", "admin/dsh/status", "既有"},
	{"POST", "login", "既有（控制台密码闸）"},
	{"POST", "logout", "既有（控制台密码闸）"},

	// ---- 契约 A：账号批量动作（owner: backend-accounts）----
	{"POST", "admin/accounts/revive", "契约A"},
	{"POST", "admin/accounts/control", "契约A"},
	{"POST", "admin/accounts/delete", "契约A"},

	// ---- 契约 B：认证与用户（owner: backend-auth）----
	{"GET", "admin/system", "契约B"},
	{"POST", "admin/system/allow_register", "契约B"},
	{"POST", "auth/register", "契约B"},
	{"POST", "auth/login", "契约B"},
	{"POST", "auth/logout", "契约B"},
	{"POST", "me/password", "契约B"},
	{"POST", "me/api-key/reset", "契约B"},
	{"POST", "admin/users/probe-id/reset-password", "契约B"},

	// ---- 契约 C：dsh 扩展 + 更新检测（owner: backend-dsh）----
	{"POST", "admin/dsh/install", "契约C"},
	{"POST", "admin/dsh/reset", "契约C"},
	{"GET", "admin/dsh/export", "契约C"},
	{"GET", "admin/system/update/check", "契约C"},
	{"POST", "admin/system/update/apply", "契约D"},
}

// consoleDynamicOverrides 处理"动态段其实是枚举值"的调用点。
//
// key = 把动态段归一成 '*' 之后的模板；value = 它可能取到的具体路径。
// 例：`post('admin/accounts/' + uid + '/' + action)` 里 action 的取值在调用点
// 之外，用占位值探不到任何真路由，必须在这里显式给出。
//
// 表里的条目会被反向核对：没人用到 = 过期条目，直接报错。
var consoleDynamicOverrides = map[string][]string{
	"admin/accounts/*/*": {
		"admin/accounts/probe-uid/enable",
		"admin/accounts/probe-uid/disable",
	},
}

// consoleRouteTable 取出**真实**路由表。
//
// 断言必须打在它上面。用 Server.handler()（未包安全头的 mux 本体）而不是
// s.Handler()，因为只有前者能给出 ServeMux 的匹配结果。
func consoleRouteTable(t *testing.T) *http.ServeMux {
	t.Helper()
	s := &Server{
		cfg:   &config.Config{APIKey: ""},
		pool:  pool.New(pool.Options{MaxInFlight: 1}),
		users: usermgr.New(filepath.Join(t.TempDir(), "users.json")),
	}
	mux, ok := s.handler().(*http.ServeMux)
	if !ok {
		t.Fatalf("Server.handler() 不再是 *http.ServeMux —— 端点闸失效，必须改这个测试")
	}
	return mux
}

// matchRoute 让**真实路由表**裁决一个请求。
//
// 返回命中的注册模式；未命中时返回 404/405（用于给出可读的失败信息）。
// 用 mux.Handler() 而不是真正执行 handler：走的是同一个 ServeMux 匹配器
// （含 Go 1.22 的方法限定与通配优先级规则），但不产生副作用。
func matchRoute(mux *http.ServeMux, method, target string) (pattern string, status int) {
	req := httptest.NewRequest(method, "/"+target, nil)
	h, pat := mux.Handler(req)
	// 门户兜底("GET /")会吞掉所有 GET 路径 —— 它不是一条真端点。
	if pat != "" && !(pat == portalCatchAll && req.URL.Path != "/") {
		return pat, 0
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req) // 只会是 NotFoundHandler 或 mux 内置的 405 处理器
	return "", w.Code
}

// consoleCall 是页面里的一处 HTTP 调用点。
type consoleCall struct {
	fn         string   // api / post / fetch
	method     string   // 实际 HTTP 方法
	template   string   // 拼接后的路径模板（动态段写成 {{...}}），仅用于报错定位
	normalized string   // 动态段归一成 '*' 的模板（consoleDynamicOverrides 的 key）
	probes     []string // 具体化后的探测路径
	src        string   // app.js 里的原始片段
}

var (
	consoleCallRe = regexp.MustCompile(`\b(api|post|fetch)\s*\(`)
	consoleMethRe = regexp.MustCompile(`method\s*:\s*['"]([A-Za-z]+)['"]`)
)

// probeToken 是路径变量占位值。路径通配段（{uid}/{id}/{name}）能被任何一段值命中，
// 但"动态段其实是枚举值"的调用点（例：uid 后跟 enable/disable）必须靠
// consoleDynamicOverrides 给出取值 —— 那时占位值会探测失败并报错。
const probeToken = "probe"

// extractConsoleCalls 从 app.js 里自动抽出每一个 HTTP 调用点。
//
// 它只做"找调用 + 拼路径字面量 + 具体化动态段"，不做任何合法性判断 ——
// 合法性一律交给真实路由表。
//
// 两种"抽不出来"的处理都是**不会静默放过**的：
//   - 动态段取值是枚举值（占位值探不中）→ 该调用点的探测会失败并点名要求补表；
//   - 括号不配平 / 参数为空 / 展开出异常条数 → 直接 t.Errorf。
//
// 唯一的例外是 api()/post()/fetch() 自身的函数定义（第一个参数是 `path`），显式跳过。
func extractConsoleCalls(t *testing.T, js string) []consoleCall {
	t.Helper()
	var out []consoleCall
	usedOverride := map[string]bool{}

	for _, loc := range consoleCallRe.FindAllStringSubmatchIndex(js, -1) {
		fn := js[loc[2]:loc[3]]
		open := loc[1] - 1
		end := matchBracket(js, open)
		if end < 0 {
			t.Errorf("app.js 里 %s( 的括号不配平（offset %d）", fn, open)
			continue
		}
		callText := js[open+1 : end]
		arg := strings.TrimSpace(splitTopLevel(callText, ',')[0])
		if arg == "path" {
			continue // 这三个是 api/post/fetch 自身的函数定义
		}
		if arg == "" {
			t.Errorf("app.js 里 %s( 的第一个参数为空：%.60q", fn, callText)
			continue
		}

		method := "GET"
		if fn == "post" {
			method = "POST"
		} else if m := consoleMethRe.FindStringSubmatch(callText); m != nil {
			method = strings.ToUpper(m[1])
		}

		parts := splitTopLevelAll(arg, '+')
		tmplParts := make([]string, 0, len(parts))
		normalized := ""
		options := make([][]string, 0, len(parts))
		for _, raw := range parts {
			p := strings.TrimSpace(raw)
			lits, dyn := jsPartLiterals(p)
			switch {
			case dyn || len(lits) == 0:
				// 抽不出值的动态段：先用占位 token 探测（路径变量都能被任何一段值命中）。
				// 只有"动态段其实是枚举值"的情况才会因此探测失败 —— 那种调用点
				// 必须写进 consoleDynamicOverrides，失败信息里会直接点出来。
				normalized += "*"
				tmplParts = append(tmplParts, "{{"+p+"}}")
				options = append(options, []string{probeToken})
			default:
				normalized += strings.Join(lits, "|")
				tmplParts = append(tmplParts, strings.Join(lits, "|"))
				options = append(options, lits)
			}
		}

		call := consoleCall{
			fn:         fn,
			method:     method,
			template:   strings.Join(tmplParts, ""),
			normalized: normalized,
			src:        strings.Join(strings.Fields(js[loc[0]:end+1]), " "),
		}
		if probes, ok := consoleDynamicOverrides[normalized]; ok {
			usedOverride[normalized] = true
			call.probes = probes
		} else {
			probes := []string{""}
			for _, opts := range options {
				var next []string
				for _, base := range probes {
					for _, o := range opts {
						next = append(next, base+o)
					}
				}
				probes = next
			}
			if len(probes) == 0 || len(probes) > 8 {
				t.Errorf("调用点展开出的探测路径数量异常（%d）：%s", len(probes), call.src)
				continue
			}
			call.probes = probes
		}
		out = append(out, call)
	}

	// 反向核对：表里的每一项都必须真的被某个调用点用到（防止过期条目长期潜伏）。
	for k := range consoleDynamicOverrides {
		if !usedOverride[k] {
			t.Errorf("consoleDynamicOverrides 里的 %q 没有任何调用点用到 —— 过期条目，请删除", k)
		}
	}
	return out
}

// matchBracket 返回 s[open] 对应右括号的下标（open 必须指向 '(' 或 '[' 或 '{'）。
func matchBracket(s string, open int) int {
	pairs := map[byte]byte{'(': ')', '[': ']', '{': '}'}
	closer, ok := pairs[s[open]]
	if !ok {
		return -1
	}
	depth := 0
	for i := open; i < len(s); i++ {
		c := s[i]
		if c == '\'' || c == '"' || c == '`' {
			i = skipJSString(s, i)
			if i < 0 {
				return -1
			}
			continue
		}
		switch c {
		case s[open]:
			depth++
		case closer:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// skipJSString 返回从 s[i]（引号）开始的字符串字面量的收尾下标；未闭合返回 -1。
func skipJSString(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		if s[j] == '\\' {
			j++
			continue
		}
		if s[j] == q {
			return j
		}
	}
	return -1
}

// splitTopLevel 按 sep 切一刀（只在括号/引号之外切），返回前后两段。
func splitTopLevel(s string, sep byte) [2]string {
	parts := splitTopLevelAll(s, sep)
	if len(parts) <= 1 {
		return [2]string{s, ""}
	}
	return [2]string{parts[0], strings.Join(parts[1:], string(sep))}
}

// splitTopLevelAll 按 sep 全切（只在括号/引号之外切）。
func splitTopLevelAll(s string, sep byte) []string {
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' || c == '"' || c == '`' {
			j := skipJSString(s, i)
			if j < 0 {
				break
			}
			i = j
			continue
		}
		switch c {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case sep:
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// jsPartLiterals 判断一个路径片段能取到哪些字面量。
//
//   - `'admin/accounts/'`          → (["admin/accounts/"], false)
//   - `encodeURIComponent(uid)`    → (nil, true)      —— 抽不出值
//   - `(x ? 'disable' : 'enable')` → (["disable","enable"], false)
func jsPartLiterals(p string) (lits []string, dynamic bool) {
	if p == "" {
		return nil, true
	}
	// 纯字面量
	if q := p[0]; q == '\'' || q == '"' || q == '`' {
		if end := skipJSString(p, 0); end == len(p)-1 {
			body := p[1:end]
			if q == '`' && strings.Contains(body, "${") {
				return nil, true
			}
			return []string{body}, false
		}
	}
	// 表达式里嵌的字面量（三元、数组下标之类）
	for i := 0; i < len(p); i++ {
		q := p[i]
		if q != '\'' && q != '"' && q != '`' {
			continue
		}
		j := skipJSString(p, i)
		if j < 0 {
			return nil, true
		}
		body := p[i+1 : j]
		if q == '`' && strings.Contains(body, "${") {
			return nil, true
		}
		lits = append(lits, body)
		i = j
	}
	if len(lits) == 0 {
		return nil, true
	}
	return lits, false
}

// consoleNoExec 是"路由上确实存在、但不能在单测里真发请求"的端点。
//
// 理由必须写在这里：这些 handler 会拉起子进程、开 OAuth 会话或写运行期文件，
// 单测里实跑一遍是**副作用**（可能真的启动 node、真的下载东西）。
// 它们的路由存在性由 mux 匹配层（matchRoute）负责 —— 那一层同样是真实路由表
// 在裁决，不需要真跑 handler 才能证明"这条路由存在"。
//
// key 是真实路由表返回的注册模式（形如 "POST /admin/dsh/start"）。
var consoleNoExec = map[string]string{
	"POST /admin/dsh/start":           "会拉起 node 子进程",
	"POST /admin/dsh/stop":            "会操作真实子进程",
	"POST /admin/dsh/install":         "会 exec 探测本机 node/desktop（契约也要求它绝不下载）",
	"POST /admin/dsh/reset":           "会停进程并清理运行态",
	"POST /admin/login/start":         "会开启 OAuth 登录会话（等回调 / 可能拉起浏览器）",
	"POST /admin/login/callback":      "会与上游通信换取凭据",
	"POST /admin/password":            "会写 data/console.json（控制台密码）",
	"POST /admin/system/update/apply": "会下载更新并替换执行文件及重启进程",
}

// consoleProbeServer 起一个真实的 HTTP 服务用于**实发请求**。
//
// 刻意用"裸 Server"而不是 server.New(cfg)：
//   - New() 会加载本机 data/accounts 里的**真实账号**，于是 /admin/checkin、
//     /admin/credits/refresh 会真的往上游发请求（实测慢 7 秒）；
//   - 裸 Server 的账号池是空的，所有端点要么 4xx 要么不做事 —— 单测里不出网。
//
// accounts 用 t.TempDir() 下的空快照存储：给 /admin/accounts、/admin/accounts/export
// 一个真实的（空的）存储，避免它们因为 nil 而 panic，同时不碰仓库里的任何数据。
func consoleProbeServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	s := &Server{
		cfg:      &config.Config{APIKey: ""},
		pool:     pool.New(pool.Options{MaxInFlight: 1}),
		users:    usermgr.New(filepath.Join(t.TempDir(), "users.json")),
		accounts: accountstore.New(filepath.Join(t.TempDir(), "accounts")),
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, &http.Client{Timeout: 5 * time.Second}
}

// realProbe 实发一次请求。
//
// 只把 **ServeMux 的兜底响应**（"404 page not found" / "405 method not allowed"，
// 也就是"这条路径没有被任何真端点服务"）算作失败；handler 自己返回的 404
// （例如"账号不存在"，通常是 JSON）说明请求已经到达了真实 handler，不算失败。
func realProbe(c *http.Client, base, method, target string) (status int, body string, err error) {
	reqPath := "/" + strings.TrimPrefix(target, "/")
	req, err := http.NewRequest(method, base+reqPath, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return resp.StatusCode, string(raw), nil
}

// isMuxFallback 判断响应是不是 ServeMux/门户兜底（= 路径没被真端点服务）。
//
// 只认 Go 标准库兜底响应那两句话：handler 自己返回的 404（"账号不存在"之类，
// 通常是 JSON）说明请求已经到达真实 handler，不算失败。
func isMuxFallback(status int, body string) bool {
	switch status {
	case http.StatusNotFound:
		return strings.HasPrefix(body, "404 page not found")
	case http.StatusMethodNotAllowed:
		return strings.HasPrefix(body, "405 method not allowed")
	}
	return false
}

// TestConsoleFrontendEndpointsAreRouted —— 正向闸：页面调用的每个端点，
// 都必须能在**真实路由表**上命中；除 consoleNoExec 里写明理由的那几个之外，
// 还要对真实 Handler 实发一次请求，断言拿到的不是路由兜底 404/405。
func TestConsoleFrontendEndpointsAreRouted(t *testing.T) {
	mux := consoleRouteTable(t)
	calls := extractConsoleCalls(t, consoleAppJS)
	if len(calls) < 40 {
		t.Fatalf("只从 app.js 抽到 %d 个调用点（预期 ≥40）—— 抽取器已失效，端点闸形同虚设", len(calls))
	}
	ts, client := consoleProbeServer(t)

	probes, executed, skipped := 0, 0, 0
	seenNoExec := map[string]bool{}
	for _, c := range calls {
		for _, p := range c.probes {
			probes++
			pat, status := matchRoute(mux, c.method, p)
			if pat == "" {
				t.Errorf("前端调用了**路由表上不存在**的端点：%s /%s（真实路由表返回 %d）\n"+
					"  出处：%s\n"+
					"  若这个端点的动态段其实是枚举值，请在 consoleDynamicOverrides 里给 %q 补上具体取值。",
					c.method, p, status, c.src, c.normalized)
				continue
			}
			t.Logf("路由命中  %-6s %-46s → %q", c.method, p, pat)

			// pat 是 ServeMux 返回的注册模式，本身已带方法（"POST /admin/dsh/start"）
			key := pat
			if why, noExec := consoleNoExec[key]; noExec {
				skipped++
				seenNoExec[key] = true
				t.Logf("跳过实发  %-6s %-46s（%s）", c.method, p, why)
				continue
			}
			executed++
			got, body, err := realProbe(client, ts.URL, c.method, p)
			switch {
			case err != nil:
				t.Logf("实发请求  %-6s %-46s → 无响应（%v）", c.method, p, err)
			case isMuxFallback(got, body):
				t.Errorf("实发请求 %s /%s 拿到路由兜底响应 %d（body=%q）—— 该端点是坏的\n  出处：%s",
					c.method, p, got, strings.TrimSpace(body), c.src)
			default:
				t.Logf("实发请求  %-6s %-46s → HTTP %d", c.method, p, got)
			}
		}
	}
	// 反向核对：consoleNoExec 里不能有过期条目（否则它会悄悄豁免不存在的端点）
	for k := range consoleNoExec {
		if seenNoExec[k] {
			continue
		}
		if _, ok := contractPatternSet(t, mux)[k]; !ok {
			t.Errorf("consoleNoExec 里的 %q 既不在前端调用里也不在契约里 —— 过期条目，请删除", k)
		}
	}
	t.Logf("共抽取 %d 个调用点 / %d 条探测路径；实发请求 %d 条，按理由跳过 %d 条（consoleNoExec）",
		len(calls), probes, executed, skipped)
}

// contractPatternSet 返回契约端点命中过的注册模式集合。
//
// key 直接用 ServeMux 返回的模式串（形如 "POST /admin/dsh/start"），
// 不做任何拼接/改写 —— 免得"两边用同一种错误拼法"又变成一次自欺。
func contractPatternSet(t *testing.T, mux *http.ServeMux) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, ep := range frozenContract {
		if pat, _ := matchRoute(mux, ep.method, ep.path); pat != "" {
			out[pat] = ep.from
		}
	}
	return out
}

// TestConsoleFrozenContractEndpointsExist —— 反向闸：冻结契约里的每个端点都必须
// 真实存在。这一条就是"契约端点存在性"的集成检查：任何一个角色少实现一条，
// 这里当场红，而不是等下一轮验收才发现（第 1 轮 B4 的教训）。
func TestConsoleFrozenContractEndpointsExist(t *testing.T) {
	mux := consoleRouteTable(t)
	for _, ep := range frozenContract {
		pat, status := matchRoute(mux, ep.method, ep.path)
		if pat == "" {
			t.Errorf("冻结契约端点不存在：%s /%s（真实路由表返回 %d）—— 该契约未落地。来源：%s",
				ep.method, ep.path, status, ep.from)
			continue
		}
		t.Logf("契约端点  [%-8s] %-6s %-46s → %q", ep.from, ep.method, ep.path, pat)
	}
}

// TestConsoleEndpointGateCanFail —— 闸门必须**可被证伪**。
//
// 第 1 轮的教训是"闸门看起来在跑，其实永远绿"。所以这里反向自检：
// 拿几个**确定不存在**的端点走同一套判定，必须被判为"未命中"。
// 如果哪天这条测试红了，说明 matchRoute 的判定烂掉了（比如门户兜底
// "GET /" 又把不存在的 GET 路径吞了）—— 那才是端点闸失效的警报。
func TestConsoleEndpointGateCanFail(t *testing.T) {
	mux := consoleRouteTable(t)
	negative := []struct{ method, path string }{
		{"POST", "admin/accounts/definitely-not-implemented"},
		{"POST", "admin/accounts/probe-uid/definitely-not-implemented"},
		{"POST", "admin/dsh/definitely-not-implemented"},
		{"GET", "admin/definitely-not-implemented"},       // 只能靠门户兜底 404 识别
		{"GET", "admin/dsh/definitely-not-implemented"},   // 同上
		{"GET", "v1/definitely-not-implemented"},          // 同上
		{"DELETE", "admin/users/probe-id/definitely-not"}, // 方法不匹配 + 路径不存在
	}
	for _, c := range negative {
		pat, status := matchRoute(mux, c.method, c.path)
		if pat != "" {
			t.Errorf("负向自检失败：不存在的端点 %s /%s 竟然命中路由 %q —— "+
				"端点闸无法证伪，等于没有闸门", c.method, c.path, pat)
			continue
		}
		t.Logf("负向自检  %-6s %-52s → 未命中（HTTP %d）", c.method, c.path, status)
	}
}

// TestConsoleFrontendEndpointsAreInContract —— 归属闸：页面命中的路由必须落在
// 契约端点命中过的路由集合里。
//
// 两侧的 pattern 都由**真实路由表**给出，所以这不是"字符串比对白名单"，
// 而是"路由表自己说这两个请求命中了同一条规则"。
func TestConsoleFrontendEndpointsAreInContract(t *testing.T) {
	mux := consoleRouteTable(t)
	contractPatterns := contractPatternSet(t, mux)

	// 参考实现的 /ui/* 一个都不能残留（照抄会导致整页按钮全坏）
	if m := regexp.MustCompile(`(?i)(src|href)\s*=\s*["']/ui/`).FindString(consoleHTML); m != "" {
		t.Errorf("页面里有指向 /ui/* 的资源引用 %q", m)
	}

	for _, c := range extractConsoleCalls(t, consoleAppJS) {
		if strings.Contains(c.template, "/ui/") || strings.HasPrefix(c.template, "ui/") {
			t.Errorf("前端仍在调用参考实现的端点：%s", c.src)
			continue
		}
		for _, p := range c.probes {
			pat, _ := matchRoute(mux, c.method, p)
			if pat == "" {
				continue // 已由 TestConsoleFrontendEndpointsAreRouted 报错
			}
			if _, ok := contractPatterns[pat]; !ok {
				t.Errorf("前端调用了契约之外的路由：%s /%s 命中 %q\n  出处：%s",
					c.method, p, pat, c.src)
			}
		}
	}
}
