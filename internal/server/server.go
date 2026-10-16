// Package server 暴露 OpenAI 兼容 HTTP 接口。
//
// 路由一览：
//
//	GET  /healthz                 健康检查（无需鉴权）
//	GET  /v1/models               模型列表（含 trae:/solo: 前缀与能力字段）
//	POST /v1/chat/completions     对话（流式 / 非流式，支持原生工具调用）
//	GET  /status                  运行状态（账号池、冷却、渠道）
//	GET  /                        简易控制台（只读状态页）
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"trae2api/internal/accountstore"
	"trae2api/internal/config"
	"trae2api/internal/docparse"
	"trae2api/internal/metrics"
	"trae2api/internal/pool"
	"trae2api/internal/prompt"
	"trae2api/internal/realm"
	"trae2api/internal/session"
	"trae2api/internal/traectl"
	"trae2api/internal/usermgr"
)

// newDocExpander 依据配置构造附件展开器。
//
// 安全：默认关闭；Enabled=true 但 AllowedRoots 为空时，docparse.New 会**强制关闭**
// （fail-closed）—— 配置写漏了必须变成"功能不可用"，而不是"开放整个文件系统"。
func newDocExpander(cfg *config.Config) *docparse.Expander {
	opt := docparse.DefaultOptions()
	opt.Enabled = cfg.Files.Enabled
	opt.AllowedRoots = cfg.Files.AllowedRoots
	if cfg.Files.MaxFileBytes > 0 {
		opt.MaxFileBytes = cfg.Files.MaxFileBytes
	}
	if cfg.Files.MaxTotalBytes > 0 {
		opt.MaxTotalBytes = cfg.Files.MaxTotalBytes
	}
	if cfg.Files.MaxFiles > 0 {
		opt.MaxFiles = cfg.Files.MaxFiles
	}
	e := docparse.New(opt)
	if cfg.Files.Enabled && !e.Enabled() {
		log.Printf("WARN: files.enabled=true 但 allowed_roots 为空 —— 附件展开已强制关闭。" +
			" 这是刻意的 fail-closed：没有根目录白名单就开放文件读取等于把本机文件暴露给调用方。")
	} else if e.Enabled() {
		log.Printf("INFO: 附件展开已启用，允许根目录：%v", e.Roots())
	}
	return e
}

// newPromptRewriter 依据配置构造 System Prompt 改写器。
//
// 配置里的 mode 已在校验阶段确认合法；这里再 ParseMode 一次是为了拿到
// prompt.Mode 类型（配置侧存的是字符串，避免 config 包依赖 prompt 的类型）。
func newPromptRewriter(cfg *config.Config) *prompt.Rewriter {
	mode, err := prompt.ParseMode(cfg.Prompt.Mode)
	if err != nil {
		// 不该发生（配置校验已拦），但真发生时不静默：回落 passthrough 并留痕
		log.Printf("WARN: prompt.mode 非法（%v），已回落到 passthrough", err)
		mode = prompt.ModePassthrough
	}
	r := prompt.New(mode, cfg.Prompt.Text)
	if r.Enabled() {
		log.Printf("INFO: System Prompt 改写已启用：mode=%s，文本 %d 字", mode, len(cfg.Prompt.Text))
	}
	return r
}

// Version 网关版本。
//
// ⚠️ 必须是 **var** 而不是 const —— 发布流程用
// `-ldflags "-X trae2api/internal/server.Version=$TAG"` 注入真实版本号，
// 而 `-X` **只能改 string 变量，改不了常量**。
//
// 曾经这里是 const，于是 release.yml 里那句"-X 覆盖 var"的注释是假的
// （LDFLAGS 里根本没有 -X），后果是发 v0.2.0 时 Release 标题写着 v0.2.0、
// 二进制 `version` 却报 0.1.0。发布流程从没跑过，所以一直没人发现。
//
// 这个默认值是**本地 go build 出来的**二进制会报的版本（不含 tag 信息）。
var Version = "0.2.2"

// ServiceName 网关身份标识（/healthz 透出，便于宿主识别"假成功"）。
const ServiceName = "trae2api"

// Server 是网关实例。
type Server struct {
	cfg      *config.Config
	pool     *pool.Pool
	channels map[string]*Channel
	sessions *session.Router
	// users 用户与角色管理（多租户 RBAC）。为空表示未启用用户体系。
	users *usermgr.Manager
	// accounts 便携凭据快照存储（导入/导出）。
	accounts *accountstore.Store
	// metrics 指标聚合器（可为 nil = 不采集）。
	metrics *metrics.Collector
	// wafIP WAF IP 级拦截的 fail-fast 闸（进程内状态，重启清零）。
	wafIP *wafIPGate
	// prompt System Prompt 改写器（passthrough 时不做任何改写）。
	prompt *prompt.Rewriter
	// docs 附件（文件引用）展开器。未配置根目录时自动关闭。
	docs *docparse.Expander
	// logins OAuth 一键登录的会话管理。
	logins *loginManager
	// degrade 提示词降级闸（内容被上游拦时切中性提示词到次日零点）。
	degrade *degradeGate
	// webAuth 控制台密码闸（config.web_password；未配置时免密）。
	webAuth *webAuth
	// loginLimit 控制台登录的按 IP 失败限流（防暴力破解）。
	loginLimit *loginLimiter
	// imports 导入后的异步校验任务。
	imports *importValidator
	// consoleSecret 运行时控制台密码（data/console.json，优先于 config）。
	consoleSecret *consoleSecretStore

	// dsh 内嵌的 DeepSeek Harness 智能体的进程管理器（见 dsh.go）。
	//
	// 懒创建：不碰「Agent (dsh)」标签页的部署完全不为它付出代价，
	// 测试里直接构造的 Server（不跑 New）也能用这些端点。
	dshMu sync.Mutex
	dsh   *dshManager

	maxRotate     int
	modelCooldown time.Duration
	webhook       *WebhookNotifier

	mu sync.RWMutex
	// 运行时统计
	startedAt time.Time
	requests  int64
}

// New 依据配置构造网关（打开账号、加载模型目录、注册进池）。
func New(cfg *config.Config) (*Server, error) {
	breakerCD, err := cfg.BreakerCooldownDur()
	if err != nil {
		return nil, err
	}
	softBase, err := cfg.SoftRateDur()
	if err != nil {
		return nil, err
	}
	softMax, err := cfg.SoftRateMaxDur()
	if err != nil {
		return nil, err
	}
	modelCD, err := cfg.ModelCooldownDur()
	if err != nil {
		return nil, err
	}
	pickGap, err := cfg.PickGapDur()
	if err != nil {
		return nil, err
	}
	stickyTTL, err := cfg.StickyTTL()
	if err != nil {
		return nil, err
	}

	p := pool.New(pool.Options{
		MaxInFlight:      cfg.Pool.MaxInFlight,
		BreakerThreshold: cfg.Pool.BreakerThreshold,
		BreakerCooldown:  breakerCD,
		IdlePerHour:      cfg.Pool.IdleWeightPerHour,
		IdleMax:          cfg.Pool.IdleWeightMax,
		PickGap:          pickGap,
		SoftRateBase:     softBase,
		SoftRateMax:      softMax,
		ModelBase:        modelCD,
		StateFile:        cfg.StateFile(),
	})

	// 注入导入校验的真实动作（见 importcheck.go）
	// 注意：必须在 New 里做，且 checker 是包级变量 —— 它只在本包内使用，
	// 这样 importcheck.go 不必依赖 Server 的字段布局。
	// （放在 s 构造之后，见下方 s.wireImportChecker()）
	//
	// 注册国际版产品（必须在 BuildChannels 之前 —— 它按 Products 推导账号清单）。
	//
	// 未启用或配置不全时**不注册**，上层会明确看到"没有 intl 渠道"，
	// 而不是拿到一个指向错误主机的渠道。
	if err := traectl.ConfigureIntl(traectl.IntlConfig{
		Enabled:      cfg.Intl.Enabled,
		UGHost:       cfg.Intl.UGHost,
		ChatHost:     cfg.Intl.ChatHost,
		TraeClientID: cfg.Intl.TraeClientID,
		SoloClientID: cfg.Intl.SoloClientID,
	}); err != nil {
		log.Printf("WARN: 国际版未启用: %v", err)
	} else if cfg.Intl.Enabled {
		log.Printf("INFO: 国际版已启用（UG=%s chat=%s）", cfg.Intl.UGHost, cfg.Intl.ChatHost)
	}

	// 便携凭据快照存储（导入/导出）：先建好，BuildChannels 会从中加载账号
	accStore := accountstore.New(cfg.AccountsDir())

	// 指标聚合器（含 byDay 保留窗口；持久化与池状态分开，可独立清理）
	metricsRetain := cfg.Metrics.RetainDays
	if metricsRetain == 0 {
		metricsRetain = 14
	}
	mc := metrics.New(metrics.Options{
		RetainDays: metricsRetain,
		StateFile:  cfg.MetricsFile(),
	})
	if err := mc.Restore(); err != nil {
		log.Printf("WARN: 恢复指标快照失败（按全新开始）: %v", err)
	}

	channels, err := BuildChannels(cfg, p, accStore)
	if err != nil {
		return nil, err
	}

	// 恢复上次运行的冷却/熔断/禁用状态（账号不存在则自动跳过）
	if err := p.Restore(); err != nil {
		log.Printf("WARN: 恢复池状态失败（忽略，按全新启动处理）: %v", err)
	}

	s := &Server{
		cfg: cfg, pool: p, channels: channels, accounts: accStore, metrics: mc,
		wafIP:         newWAFIPGate(defaultWAFIPConfig()),
		prompt:        newPromptRewriter(cfg),
		docs:          newDocExpander(cfg),
		logins:        newLoginManager(),
		degrade:       &degradeGate{},
		webAuth:       newWebAuth(cfg.WebPassword),
		consoleSecret: newConsoleSecretStore(cfg.DataDir),
		loginLimit:    newLoginLimiter(),
		imports:       newImportValidator(),
		webhook:       newWebhookNotifier(cfg.Webhook.URL, cfg.Webhook.Secret, cfg.Webhook.Enabled),
		maxRotate:     3,
		modelCooldown: modelCD,
		startedAt:     time.Now(),
	}
	if cfg.SessionSticky.Enabled {
		s.sessions = session.New(stickyTTL)
	}
	// 用户与角色管理：即使没有用户文件也创建管理器（便于运行时创建首个管理员）
	s.users = usermgr.New(cfg.UsersFile())
	if err := s.users.Load(); err != nil {
		// 用户文件损坏不该让网关起不来：以空用户表启动并告警
		log.Printf("WARN: 加载用户文件失败（将以空用户表启动）: %v", err)
	}
	if n := s.users.Count(); n > 0 {
		log.Printf("INFO: 已加载 %d 个用户（多租户 RBAC 已启用）", n)
	}
	// 运行时密码优先于 config：data/console.json 存在就用它（含"空密码 = 已清除"）。
	//
	// 为什么要区分"文件不存在"与"文件里是空串"：
	//   - 不存在 → 还没改过，用 config.web_password
	//   - 存在且为空 → 用户主动清除了密码，**必须**恢复免密
	// 把两者混同会导致"用户清了密码，重启后 config 里的旧密码又生效了"。
	if pw, ok, err := s.consoleSecret.Load(); err != nil {
		log.Printf("WARN: 读取运行时控制台密码失败（回落到 config）: %v", err)
	} else if ok {
		s.webAuth.password = pw
		if pw == "" {
			log.Printf("INFO: 控制台密码已按 data/console.json 清除（单机免密）")
		} else {
			log.Printf("INFO: 控制台密码取自 data/console.json（运行时设置，优先于 config）")
		}
	}

	// 注入导入校验的真实动作（包级变量，见 importcheck.go 的说明）
	s.wireImportChecker()

	return s, nil
}

// Pool 暴露账号池（供定时任务等外部组件使用）。
func (s *Server) Pool() *pool.Pool { return s.pool }

// Channels 暴露渠道表。
func (s *Server) Channels() map[string]*Channel { return s.channels }

// Handler 返回带全部路由的 http.Handler。
// Handler 返回带**全局安全响应头**的 HTTP 处理器。
//
// 安全头必须加在最外层（所有路由都过），而不是逐个 handler 里设 ——
// 逐个设必然会漏（我自己就漏了：只有导出端点设了 nosniff）。
func (s *Server) Handler() http.Handler {
	return withSecurityHeaders(s.handler())
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /v1/models", s.withAuth(s.handleModels))
	// 对话入口先过 handleChatRouter：它只做一件事 —— 把虚拟智能体模型
	// `model: dsh` 拦下来（见 dsh.go），其余请求原样交给 handleChat。
	mux.HandleFunc("POST /v1/chat/completions", s.withAuth(s.handleChatRouter))
	mux.HandleFunc("POST /v1/messages", s.withAuth(s.handleAnthropicMessages))
	mux.HandleFunc("GET /status", s.withAuth(s.handleStatus))
	mux.HandleFunc("GET /v1/credits", s.withAuth(s.handleCredits))
	mux.HandleFunc("GET /v1/stats", s.withAuth(s.handleStats))
	mux.HandleFunc("POST /v1/stats/reset", s.withAuth(s.handleStatsReset))
	// 管理端点（要求管理员权限：用户专属 Key 需 role=admin，或用全局 api_key）
	mux.HandleFunc("POST /admin/checkin", s.withAuth(s.handleCheckin))
	mux.HandleFunc("POST /admin/credits/refresh", s.withAuth(s.handleRefreshCredits))
	mux.HandleFunc("POST /admin/waf/reset", s.withAuth(s.handleWAFReset))
	mux.HandleFunc("POST /admin/degrade/reset", s.withAuth(s.handleDegradeReset))
	mux.HandleFunc("POST /admin/accounts/{uid}/clear-cooldown", s.withAuth(s.handleClearCooldown))
	mux.HandleFunc("POST /admin/accounts/{uid}/disable", s.withAuth(s.handleSetDisabled(true)))
	mux.HandleFunc("POST /admin/accounts/{uid}/enable", s.withAuth(s.handleSetDisabled(false)))
	mux.HandleFunc("POST /admin/accounts/{uid}/test", s.withAuth(s.handleAccountTest))
	mux.HandleFunc("POST /admin/accounts/{uid}/proxy", s.withAuth(s.handleAccountProxy))
	// OAuth 一键登录
	mux.HandleFunc("POST /admin/login/start", s.withAuth(s.handleLoginStart))
	mux.HandleFunc("GET /admin/login/status", s.withAuth(s.handleLoginStatus))
	mux.HandleFunc("POST /admin/login/cancel", s.withAuth(s.handleLoginCancel))
	mux.HandleFunc("GET /admin/login/sessions", s.withAuth(s.handleLoginList))
	mux.HandleFunc("POST /admin/login/callback", s.withAuth(s.handleLoginSubmitCallback))

	// 内嵌 DeepSeek Harness (dsh) 智能体（见 dsh.go；install/reset/export 见 dsh_extra.go）。
	// start/stop 都是幂等的：重复 start 不会产生第二个进程，
	// 重复 stop 也不会报错 —— 控制台的按钮会被连点，必须能扛住。
	mux.HandleFunc("POST /admin/dsh/start", s.withAuth(s.handleDSHStart))
	mux.HandleFunc("POST /admin/dsh/stop", s.withAuth(s.handleDSHStop))
	mux.HandleFunc("GET /admin/dsh/status", s.withAuth(s.handleDSHStatus))
	// install 只检测环境（**绝不下载 Node.js**）；reset 停进程 + 清日志与运行态
	// （不删用户数据）；export 是 text/plain 附件（文件名过滤过，防响应头注入）。
	// 三者都要求管理员 —— 与上面的启停一致。
	mux.HandleFunc("POST /admin/dsh/install", s.withAuth(s.handleDSHInstall))
	mux.HandleFunc("POST /admin/dsh/reset", s.withAuth(s.handleDSHReset))
	mux.HandleFunc("GET /admin/dsh/export", s.withAuth(s.handleDSHExport))

	// 账号导入/导出（便携快照）
	mux.HandleFunc("GET /admin/accounts", s.withAuth(s.handleAccountsList))
	mux.HandleFunc("POST /admin/accounts/import", s.withAuth(s.handleImport))
	mux.HandleFunc("GET /admin/accounts/export", s.withAuth(s.handleExport))
	mux.HandleFunc("DELETE /admin/accounts/{name}", s.withAuth(s.handleAccountDelete))
	// 导入后的异步校验进度（轮询用；恒返 200，见 handleImportStatus）
	mux.HandleFunc("GET /admin/accounts/import/status", s.withAuth(s.handleImportStatus))
	// 批量动作（见 accounts_ui.go）。注意与上面的 DELETE /admin/accounts/{name}
	// 共存是安全的：这三条是无通配的具体路径，ServeMux 会优先精确匹配；
	// 而 {name} 只匹配单个路径段，POST /admin/accounts/revive 命中新注册的精确模式。
	mux.HandleFunc("POST /admin/accounts/revive", s.withAuth(s.handleAccountsRevive))
	mux.HandleFunc("POST /admin/accounts/control", s.withAuth(s.handleAccountsControl))
	mux.HandleFunc("POST /admin/accounts/delete", s.withAuth(s.handleAccountsDelete))

	// 用户与角色管理
	//
	// 必须按方法分别注册：Go 1.22+ 的 ServeMux 拒绝「无方法限定的具体路径」
	// 与「有方法限定的通用路径 GET /」共存（会 panic：
	// "GET / matches fewer methods than /admin/users, but has a more general path pattern"）。
	// 这个 panic 只在真正启动时才暴露，编译期与单测都不会发现。
	mux.HandleFunc("GET /admin/users", s.withAuth(s.handleUsers))
	mux.HandleFunc("POST /admin/users", s.withAuth(s.handleUsers))
	mux.HandleFunc("DELETE /admin/users/{id}", s.withAuth(s.handleUserDelete))
	mux.HandleFunc("POST /admin/users/{id}/reset-key", s.withAuth(s.handleUserAction("reset-key")))
	mux.HandleFunc("POST /admin/users/{id}/role", s.withAuth(s.handleUserAction("role")))
	mux.HandleFunc("POST /admin/users/{id}/disable", s.withAuth(s.handleUserAction("disable")))
	mux.HandleFunc("POST /admin/users/{id}/enable", s.withAuth(s.handleUserAction("enable")))
	mux.HandleFunc("POST /admin/users/{id}/quota", s.withAuth(s.handleUserAction("quota")))
	mux.HandleFunc("POST /admin/users/{id}/whitelist", s.withAuth(s.handleUserAction("whitelist")))
	mux.HandleFunc("POST /admin/users/{id}/reset-password", s.withAuth(s.handleUserResetPassword))

	// 用户密码认证（见 auth.go）：注册 / 登录 / 登出 / 自助改密 / 自助换 Key。
	//
	// ⚠️ GET /admin/system **故意不挂 withAuth**：登录/注册弹窗需要在还没有任何
	// 凭证的时候就问"要不要显示注册入口"。它只透出 version / service /
	// allow_register，前两者 /healthz 早已公开。真正的门控在 /auth/register 内
	// （服务端二次判定），不依赖前端隐藏按钮。
	mux.HandleFunc("GET /admin/system", s.handleSystemInfo)
	// 系统更新检测（见 dsh_extra.go）。与 GET /admin/system 一样**公开只读**：
	// 只回三个常量（supported / reason / current），不碰密钥与用户数据；
	// 而控制台在还没有凭证时就会调它（页面加载 2 秒后拉版本号），挂鉴权会让
	// "检查更新"模态先报一次失败 —— 那正是要修掉的症状。
	mux.HandleFunc("GET /admin/system/update/check", s.handleUpdateCheck)
	mux.HandleFunc("POST /admin/system/update/apply", s.withAuth(s.handleUpdateApply))
	mux.HandleFunc("POST /admin/system/restart", s.withAuth(s.handleSystemRestart))
	mux.HandleFunc("POST /admin/system/allow_register", s.withAuth(s.handleSetAllowRegister))
	mux.HandleFunc("POST /auth/register", s.handleRegister)
	mux.HandleFunc("POST /auth/login", s.handleLogin)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.HandleFunc("POST /me/password", s.handleMePassword)
	mux.HandleFunc("POST /me/api-key/reset", s.handleMeAPIKeyReset)
	// 公开门户（无需鉴权，只透出脱敏的可用性信息）
	mux.HandleFunc("GET /", s.handlePortal)
	mux.HandleFunc("GET /portal/status", s.handlePortalStatus)
	mux.HandleFunc("GET /ui/public/status", s.handlePortalStatus)

	// 管理中枢（受 web_password 保护）
	mux.HandleFunc("GET /console", s.handleConsole)
	// 别名：参考实现同时用 /console 与 /dashboard，用户习惯不一
	mux.HandleFunc("GET /dashboard", s.handleConsole)
	// 控制台密码闸（config.web_password）。未配置时这两个端点仍存在但只做跳转。
	mux.HandleFunc("POST /login", s.handleConsoleLogin)
	mux.HandleFunc("POST /logout", s.handleConsoleLogout)
	// 控制台密码的运行时修改
	mux.HandleFunc("POST /admin/password", s.withAuth(s.handlePasswordChange))
	mux.HandleFunc("GET /admin/password", s.withAuth(s.handlePasswordStatus))
	return mux
}

// contextWithTimeout 从请求派生带超时的 ctx（客户端断开时一并取消）。
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// logf 统一日志前缀。
func (s *Server) logf(format string, args ...any) {
	log.Printf(format, args...)
}

// ---- 鉴权（RBAC）----

// ctxKey 是本包在请求 Context 中使用的键类型。
type ctxKey string

// userCtxKey 指向当前请求的用户（*usermgr.User）。
const userCtxKey ctxKey = "trae2api_user"

// userFrom 取出当前请求的用户；未鉴权或匿名时返回 nil。
func userFrom(r *http.Request) *usermgr.User {
	if u, ok := r.Context().Value(userCtxKey).(*usermgr.User); ok {
		return u
	}
	return nil
}

// isAdmin 报告当前请求是否具备管理员权限。
//
// 语义：显式 admin 用户，或未启用用户体系时用全局 API Key 通过鉴权的匿名管理员。
func isAdmin(r *http.Request) bool {
	u := userFrom(r)
	return u != nil && u.Role == usermgr.RoleAdmin
}

// withAuth 鉴权中间件（支持多用户 RBAC）。
//
// 判定顺序：
//  1. 用户专属 API Key（usermgr）→ 注入用户身份，按角色授权；
//  2. 全局 API Key（config.api_key）→ 视为管理员（root）；
//  3. 两者都未命中 → 401。
//
// 安全要点：**一旦存在用户，就不允许匿名通过** —— 否则"创建了用户"反而
// 让 api_key 为空时的网关完全敞开。未创建用户时保持单机免密语义（零回归）。
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if key == "" {
			key = strings.TrimSpace(r.Header.Get("x-api-key"))
		}
		hasUsers := s.users != nil && s.users.Count() > 0

		// 1) 用户专属 Key
		if hasUsers && key != "" {
			if u, ok := s.users.FindByAPIKey(key); ok {
				next(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, u)))
				return
			}
		}

		// 1.5) 控制台用户会话 Cookie（支持通过 Web 登录后无缝调用受保护端点）
		if u := s.sessionUser(w, r); u != nil {
			next(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, u)))
			return
		}
		// 1.6) 控制台密码闸门禁 Cookie（已通过 web_password 鉴权的管理端会话）
		if s.webAuth != nil && s.webAuth.Enabled() && s.consoleAuthed(r) {
			root := &usermgr.User{ID: "root", Username: "root", Role: usermgr.RoleAdmin}
			next(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, root)))
			return
		}

		// 2) 全局 Key（管理员）
		if s.cfg.APIKey != "" {
			if key != "" && subtle.ConstantTimeCompare([]byte(key), []byte(s.cfg.APIKey)) == 1 {
				root := &usermgr.User{ID: "root", Username: "root", Role: usermgr.RoleAdmin}
				next(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, root)))
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="trae2api"`)
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "Invalid API key")
			return
		}

		// 3) 未配置全局 Key
		if hasUsers {
			// 有用户但没配全局 Key：只认用户 Key，禁止匿名（否则等于没鉴权）
			w.Header().Set("WWW-Authenticate", `Bearer realm="trae2api"`)
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "Invalid API key")
			return
		}
		// 单机免密模式（零回归）。
		//
		// 必须注入一个匿名管理员身份：否则 isAdmin() 恒为 false，
		// 控制台的所有管理端点会全部 403 —— 单机模式下反而不能管理自己。
		// （这个 bug 是单元测试抓出来的，不是读代码看出来的。）
		anon := &usermgr.User{ID: "anonymous", Username: "anonymous", Role: usermgr.RoleAdmin}
		next(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, anon)))
	}
}

// requireAdmin 校验管理员权限；不通过时已写出 403 并返回 false。
func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if isAdmin(r) {
		return true
	}
	writeOpenAIError(w, http.StatusForbidden, "forbidden",
		"该操作需要管理员权限（请使用管理员 API Key）")
	return false
}

// ownerOf 返回请求者的归属标识集合（多租户隔离用）。
//
// 管理员或单机免密模式返回 nil（= 不限归属）；普通用户返回自己的 ID 与用户名
// —— 账号的 owner 写哪一个都能匹配上（见 pool 的 containsOwner）。
func (s *Server) ownerOf(r *http.Request) []string {
	u := userFrom(r)
	if u == nil || u.Role == usermgr.RoleAdmin {
		return nil
	}
	out := make([]string, 0, 2)
	if u.ID != "" {
		out = append(out, u.ID)
	}
	if u.Username != "" {
		out = append(out, u.Username)
	}
	return out
}

// sessionKey 组合会话粘性键（带归属前缀，防不同用户相同会话 ID 互撞）。
func sessionKey(body []byte, owners []string) string {
	k := session.ExtractKey(body)
	if k == "" {
		k = session.FallbackKey(body)
	}
	if k != "" && len(owners) > 0 {
		k = owners[0] + ":" + k
	}
	return k
}

// ---- 只读端点 ----

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "service": ServiceName, "version": Version,
		"uptime_seconds": int(time.Since(s.startedAt).Seconds()),
		"realms":         s.realmNames(),
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, 0)
	for _, rk := range realm.All() {
		if ch, ok := s.channels[rk]; ok {
			out = append(out, ch.ModelEntries()...)
		}
	}
	// 虚拟智能体模型 dsh（见 dsh.go）。
	//
	// 它**永远**在清单里，与"dsh 进程当前有没有在跑"无关：模型清单描述的是
	// "这个网关提供哪些可调用的名字"，而不是"现在哪些进程活着"。
	// 让清单随进程启停抖动，客户端缓存会跟着乱跳。
	out = append(out, dshModelEntry())
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

// editionStatus 汇总已注册的版本域与产品（供 /status 诊断）。
//
// 为什么需要暴露它："国际版配了但没生效"是最难自查的问题之一 ——
// 配置写错时只是**不注册**，没有任何报错，表现只是"国际账号用不了"。
func (s *Server) editionStatus() map[string]any {
	// ⚠️ 必须按**实际使用的 spec** 统计，而不是全局 `traelctl.Products`。
	//
	// 为什么：显式写了 `trae.accounts` 时，`resolveSpecs` 会**直接返回那份清单**，
	// 不再自动探测任何产品（包括国际版）。此时若按全局 Products 报告，
	// `registered` 会宣称"intl 已注册"，而实际一个国际版账号都没去探测 ——
	// 这个端点的用途恰恰是诊断"国际版配了但没生效"，**在该故障下它却说反话**。
	//
	// 用 resolveSpecs 的返回值：它正是 BuildChannels 用的同一份输入，天然一致。
	byEdition := map[string][]string{}
	for _, sp := range resolveSpecs(s.cfg) {
		k := string(sp.product.Edition)
		byEdition[k] = append(byEdition[k], sp.product.Key)
	}
	active := map[string]int{}
	for rk, ch := range s.channels {
		active[rk] = len(ch.Accounts())
	}
	return map[string]any{
		"registered":      byEdition,
		"active_channels": active,
		"intl_enabled":    s.cfg.Intl.Enabled,
		// 显式配了 accounts 时自动探测被跳过 —— 显式说出来，否则这个差异只能靠
		// "registered 里少了 intl" 去猜，而少了也可能只是因为 intl.enabled=false。
		"explicit_accounts": len(s.cfg.Trae.Accounts) > 0,
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := s.pool.Counts()
	realmTotals := map[string]map[string]int{}
	for _, rk := range realm.All() {
		t, h, c, d, f := s.pool.CountsForRealm(rk)
		realmTotals[rk] = map[string]int{
			"total": t, "healthy": h, "cooling": c, "disabled": d, "in_flight_full": f,
		}
	}
	channels := make([]map[string]any, 0, len(s.channels))
	for _, rk := range realm.All() {
		ch, ok := s.channels[rk]
		if !ok {
			continue
		}
		channels = append(channels, map[string]any{
			"realm": rk, "label": ch.Product.Label, "product": ch.Product.Key,
			"function": ch.Product.Function, "models": len(ch.Catalog.Models),
			"accounts": len(ch.Accounts()),
		})
	}
	sticky := 0
	if s.sessions != nil {
		sticky = s.sessions.Count()
	}
	userCount := 0
	if s.users != nil {
		userCount = s.users.Count()
	}
	resp := map[string]any{
		"service":         ServiceName,
		"version":         Version,
		"uptime_seconds":  int(time.Since(s.startedAt).Seconds()),
		"accounts":        s.pool.List(),
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"realm_totals":    realmTotals,
		"channels":        channels,
		"sticky_sessions": sticky,
		"sticky_enabled":  s.sessions != nil,
		"users":           userCount,
		"rbac_enabled":    userCount > 0,
		"checkin_enabled": s.cfg.Schedule.CheckinEnabled,
		"checkin_hours":   s.cfg.Schedule.CheckinHours,
	}
	// WAF IP 级闸状态（未激活时也给出统计，便于判断"离触发有多近"）
	if s.wafIP != nil {
		resp["waf_ip_gate"] = s.wafIP.Status()
	}
	if s.degrade != nil {
		resp["prompt_degrade"] = s.degrade.Status()
	}
	// 已注册的版本域与产品 —— 国际版"配了但没生效"是最难自查的问题之一
	//（配置写错时只是不注册，没有任何报错），所以显式暴露出来。
	resp["editions"] = s.editionStatus()
	if s.loginLimit != nil {
		resp["login_limit"] = s.loginLimit.Status()
	}
	// 图片可用性：让用户一眼看到"为什么发图不行"，而不是去猜上游的 4001
	resp["vision"] = s.visionStatus()
	writeJSON(w, http.StatusOK, resp)
}

// handleConsole 简易只读控制台（纯 HTML，无外部资源）。
//
// 为什么先做只读：控制台的价值在于"一眼看清账号池健康度与冷却原因"，
// 写操作（解除冷却/启停账号）风险更高，留到有鉴权与审计之后再补。
func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	// 管理中枢的路径是 /console（公开门户占了 /）。
	// 同时容忍 /dashboard 这个别名 —— 参考实现用两个路径，用户习惯不一。
	if p := r.URL.Path; p != "/console" && p != "/dashboard" {
		http.NotFound(w, r)
		return
	}
	// 控制台密码闸：只挡**页面本身**，不挡 API（后者由 withAuth 的 API Key 负责）。
	// 两者正交：密码闸挡"浏览器随手打开 UI"，API Key 挡"程序调接口"。
	if !s.consoleAuthed(r) {
		writeLoginHTML(w, http.StatusUnauthorized, "")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 控制台要 iframe 内嵌本机的 dsh 网页 UI，而它是**另一个端口 = 另一个 origin**。
	// 全局安全头里的 CSP 是 `default-src 'self'`，frame-src 回落到它 —— 不放开的话
	// iframe 会被浏览器直接拦掉，而且失败只体现在 DevTools 控制台里，页面上看不出原因。
	// 这里只把 frame-src 放宽到本机回环（其余指令与全局策略一致），
	// 不放宽 X-Frame-Options：控制台自身仍然不允许被别的站点嵌套。
	w.Header().Set("Content-Security-Policy", consoleCSP)
	// 有密码闸时禁止缓存，避免共享机器上退出后仍能从缓存看到页面
	if s.webAuth != nil && s.webAuth.Enabled() {
		w.Header().Set("Cache-Control", "no-store")
	}
	_, _ = io.WriteString(w, consoleHTML)
}

// consoleCSP 是控制台专用的 CSP：在全局策略基础上放开 frame-src。
//
// 为什么必须是"本机回环 + 任意端口"而不是某个固定端口：dsh 的端口是**动态分配**的
// （`--port 0`，见 dsh.go），没法在编译期写死。
const consoleCSP = "default-src 'self'; img-src 'self' data: https:; " +
	"style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; frame-src http://127.0.0.1:* http://localhost:*; " +
	"frame-ancestors 'self'"

// ---- 小工具 ----

func (s *Server) realmNames() []string {
	out := make([]string, 0, len(s.channels))
	for rk := range s.channels {
		out = append(out, rk)
	}
	sort.Strings(out)
	return out
}

func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// randHex 生成 n 字节随机数的 hex（不引入第三方依赖）。
func randHex(n int) string {
	const hexdigits = "0123456789abcdef"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	out := make([]byte, n*2)
	for i, v := range b {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0x0f]
	}
	return string(out)
}

// ---- 请求级统计 ----

// chatStat 记录单次请求的可观测数据，请求出口落一行日志并记入指标聚合。
//
// 「缺观测」与「显式 0」在这里就必须分开：hasUsage=false 表示上游没给 usage
// （错误响应、流中断、上游省略 token_usage 事件），此时**不能**把 token 当 0 记入，
// 否则均值会被系统性拉低、看板出现"tokens 骤降"的假警报。
// 同理 ttfbSet 区分"非流式没有首字节概念"与"首字节耗时真的是 0"。
type chatStat struct {
	start      time.Time
	model      string
	mode       string
	realm      string
	accountUID string
	accountLbl string

	ttfb    time.Duration
	ttfbSet bool

	success   bool
	promptTok int64
	compTok   int64
	cacheHit  int64
	cacheMiss int64
	cacheWr   int64
	hasUsage  bool

	credits    float64
	hasCredits bool

	finish string
	logged bool

	// collector 指标聚合器（可为 nil = 不采集）。
	collector *metrics.Collector
}

func newChatStat(now time.Time, model string, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: model, mode: mode, finish: "stop", success: true}
}

func (s *chatStat) setAccount(uid, label string) { s.accountUID, s.accountLbl = uid, label }

func (s *chatStat) setRealm(r string) { s.realm = r }

// markFailed 标记本次请求失败（供指标统计成功率）。
func (s *chatStat) markFailed() { s.success = false }

func (s *chatStat) setFinish(f string) {
	if f != "" {
		s.finish = f
	}
}

func (s *chatStat) finishReason() string {
	if s.finish == "" {
		return "stop"
	}
	return s.finish
}

func (s *chatStat) recordUsage(u *traectl.Usage) {
	if u == nil {
		return
	}
	s.hasUsage = true
	s.promptTok = int64(u.PromptTokens)
	s.compTok = int64(u.CompletionTokens)
	s.cacheHit = int64(u.CacheReadInputTokens)
	s.cacheWr = int64(u.CacheCreationInputTokens)
}

func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	total := time.Since(s.start)
	tok := "-"
	if s.hasUsage {
		tok = fmt.Sprintf("%d+%d", s.promptTok, s.compTok)
	}
	acct := s.accountUID
	if acct == "" {
		acct = "-"
	}
	log.Printf("[req] %-28s %-6s acct=%-28s finish=%-11s tok=%-10s total=%s",
		s.model, s.mode, acct, s.finishReason(), tok, total.Round(time.Millisecond))

	if s.collector == nil {
		return
	}
	ttfb := metrics.NoObservation
	if s.ttfbSet {
		ttfb = s.ttfb.Milliseconds()
	}
	s.collector.Record(metrics.Sample{
		Model: s.model, Account: s.accountUID, Realm: s.realm,
		Streaming: s.mode == "stream", Success: s.success,
		TTFBMS:       ttfb,
		LatencyMS:    total.Milliseconds(),
		PromptTokens: s.promptTok, CompletionTokens: s.compTok,
		CacheHit: s.cacheHit, CacheMiss: s.cacheMiss, CacheWrite: s.cacheWr,
		HasUsage:   s.hasUsage,
		Credits:    s.credits,
		HasCredits: s.hasCredits,
		At:         s.start,
	})
}
