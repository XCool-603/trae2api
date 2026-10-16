package server

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"trae2api/internal/accountstore"
	"trae2api/internal/config"
	"trae2api/internal/pool"
	"trae2api/internal/realm"
	"trae2api/internal/traectl"
)

// Channel 是一个模型域（realm）的运行时载体：一个产品 + 一批账号 + 一份模型目录。
//
// 为什么按 realm 聚合而不是按账号：模型目录、function 字段、ClientID 都是
// **产品级**的，同域内所有账号共享；账号级差异只在凭据与运行时状态（后者在 pool 里）。
type Channel struct {
	// Realm 域标识（trae / solo）。
	Realm string
	// Product 产品常量。
	Product traectl.Product
	// Catalog 服务端下发的模型目录（可能为空 = 目录加载失败但渠道仍可用）。
	Catalog *traectl.Catalog

	mu       sync.RWMutex
	accounts map[string]*pool.Account // uid → 池内账号
	client   *traectl.Client          // 模板客户端（base/UA/版本头）；调用时按账号绑定 Store
}

// RealmOfProduct 把产品 key 映射到 realm。
func RealmOfProduct(key string) string {
	switch key {
	case "trae-cn":
		return realm.Trae
	case "solo-cn":
		return realm.Solo
	case "trae-intl":
		return realm.TraeIntl
	case "solo-intl":
		return realm.SoloIntl
	}
	return ""
}

// AccountUID 生成池内唯一 ID。
//
// 形如 "trae-cn-916348159159136"。**不能只用 userId**：Trae CN 与 TRAE SOLO CN
// 在本机通常是同一个账号（实测同一 userId），只用 userId 会撞号。
func AccountUID(p traectl.Product, userID string) string {
	return p.Key + "-" + userID
}

// accountSpec 是解析后的账号配置（含默认路径展开）。
type accountSpec struct {
	product     traectl.Product
	storagePath string
	label       string
	owner       string
	maxInFlight int
	disabled    bool
	proxy       string
}

// newTraeClient 是 traectl.Client 的**唯一构造点**。
//
// ⚠️ 为什么必须集中：这个客户端有若干"配错就静默失效"的字段 ——
// 例如 IdleTimeout，设成 0 时流中空闲监控会**直接不启用**，而且没有任何报错，
// 表现只是"偶尔有请求永远挂住"。
//
// 曾经就是因为有两个构造点（BuildChannels 与 registerAccount），
// 我给字段加进结构体后只改了其中一个的意图、另一个仍是默认值 ——
// 于是运行时热导入的账号完全没有空闲监控。抽成一个函数后这类分叉不可能再发生。
func newTraeClient(cfg *config.Config, prod traectl.Product) *traectl.Client {
	idle := time.Duration(cfg.Trae.IdleTimeoutSeconds) * time.Second
	if cfg.Trae.IdleTimeoutSeconds < 0 {
		idle = 0 // 显式负值 = 禁用
	}
	// chat base 按**版本域**选：国内版与国际版是不同的上游主机。
	// 用同一个 base 会让国际账号的请求打到国内上游（或反之），
	// 表现为"账号健康却总是失败"。
	base := cfg.Trae.ChatBase
	if prod.Edition == traectl.EditionIntl {
		base = cfg.Intl.ChatHost
	}
	return &traectl.Client{
		Base:           base,
		UserAgent:      cfg.Trae.UserAgent,
		IDEVersion:     cfg.Trae.IDEVersion,
		IDEVersionCode: cfg.Trae.IDEVersionCode,
		IdleTimeout:    idle,
	}
}

// resolveSpecs 依据配置推导账号清单。
//
// 优先级：显式 trae.accounts > 自动探测两个产品的默认路径（各一个账号）。
// 自动探测让"本机已登录即可用"成为默认体验，不需要用户先写配置。
func resolveSpecs(cfg *config.Config) []accountSpec {
	if len(cfg.Trae.Accounts) > 0 {
		out := make([]accountSpec, 0, len(cfg.Trae.Accounts))
		for _, a := range cfg.Trae.Accounts {
			p, ok := traectl.ProductByKey(a.Product)
			if !ok {
				log.Printf("WARN: 未知产品 %q（可选 trae-cn / solo-cn），已忽略", a.Product)
				continue
			}
			out = append(out, accountSpec{
				product: p, storagePath: a.StoragePath, label: a.Label,
				owner: a.Owner, maxInFlight: a.MaxInFlight, disabled: a.Disabled,
				proxy: a.Proxy,
			})
		}
		return out
	}
	out := make([]accountSpec, 0, len(traectl.Products))
	for _, p := range traectl.Products {
		out = append(out, accountSpec{product: p, owner: "public"})
	}
	return out
}

// describeCredPath 给出凭据路径用于日志。
//
// 为什么需要它：自动探测模式下 `accountSpec.storagePath` 是**空的** ——
// 真实路径由 `OpenStore` 内部用 `DefaultStoragePath` 解析。直接把空串打进日志
// 会变成「账号加载失败（）」，读者看不出到底找的是哪个目录。
//
// 三级回退：显式路径 → 解析出的默认路径 → "(默认路径)"（连 APPDATA 都没有时，
// 例如容器里 —— 那种情况错误信息本身已经说明了原因，这里只要不显示空括号即可）。
func describeCredPath(p traectl.Product, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if def, err := traectl.DefaultStoragePath(p); err == nil {
		return def
	}
	return "(默认路径)"
}

// isClientNotInstalled 判断错误是否只是"客户端没装"（路径不存在）。
//
// 为什么要单独判这一类：国际版默认开启后，"本机没装国际版客户端"是**完全正常**的
// 状态。若把它和"凭据读不出来"一起打成 WARN，每个只用国内版的用户启动时都会看到
// 两条"账号加载失败" —— 噪音会淹没真正的问题。
//
// Windows 上 `os.IsNotExist` 对 `open C:\...: The system cannot find the path
// specified.` 可能返回 false（那是 `ERROR_PATH_NOT_FOUND` 而不是
// `ERROR_FILE_NOT_FOUND`），所以除了 `errors.Is(err, fs.ErrNotExist)`，
// 还要看错误文本 —— 这是 Windows 上很常见的一个坑。
func isClientNotInstalled(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"cannot find the path specified",
		"cannot find the file specified",
		"no such file or directory",
		"the system cannot find",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// checkCredWritable 探测凭据文件所在目录是否可写，不可写时给出**可执行的修复命令**。
//
// 为什么必须在启动时做这件事：续期会把轮换后的 refreshToken **写回**凭据文件。
// 若目录不可写，失败会发生在几小时后的第一次续期，报错是模糊的 permission denied，
// 而根因（权限）在启动那一刻就已经确定了。
//
// 最常见的成因是 Docker **绑定挂载**：挂载会盖掉镜像里 RUN mkdir+chown 建立的属主，
// 容器以 10001 运行、而宿主目录属于别的 UID（通常 1000）。此时改 Dockerfile 没用，
// 只能改宿主目录属主 —— 所以这里直接把命令打出来。
func checkCredWritable(path string) {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return
	}
	// 用真实创建再删除来探测：只看权限位不够（ACL、只读挂载、SELinux 都可能让
	// 权限位"看起来可写"但实际写不进去）。
	f, err := os.CreateTemp(dir, ".trae2api-write-probe-*")
	if err != nil {
		log.Printf("WARN: 凭据目录不可写: %s", dir)
		log.Printf("      → 续期需要把新 token 写回该目录；不可写会导致 token 过期后无法自动续期。")
		log.Printf("      → 若用 Docker 绑定挂载，通常是宿主目录属主与容器内用户（默认 10001）不一致。")
		log.Printf("      → 修复（Linux 宿主）：sudo chown -R 10001:10001 <宿主上的凭据目录>")
		log.Printf("      → 修复（Windows/Mac 的 Docker Desktop）：绑定挂载权限是模拟的，通常不受影响。")
		return
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
}

// BuildChannels 打开全部账号、加载模型目录，并把账号注册进池。
//
// 账号来源有两处，都会注册进池：
//  1. config.trae.accounts（含 storage_path 指向客户端 storage.json 的情形）
//  2. accStore 里的便携快照（导入产物；网关自己拥有，可脱离客户端运行）
//
// 失败容忍：单个账号打开失败只记 WARN 并跳过（用户可能只装了其中一个产品，
// 或某个快照路径暂时不可用），不因此让整个网关起不来。
func BuildChannels(cfg *config.Config, p *pool.Pool, accStore *accountstore.Store) (map[string]*Channel, error) {
	if cfg != nil {
		traectl.SetGlobalProxy(cfg.Trae.Proxy)
	}
	specs := resolveSpecs(cfg)

	// 追加便携快照（去重：同一 product+userId 只留一份，快照优先于 config 里的同账号）
	if accStore != nil {
		entries, skipped := accStore.List()
		for _, s := range skipped {
			log.Printf("WARN: 跳过无效快照 %s", s)
		}
		for _, e := range entries {
			prod, ok := traectl.ProductByKey(e.Snap.Product)
			if !ok {
				log.Printf("WARN: 快照 %s 的产品 %q 未知，已跳过", e.Name, e.Snap.Product)
				continue
			}
			specs = append(specs, accountSpec{
				product:     prod,
				storagePath: e.Path,
				label:       e.Snap.Label,
				owner:       "public",
				proxy:       e.Snap.Proxy,
			})
		}
	}

	channels := map[string]*Channel{}
	opened := 0

	for _, s := range specs {
		rk := RealmOfProduct(s.product.Key)
		if rk == "" {
			continue
		}
		store, err := traectl.OpenStore(s.product, s.storagePath)
		if err != nil {
			// **区分「客户端没装」与「装了但读不出来」** —— 这两件事的对用户含义
			// 完全不同，混在一起打 WARN 会制造大量噪音。
			//
			// 背景：国际版默认开启后，"本机没装国际版客户端"是**完全正常**的状态，
			// 但它会走到这里。若一律打 WARN，每个只用国内版的用户启动时都会看到
			// 两条"账号加载失败"，以为出了故障。
			//
			// 判据：路径不存在 = 客户端没装（正常，INFO）；
			// 其它错误（权限、格式损坏、JSON 解析失败）= 真问题（WARN）。
			where := describeCredPath(s.product, s.storagePath)
			if isClientNotInstalled(err) {
				log.Printf("INFO: [%s] 未检测到客户端（跳过）: %s", s.product.Label, where)
				continue
			}
			log.Printf("WARN: [%s] 账号加载失败（%s）: %v", s.product.Label, where, err)
			continue
		}
		// 启动时就探一次可写性：续期要写回凭据，不可写的话失败会拖到几小时后
		// 第一次续期才暴露，且报错看不出根因（Docker 绑定挂载盖属主是最常见成因）。
		checkCredWritable(store.Path())
		cred, _, _ := store.Snapshot()
		uid := AccountUID(s.product, cred.UserID)

		ch := channels[rk]
		if ch == nil {
			// 模型目录从**实际凭据路径的同级**推导 state.vscdb，而不是从 %APPDATA%：
			// 容器/自定义部署里 APPDATA 往往不存在，而挂载整个 globalStorage 目录
			// 是最自然的做法（storage.json 与 state.vscdb 是兄弟文件）。
			catalog, cerr := traectl.LoadCatalog(s.product, traectl.StateDBPathBeside(store.Path()))
			if cerr != nil {
				log.Printf("INFO: [%s] 本地未找到 state.vscdb，使用官方内置默认模型目录: %v", s.product.Label, cerr)
				catalog = traectl.DefaultCatalog(s.product)
			}
			ch = &Channel{
				Realm: rk, Product: s.product, Catalog: catalog,
				accounts: map[string]*pool.Account{},
				client:   newTraeClient(cfg, s.product),
			}
			channels[rk] = ch
		}
		if _, dup := ch.accounts[uid]; dup {
			log.Printf("WARN: [%s] 账号 %s 重复配置（同产品同 userId），已忽略后一份", s.product.Label, uid)
			continue
		}

		label := s.label
		if label == "" {
			label = s.product.Label
		}
		if s.proxy != "" {
			store.SetProxy(s.proxy)
		}
		a := &pool.Account{
			UID: uid, Realm: rk, Label: label, Product: s.product,
			Owner: s.owner, Store: store, Proxy: s.proxy,
		}
		p.Add(a)
		if s.disabled {
			p.SetDisabled(uid, true, "配置中标记为禁用")
		}
		ch.mu.Lock()
		ch.accounts[uid] = a
		ch.mu.Unlock()
		opened++

		log.Printf("INFO: [%s] 账号就绪: uid=%s label=%s user=%s token至=%s",
			s.product.Label, uid, label, cred.UserID, cred.ExpiredAt)
	}

	// 确保所有已知且启用的产品对应的渠道均已初始化（即使当前暂无账号），
	// 以便 /v1/models 与控制台 Playground 能立即列出各域支持的官方模型。
	for _, prod := range traectl.Products {
		rk := RealmOfProduct(prod.Key)
		if rk == "" {
			continue
		}
		if _, ok := channels[rk]; !ok {
			channels[rk] = &Channel{
				Realm:    rk,
				Product:  prod,
				Catalog:  traectl.DefaultCatalog(prod),
				accounts: map[string]*pool.Account{},
				client:   newTraeClient(cfg, prod),
			}
		}
	}

	if opened == 0 {
		log.Printf("WARN: 当前未检测到可用账号（本机未找到 Trae CN/SOLO 客户端或凭据文件）。网关已就绪，请在 Web 控制台导入账号或通过配置指定。")
	}
	for rk, ch := range channels {
		log.Printf("INFO: 渠道 %s 就绪: %s, %d 个账号, %d 个模型, function=%s",
			rk, ch.Product.Label, len(ch.accounts), len(ch.Catalog.Models), ch.Product.Function)
	}
	return channels, nil
}

// Accounts 返回该渠道的账号（稳定顺序）。
func (c *Channel) Accounts() []*pool.Account {
	c.mu.RLock()
	defer c.mu.RUnlock()
	uids := make([]string, 0, len(c.accounts))
	for uid := range c.accounts {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]*pool.Account, 0, len(uids))
	for _, uid := range uids {
		out = append(out, c.accounts[uid])
	}
	return out
}

// Account 按 UID 取账号。
func (c *Channel) Account(uid string) *pool.Account {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.accounts[uid]
}

// ClientFor 返回绑定到某账号凭据的协议客户端。
func (c *Channel) ClientFor(a *pool.Account) *traectl.Client {
	cl := *c.client
	cl.Store = a.Store
	cl.Proxy = a.Proxy
	return &cl
}

// ModelEntries 返回 /v1/models 条目（带 realm 前缀）。
func (c *Channel) ModelEntries() []map[string]any {
	out := make([]map[string]any, 0, len(c.Catalog.Models))
	for _, m := range c.Catalog.Models {
		entry := map[string]any{
			"id":                  c.Realm + ":" + m.ID,
			"object":              "model",
			"owned_by":            c.Product.Label,
			"realm":               c.Realm,
			"display_name":        m.Display,
			"multimodal":          m.Multimodal,
			"native_tool_calling": m.NativeFC,
			"parallel_tool_calls": m.ParallelFC,
			"context_window":      m.ContextWindow,
			"prompt_max_tokens":   m.PromptMaxTokens,
			"is_beta":             m.IsBeta,

			// ---- 倍率与能力标识（2026-10 补齐）----
			//
			// 倍率用 fee_model_level 原值 + 一个**已格式化**的显示串。
			// 为什么要两个：0 表示"上游未下发"，直接展示成 "0x" 会让人以为免费，
			// 所以 display 侧统一处理成 "—"。
			"fee_model_level":   m.FeeModelLevel,
			"consumption_rate":  m.ConsumptionRate,
			"multiplier":        m.MultiplierDisplay(),
			"model_type":        m.ModelType,
			"reasoning":         m.Reasoning,
			"reasoning_efforts": m.ReasoningEfforts,
			"reasoning_default": m.ReasoningDefault,
			"max_mode":          m.MaxMode,
			"max_tokens":        m.MaxTokens,
			"max_turn":          m.MaxTurn,
			"dev_context":       m.DevContext,
			"max_context":       m.MaxContext,
			"is_default":        m.IsDefault,
			"is_new":            m.IsNew,
			"icon_url":          m.IconURL,
			"tags":              m.Tags,
			"function":          m.Function,
		}
		out = append(out, entry)
	}
	return out
}

// DefaultModel 返回该渠道的默认模型（优先 glm-5.2，其次字典序首个）。
func (c *Channel) DefaultModel() string {
	if _, ok := c.Catalog.ByID("glm-5.2"); ok {
		return "glm-5.2"
	}
	if len(c.Catalog.Models) > 0 {
		return c.Catalog.Models[0].ID
	}
	return "glm-5.2"
}
