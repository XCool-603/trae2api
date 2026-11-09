// Command trae2api 是**单一可执行文件**入口。
//
// 用法：
//
//	trae2api                      # 启动网关（等价于 serve）
//	trae2api serve [-config …]    # 启动网关
//	trae2api acct  <子命令>        # 账号管理
//	trae2api credit [-refresh]    # 额度查询
//	trae2api models [-multimodal] # 模型清单
//	trae2api signin               # 每日签到
//	trae2api login [-product …]   # OAuth 一键授权
//	trae2api stats                # 实时统计 TUI
//	trae2api version              # 版本
//	trae2api help                 # 本帮助
//
// ## 为什么是单一可执行文件
//
// 参考实现把"摒弃散落的辅助可执行文件（login.exe / signin.exe / trial.exe）"
// 列为第一条特色，理由很实在：
//
//   - **分发**：一个文件随下随用，不用管哪个 exe 该放哪、版本是否配套
//   - **启动脚本消失**：不再需要 .cmd 去串起多个 exe（那些脚本在非 Windows 上没用）
//   - **体积**：Go 的多个小 exe 各自带一份运行时；合并后共享
//   - **一致性**：所有命令必然同一版本（散落时很容易只更新了其中一个）
//
// 我最初做成了 7 个 exe —— 方向正好相反。这个入口把它们合成一个。
package main

import (
	"fmt"
	"os"
	"strings"

	"trae2api/internal/cmd/acct"
	"trae2api/internal/cmd/credit"
	"trae2api/internal/cmd/invite"
	"trae2api/internal/cmd/login"
	"trae2api/internal/cmd/models"
	cmdsrv "trae2api/internal/cmd/server"
	"trae2api/internal/cmd/signin"
	"trae2api/internal/cmd/stats"
	"trae2api/internal/server"
)

func main() {
	os.Exit(dispatch(os.Args[1:]))
}

// dispatch 按第一个参数选择子命令，返回退出码。
func dispatch(args []string) int {
	// 元命令（version / help 及其 flag 写法）必须**排在最前面**。
	//
	// ⚠️ 这里的顺序是有讲究的：下面有一条兜底规则"第一个参数以 - 开头就当网关
	// 参数"。如果元命令放在它后面，`trae2api --help` / `--version` 会被送进网关的
	// FlagSet，用户得到的是一句 "flag provided but not defined: -help" 加一串
	// 他并不关心的 serve 参数 —— 而他只是想问"这玩意儿怎么用"。
	if len(args) > 0 {
		switch args[0] {
		case "version", "-v", "--version", "-version":
			fmt.Printf("trae2api %s\n", server.Version)
			return 0
		case "help", "-h", "--help", "-help":
			usage(os.Stdout)
			return 0
		}
	}

	// 无参数、或第一个参数就是 flag（如 `trae2api -config x.json`）
	// → 默认启动网关。这让"双击 exe"与"老命令行习惯"都能直接工作。
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return cmdsrv.Run(args)
	}

	name, rest := args[0], args[1:]
	switch name {
	case "serve", "server", "run", "start":
		return cmdsrv.Run(rest)
	case "acct", "account", "accounts":
		return acct.Run(rest)
	case "credit", "credits", "quota":
		return credit.Run(rest)
	case "models", "model":
		return models.Run(rest)
	case "signin", "checkin":
		return signin.Run(rest)
	case "invite":
		return invite.Run(rest)
	case "login", "signin-oauth", "auth":
		return login.Run(rest)
	case "stats", "stat", "monitor":
		return stats.Run(rest)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n", name)
		usage(os.Stderr)
		return 2
	}
}

// usage 打印帮助。
//
// 写成表格而不是一长串：用户是来找"我要做 X 该敲什么"的，
// 对齐的命令列比散文好扫。
func usage(w *os.File) {
	fmt.Fprint(w, `trae2api `+server.Version+` — 把 Trae 订阅额度变成 OpenAI 兼容接口

用法: trae2api [子命令] [参数...]

子命令:
  serve      启动网关（默认；不带子命令时即此）
  acct       账号管理（list / add / del / enable / disable / cooldown）
  credit     额度查询（-refresh 强制实时刷新）
  models     模型清单（-multimodal 只看多模态）
  signin     每日签到（领取积分）
  invite     邀请与福利（查邀请码 / -redeem 兑换 / -newbie 领新手礼）
  login      OAuth 一键授权（-product trae-cn / solo-cn / trae-intl / solo-intl）
  stats      实时统计 TUI（-once 打印一次后退出）
  version    显示版本
  help       显示本帮助

常用示例:
  trae2api                                   # 启动网关（读 ./config.json）
  trae2api serve -config D:/trae/cfg.json    # 指定配置
  trae2api serve -gui                        # 额外开桌面窗口（Windows 优先 WebView2）
  trae2api acct list                         # 看账号池
  trae2api credit -refresh                   # 强制刷新额度
  trae2api stats                             # 实时看流量

环境变量:
  TRAE2API_URL   网关地址（默认 http://127.0.0.1:7870）
  TRAE2API_KEY   API Key（单机免密时可不设）

不带子命令直接跟参数（如 trae2api -config x.json）也会启动网关。
`)
}
