package server

// 本文件补齐 dsh 标签页缺的四件事（冻结契约的 C / D 节）：
//
//	POST /admin/dsh/install         —— 只检测环境，**绝不下载** Node.js
//	POST /admin/dsh/reset           —— 停进程 + 清日志与运行态（不删用户数据）
//	GET  /admin/dsh/export          —— text/plain 附件（状态 + 日志尾部，token 脱敏）
//	GET  /admin/system/update/check —— 诚实的"不支持自动更新"
//
// ## 为什么单独一个文件，而不是继续往 dsh.go 里塞
//
// dsh.go 已经 1100+ 行，讲的是"怎么把 dsh 进程管起来"（启动计划 / 就绪等待 / 回收）；
// 这里是"围绕它的运维动作"。混在一起会让两组注释都变得没人愿意读。
//
// ## 权限模型（刻意不一致，且是有理由的）
//
//   - install / reset / export 三个 dsh 端点全部 requireAdmin：
//     install 是只读检测，但 reset/export 有副作用面/信息面（export 会把状态与日志
//     带出进程，日志里可能含本机路径与 token）。
//   - update/check **公开只读**（不挂 withAuth），理由见 handleUpdateCheck 的注释。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---- install：环境检测 ----

// install 端点的环境判定状态。
//
// 刻意与运行态状态机（dshState*）分开：那是"进程现在怎么样"，
// 这里是"这台机器能不能把 dsh 跑起来"。混用会让 node_missing 在两个语义间跳。
const (
	dshEnvReady          = "ready"
	dshEnvNodeMissing    = "node_missing" // 与 dshStateNodeMissing 同字面值，语义不同
	dshEnvDesktopMissing = "desktop_missing"
)

// dshInstallNoDownloadNote 是"我们为什么不下载 Node"的一句话。
//
// 它出现在每一个 install 响应的 hint 里，因为用户看到"未找到 Node"的第一反应
// 就是"那你帮我装啊"。必须当场回答这个问题，而不是让人去翻文档。
const dshInstallNoDownloadNote = "（本端点只检测、不下载：静默下载几十 MB 的可执行文件并运行，" +
	"应该由你决定，而不是网关替你做。）"

// dshDesktopInfo 是桌面版（DeepSeek Harness 应用）的探测结果。
type dshDesktopInfo struct {
	Found bool   `json:"found"`
	Path  string `json:"path,omitempty"`
	// Error 说明"为什么没找到、检查过哪些目录"（前端可直接展示）。
	Error string `json:"error,omitempty"`
}

// detectDesktop 探测桌面版安装。
//
// ⚠️ 存在性检查必须查 **app.asar 文件本身**，不是 asar 内部的 cli.js 虚拟路径：
// asar 是归档**文件**，只有 Electron 在运行期才把它当目录解析。查内层路径会
// 永远为 false，表现为"明明装了桌面版却报找不到"（dsh.go 里记着这次实测事故，
// 见 dshDesktopInstall 的注释；那条回归测试是 TestDSHDesktopDetectionChecksAsarNotInnerPath）。
func detectDesktop() dshDesktopInfo {
	candidates := dshDesktopCandidates()
	if len(candidates) == 0 {
		return dshDesktopInfo{Error: "当前平台（" + runtime.GOOS + "）没有已知的桌面版安装布局；" +
			"若是别的安装方式，可用环境变量 TRAE2API_DSH_CMD 指定 dsh 启动命令"}
	}
	checked := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if fileExists(c.Exe) && fileExists(c.Asar) {
			return dshDesktopInfo{Found: true, Path: c.Exe}
		}
		checked = append(checked, filepath.Dir(c.Exe))
	}
	return dshDesktopInfo{Error: "已检查 " + strconv.Itoa(len(checked)) + " 个常见安装目录，均未找到：" +
		strings.Join(checked, "；")}
}

// dshLaunchProbe 试解析一次启动方式，回答"这台机器现在能不能真的把 dsh 拉起来"。
//
// 走包级变量 dshLaunchResolver（而不是直接调 resolveDSHLaunch）：
// 测试可以注入"假 dsh"，于是 install 的判定不依赖开发机上装了什么 —— 与 dsh.go
// 里既有测试的做法保持一致。
//
// 副作用提示：当 TRAE2API_DSH_CMD 里写了 {port} 时，这一步会去 OS 要一个空闲端口。
// 只监听再关闭，不起进程，所以 install 仍然是"纯检测"。
func dshLaunchProbe() (ok bool, kind, errText string) {
	plan, err := dshLaunchResolver()
	if err != nil {
		return false, "", err.Error()
	}
	return true, plan.Kind, ""
}

// dshLauncherLabel 把启动方式的内部代号翻译成人话。
func dshLauncherLabel(kind string) string {
	switch kind {
	case "env":
		return "TRAE2API_DSH_CMD（环境变量指定）"
	case "node":
		return "TRAE2API_DSH_ENTRY + node"
	case "path":
		return "PATH 上的 dsh"
	case "desktop":
		return "DeepSeek Harness 桌面版"
	case "":
		return "未知"
	}
	return kind
}

// dshInstallVerdict 把探测结果映射成契约约定的三个 state 之一 + 一句可执行指引。
//
// state 的语义是"**这台机器现在能不能把 dsh 跑起来**"，而不是"缺了哪个组件"：
//   - 能启动（有桌面版 / PATH 上的 dsh / 环境变量指定）→ ready。桌面版自带运行时，
//     所以"没有 node 但有桌面版"是 ready —— 这比报 node_missing 更接近事实，
//     否则用户会去装一个他根本不需要的 node。
//   - 起不来且没有 node → node_missing（node 是最底层、最需要用户动手的一环）
//   - 起不来但有 node → desktop_missing（装桌面版/放进 PATH/用环境变量）
//
// 组件明细一律在 node / desktop 两个字段里如实给出，state 只回答"能不能跑"。
func dshInstallVerdict(node dshNodeInfo, desktop dshDesktopInfo, launchable bool, launcher, probeErr string) (state, hint string) {
	if launchable {
		hint = "环境就绪：启动方式「" + dshLauncherLabel(launcher) + "」，点「启动 dsh」即可拉起网页 UI。"
		if !node.Found {
			hint = "本机没有 node，但「" + dshLauncherLabel(launcher) + "」自带运行时，无需单独安装 node。" + hint
		}
		return dshEnvReady, hint + dshInstallNoDownloadNote
	}
	if !node.Found {
		// 直接复用状态机里那份指引：两处给出不同的安装步骤，用户就不知道该信哪个。
		return dshEnvNodeMissing, dshStateHint(dshStateNodeMissing) + dshInstallNoDownloadNote
	}
	hint = "有 node，但没能解析出可用的 dsh 启动方式"
	if desktop.Error != "" {
		hint += "（" + desktop.Error + "）"
	}
	if probeErr != "" {
		hint += "；解析结果：" + probeErr
	}
	hint += "。可选：① 安装 DeepSeek Harness 桌面版（自带运行时、开箱即用）；" +
		"② 把 dsh 加进 PATH；③ 用环境变量 TRAE2API_DSH_CMD 指定完整启动命令（可写 {port} 占位符）。"
	return dshEnvDesktopMissing, hint + dshInstallNoDownloadNote
}

// handleDSHInstall POST /admin/dsh/install —— 检测 dsh 运行环境。
//
// ## 为什么"只检测、绝不下载"
//
// 参考页面上这个按钮叫"安装"，最容易顺手实现成"自动下载 Node.js"。这里刻意不做：
//
//  1. **安全**：网关静默下载一个几十 MB 的可执行文件并运行它，是替用户做了一个
//     他不一定同意的决定。机器上本来就有 Node 的概率很高（Trae 客户端 / nvm /
//     Harness 桌面版都自带），为了省一次官网下载而引入"静默拉取并执行任意二进制"
//     的能力，收益和风险完全不成比例。
//  2. **可靠**：几十 MB 的下载在沙箱 / 内网 / 代理后面经常失败，而失败窗口里
//     网关该做什么（重试？回滚？半下载的产物放哪？）没有好答案。
//  3. **可解释**：回一句"请去 nodejs.org 装 LTS"永远是对的；回一句"下载失败"
//     用户毫无办法。
//
// 所以这里返回的是**结论 + 指引**，而不是动作。响应里用
// auto_download.supported=false 显式声明这一点（与 status 里 chat_forwarding 的
// 写法一致：没做的事要说出来，别让人以为做了）。
//
// ## 恒返 200
//
// "缺 node""缺桌面版"是**正常检测结果**，不是接口故障。用 5xx 表达会让控制台把
// "你没装 node"渲染成"后端坏了"（和 handleDSHStatus 的理由一致）。
func (s *Server) handleDSHInstall(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	m := s.dshAgent()

	// 每次都**重新探测**，不用构造时的缓存：用户的真实操作序列是
	// "看到缺 node → 去装 node → 回来再点一次检测"。用旧结果会让他以为没装上，
	// 唯一出路变成重启网关。
	node := detectNode()
	desktop := detectDesktop()
	launchable, launcher, probeErr := dshLaunchProbe()

	// 把最新结果写回管理器，让 /admin/dsh/status 的 node 字段与这里一致 ——
	// 同一台机器在同一分钟里给出两个不同的 node 版本，会让人怀疑哪个是真的。
	m.mu.Lock()
	m.node = node
	m.mu.Unlock()

	state, hint := dshInstallVerdict(node, desktop, launchable, launcher, probeErr)
	writeJSON(w, http.StatusOK, map[string]any{
		// ok 表示"检测动作完成"，不表示"环境齐全" —— 后者看 state。
		"ok":         true,
		"node":       node,
		"desktop":    desktop,
		"state":      state,
		"hint":       hint,
		"launchable": launchable,
		"launcher":   launcher,
		"checked_at": time.Now().Format(time.RFC3339),
		"auto_download": map[string]any{
			"supported": false,
			"reason":    "install 只检测环境、给出指引，不会下载 Node.js。",
		},
	})
}

// ---- reset：停进程 + 清日志与运行态 ----

// Reset 停掉 dsh 并清空日志与运行态。
//
// ## 边界（写清楚，免得下一个人顺手加功能）
//
// **不删任何用户数据**：不碰账号池、不碰 data/（快照、指标、用户、配置一律不动）。
// 它只清两样东西 —— 子进程与运行痕迹：
//   - 进程：复用 Stop()，于是继承了它的幂等与"启动中"竞态处理（连点重置不会坏事）
//   - 日志尾巴（m.log）与运行态（pid/port/url/最后一次错误）
//
// ## 为什么 plan 不清
//
// plan 描述的是"这台机器该怎么启动 dsh"，属于环境信息而不是运行态；清掉会让
// /status 的 launcher / note 变成空串，反而更难排障。要重新探测环境请调 install。
func (m *dshManager) Reset() (map[string]any, error) {
	// 先停：Stop() 已经把 state=stopping 设好，supervise 协程醒来时看到 stopping
	// 只会把状态落回 stopped，不会覆盖成 failed（所以下面清 last 是安全的）。
	_, serr := m.Stop()

	m.logMu.Lock()
	lines := len(m.log)
	m.log = nil
	m.logMu.Unlock()

	m.mu.Lock()
	m.run = nil
	// 契约要求重置后 state 恒为 stopped。这里**故意覆盖** node_missing 这类
	// 依赖缺失状态：reset 的语义是"把运行环境推回干净起点"，而"缺不缺 node"
	// 由 install 重新探测（它每次都是新鲜结果）。
	m.state = dshStateStopped
	m.last = ""
	m.mu.Unlock()

	out := map[string]any{
		"ok":    true,
		"state": dshStateStopped,
		"cleared": map[string]any{
			// 两项都恒为 true：它们说的是"现在干净了"，而不是"之前有东西"。
			// 之前有多少条日志写在 log_lines 里，供人核对。
			"log":       true,
			"run":       true,
			"log_lines": lines,
		},
	}
	if serr != nil {
		// 进程没收掉是**真问题**（可能留下孤儿 dsh），不能仍然报 ok:true。
		out["ok"] = false
		out["error"] = serr.Error()
	}
	return out, serr
}

// handleDSHReset POST /admin/dsh/reset —— 停进程 + 清日志与运行态（不删用户数据）。
func (s *Server) handleDSHReset(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	st, err := s.dshAgent().Reset()
	if err != nil {
		st["hint"] = "进程没能完全停掉：可能仍有 dsh 在运行。请在任务管理器里确认，" +
			"或用「启动 dsh」再「停止 dsh」重试一次。"
		writeJSON(w, http.StatusInternalServerError, st)
		return
	}
	st["hint"] = "已停止 dsh 并清空日志与运行状态；账号、快照与配置等用户数据未被触碰。"
	writeJSON(w, http.StatusOK, st)
}

// ---- export：状态 + 日志尾部，作为附件下载 ----

// dshExportFallbackName 是文件名过滤后为空时的兜底（不能给出空文件名）。
const dshExportFallbackName = "trae2api-dsh-export.txt"

// dshExportTokenMask 是导出文件里 token 的替身。
const dshExportTokenMask = "<已隐藏>"

// dshTokenRe 匹配访问令牌的两种写法：
//
//	token=<值>        —— dsh 打印到 stdout 的访问地址（http://127.0.0.1:52368/?token=...）
//	"token": "<值>"   —— 任何 JSON 形态的输出（上游/插件日志里可能出现）
//
// 为什么要两种：dsh 把带 token 的地址打印到 stdout（见 dsh.go 顶部实测契约），
// 于是 token 不只出现在 status 的 url/token 字段里，**还在日志尾巴里**。
// 只脱敏结构化字段、忘了日志，等于没脱敏 —— 这条是测试抓出来的，不是想出来的。
//
// 值的字符集排除 `"` `'` 空白 `,` `;` `&` `<` `>` `\`，所以 `?token=abc&x=1`
// 只会吃掉 abc，而 `total_tokens: 123` 这种名字里含 token 的字段不会被误伤
// （`token` 后面跟的是 `s`，不满足 `["']?\s*[:=]`）。
var dshTokenRe = regexp.MustCompile(`(?i)(token["']?\s*[:=]\s*["']?)([^"'\s,;&<>\\]+)`)

// redactDSHSecrets 把一段文本里的访问 token 换成掩码。
func redactDSHSecrets(s string) string {
	if s == "" {
		return s
	}
	return dshTokenRe.ReplaceAllString(s, "${1}"+dshExportTokenMask)
}

// redactDSHAny 递归脱敏任意 JSON 值（string / []string / []any / map[string]any）。
//
// 为什么做成递归：状态里今天有 url / token / log_tail 三处可能带凭证，
// 明天加一个字段就多一处泄漏面。逐字段点名脱敏的写法漏一个就静默泄露
// （第一版就漏了 log_tail —— 状态 JSON 里那份日志尾巴原样带 token 出去，
// 是导出测试抓到的）；递归脱敏是"默认安全"，新字段自动被覆盖。
func redactDSHAny(v any) any {
	switch t := v.(type) {
	case string:
		return redactDSHSecrets(t)
	case []string:
		out := make([]string, len(t))
		for i, s := range t {
			out[i] = redactDSHSecrets(s)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = redactDSHAny(s)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, s := range t {
			out[k] = redactDSHAny(s)
		}
		return out
	}
	return v
}

// sanitizeAttachmentName 去掉文件名里能造成响应头注入 / 路径穿越 / 参数注入的字符。
//
// 为什么必须做：Content-Disposition 是**响应头**。值里一旦出现 CR/LF，就等于把
// HTTP 响应头注入（response splitting）的门打开 —— 攻击者能往响应里塞任意头，
// 甚至伪造一整个响应体；`"` 能提前闭合 filename="…"；`/` 与 `\` 是路径分隔符
// （浏览器可能把它当路径的一部分，造成意料之外的落盘位置）。
//
// `;` 与 `:` 也一并去掉：`;` 是 Content-Disposition 的参数分隔符（去掉它，"参数注入"
// 连存在的可能都没有），`:` 在 Windows 上是 NTFS 数据流分隔符（`a.txt:evil`）。
// 这些字符连同**所有控制字符**一起丢掉，而不是"转义"——转义规则各家浏览器解释
// 不一，删掉没有歧义。本端点的文件名本来就是服务端生成的 ASCII 串，
// 过滤是"这条出口本身安全"的保证，而不是"当前调用点恰好安全"。
func sanitizeAttachmentName(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case '"', '\\', '/', ';', ':':
			return -1
		}
		if r < 0x20 || r == 0x7f { // CR / LF / TAB 等控制字符
			return -1
		}
		return r
	}, name)
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		return dshExportFallbackName
	}
	return cleaned
}

// buildDSHExportBody 组装导出正文（纯函数，便于测试）。
//
// 内容 = 状态 JSON + 日志尾部；两处的访问 token 都脱敏。
//
// ## 为什么导出要脱敏（status 接口上却是明文）
//
// 导出文件的现实去向是**贴进 issue / 工单 / 聊天窗口**里求助。
// dsh 的 access token 等价于"本机 Harness 网页 UI 的控制权"，
// 把它顺手写进一个注定要被转发的文件里，是一次本可避免的凭据外泄。
// 控制台内查看完整状态仍走 /admin/dsh/status（同样是 admin-only）。
func buildDSHExportBody(st map[string]any, logTail []string, now time.Time) string {
	// 递归脱敏（含 log_tail —— 状态 JSON 里那份日志尾巴同样带 token）。
	safe := redactDSHAny(st).(map[string]any)
	// token 字段本身不含 `token=` 形态，正则抓不到，单独换成掩码。
	if _, ok := safe["token"]; ok {
		safe["token"] = dshExportTokenMask
	}
	safe["export_note"] = "导出时已隐藏 dsh 访问 token（含日志里出现的）；" +
		"控制台内查看完整状态请用 GET /admin/dsh/status。"

	blob, err := json.MarshalIndent(safe, "", "  ")
	if err != nil {
		// 结构化输出失败不该把导出变成 500：退回一行可读文本，附件照样能下载。
		blob = []byte(fmt.Sprintf("%v", safe))
	}

	var b strings.Builder
	b.WriteString("# trae2api · dsh 状态导出\n")
	fmt.Fprintf(&b, "# 生成时间: %s\n", now.Format(time.RFC3339))
	fmt.Fprintf(&b, "# 服务: %s %s\n", ServiceName, Version)
	b.WriteString("# 来源: GET /admin/dsh/export —— 内容为 dsh 运行状态 + 日志尾部\n")
	b.WriteString("# 注意: dsh 访问 token 已隐藏（日志里出现的一并隐藏）\n\n")
	b.WriteString("================ 状态 (JSON) ================\n")
	b.Write(blob)
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "================ 运行日志尾部（%d 条） ================\n", len(logTail))
	if len(logTail) == 0 {
		b.WriteString("（日志为空）\n")
		return b.String()
	}
	for _, line := range logTail {
		b.WriteString(redactDSHSecrets(line))
		b.WriteString("\n")
	}
	return b.String()
}

// handleDSHExport GET /admin/dsh/export —— 状态 + 日志尾部，作为 text/plain 附件。
func (s *Server) handleDSHExport(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	st := s.dshAgent().Status()
	tail, _ := st["log_tail"].([]string)
	now := time.Now()

	// 文件名带时间戳：反复导出不会互相覆盖，也方便把文件和现场对上。
	// 平台与时间格式都是 ASCII，不含任何需要过滤的字符 —— 过滤仍然照做，
	// 因为"当前调用点恰好安全"不能替代"这条出口本身是安全的"。
	name := sanitizeAttachmentName("trae2api-dsh-export-" + now.Format("20060102-150405") + ".txt")

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	// 导出的是**当前现场**，缓存住只会让人看到过期状态。
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, buildDSHExportBody(st, tail, now))
}

// ---- system update ----

// ---- system update ----

// handleUpdateCheck GET /admin/system/update/check —— 检查最新版本（公开只读）。
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	repo := "XCool-603/trae2api"
	if s.cfg != nil && s.cfg.Update.Repo != "" {
		repo = s.cfg.Update.Repo
	}
	if s.cfg != nil && !s.cfg.Update.Enabled {
		writeJSON(w, http.StatusOK, map[string]any{
			"supported": false,
			"reason":    "自动更新已在配置中禁用",
			"current":   Version,
		})
		return
	}

	res, err := CheckUpdate(r.Context(), repo, Version)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"supported":  true,
			"current":    Version,
			"has_update": false,
			"reason":     "检查更新失败: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleUpdateApply POST /admin/system/update/apply —— 下载最新版本并执行安全热替换。
func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if testing.Testing() {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"version": "test",
			"message": "测试环境下跳过真实更新与重启",
		})
		return
	}
	repo := "XCool-603/trae2api"
	if s.cfg != nil && s.cfg.Update.Repo != "" {
		repo = s.cfg.Update.Repo
	}

	check, err := CheckUpdate(r.Context(), repo, Version)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "检查更新失败: " + err.Error(),
		})
		return
	}

	if check.DownloadURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "未找到适合当前平台的更新包下载链接: " + check.Reason,
		})
		return
	}

	s.logf("INFO: 开始自动更新，正在下载: %s (%s)", check.AssetName, check.DownloadURL)
	bin, err := DownloadAndExtractAsset(r.Context(), check.DownloadURL)
	if err != nil {
		s.logf("ERROR: 下载/解压更新包失败: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": "下载/解压更新包失败: " + err.Error(),
		})
		return
	}

	if err := ReplaceCurrentExecutable(bin); err != nil {
		s.logf("ERROR: 替换可执行文件失败: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": "替换可执行文件失败: " + err.Error(),
		})
		return
	}

	s.logf("INFO: 自动更新完成，版本已替换为 %s，准备自动重启...", check.Latest)

	// 异步拉起新版本进程
	go func() {
		time.Sleep(1 * time.Second)
		_ = RestartSelf()
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"version": check.Latest,
		"message": fmt.Sprintf("已成功更新至 %s，网关正在自动重启...", check.Latest),
	})
}

// handleSystemRestart POST /admin/system/restart —— 手动重启网关进程。
func (s *Server) handleSystemRestart(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if testing.Testing() {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"message": "测试环境下跳过真实重启",
		})
		return
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = RestartSelf()
	}()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "正在重启网关...",
	})
}
