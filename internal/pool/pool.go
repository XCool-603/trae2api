// Package pool 实现 Trae2Api 的账号池：选号、分级冷却、熔断、并发租约、状态持久化。
//
// 架构借鉴自 workbuddy2api（分池选号 / 三因子加权 / 冷却与熔断分离 / 并发租约 /
// 状态落盘），但为本项目独立实现，并针对 Trae 的特点做了取舍：
//
//   - **额度是按账号的**：Trae 的积分余量随账号不同，且官方免费额度每日重置，
//     因此把"余额"作为权重因子之一（未知时不惩罚，而不是当 0）。
//   - **模型级冷却是一等公民**：上游会对单个模型返回 4001（该账号对该模型不可用），
//     只冷却该模型而不冻整个账号 —— 换模型即豁免。
//   - **凭证失效与限流分开**：前者需要用户重新登录（长冷却 + 明确提示），
//     后者只是短时避让（短冷却 + 指数退避）。
package pool

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"trae2api/internal/traectl"
)

// CoolKind 冷却类型（决定选号时是否允许兜底参与）。
type CoolKind int

const (
	// CoolSoft 限流类短冷却：到期即可恢复，冷却期间仍可作为兜底候选（可能已恢复）。
	CoolSoft CoolKind = iota
	// CoolHard 硬冷却：凭证失效/额度耗尽等"调了必然失败"的状态，
	// **不参与任何兜底**（避免浪费轮换并产生噪音日志）。
	CoolHard
)

func (k CoolKind) String() string {
	if k == CoolHard {
		return "hard"
	}
	return "soft"
}

// Account 是池内一个 Trae 账号（凭据 + 运行时状态）。
type Account struct {
	// UID 池内唯一标识，形如 "trae-cn-916348159159136"。
	UID string
	// Realm 所属域（trae / solo）。
	Realm string
	// Label 人类可读标签。
	Label string
	// Product 产品常量（ClientID / PlatformCode / function 等）。
	Product traectl.Product
	// Owner 归属用户；"public" 表示全员可用（多租户隔离）。
	Owner string
	// Store 凭据存储句柄（协议层用它取 token / 续期）。
	Store *traectl.Store
	// Proxy 该账号专用的出站代理地址（http/https/socks5；空 = 走全局/系统代理）。
	Proxy string

	mu            sync.Mutex
	disabled      bool
	disableReason string
	coolingUntil  time.Time
	coolKind      CoolKind
	coolReason    string
	coolStreak    int                  // 连续软冷却次数（指数退避用）
	modelCooling  map[string]time.Time // 模型 → 冷却截止
	inFlight      int
	maxInFlight   int
	fails         int       // 连续失败（熔断计数）
	breakerUntil  time.Time // 熔断截止
	lastUsed      time.Time
	okCount       int64
	errCount      int64
	credits       int64
	creditsTotal  int64 // 总额度（用于算剩余比例；0 = 未知）
	creditsKnown  bool
	// expiringCredits 是**即将过期**的额度量（由上游权益包的 EndTime 算出）。
	// 用途：优先消耗快过期的额度，避免白白作废（参考实现的三因子之一）。
	expiringCredits int64
	// usedSeq 是"最近一次被选中"的单调序号（LRU 兜底用）。
	// 0 = 从未被选中。越小越久没用过。
	usedSeq uint64

	// consecutiveFails 是"**不知道原因的失败**"的连败计数（见 NoteFailures）。
	// 阈值为 0 时禁用降权（NoteFailures 直接返回）。
	// 与 fails 的区别：fails 计的是"已分类的失败"（会触发熔断），
	// consecutiveFails 计的是"未分类/传输层失败"—— 单次不罚，但持续坏会被降权。
	consecutiveFails int
	// degradeUntil 是连败降权的截止时刻。与冷却/熔断**并列取更长者**
	//（healthyLocked 里是或门，任一未到期即不可选），不叠加。
	degradeUntil time.Time
}

// Pool 账号池。
type Pool struct {
	mu    sync.RWMutex
	byUID map[string]*Account
	order []string // 稳定顺序（控制台输出确定性）

	maxInFlight      int
	breakerThreshold int
	breakerCooldown  time.Duration
	idlePerHour      float64
	idleMax          float64
	pickGap          time.Duration

	softRateBase time.Duration
	softRateMax  time.Duration
	modelBase    time.Duration

	// 连败降权参数（见 NoteFailures）。阈值 <=0 表示禁用。
	degradeThreshold int
	degradeCooldown  time.Duration

	rnd      *rand.Rand
	lastPick time.Time // 防惊群：两次选号最小间隔
	// seq 是单调递增的选号序号，给 Account.usedSeq 赋值，用于 LRU 兜底。
	// 用原子操作，因为 Pick 可能在并发请求里同时跑。
	seq atomic.Uint64

	// slotCh 名额释放通知 channel，用于事件驱动唤醒排队等待的请求
	slotCh chan struct{}

	stateFile string
}

// Options 池参数（由 config 映射而来）。
type Options struct {
	MaxInFlight      int
	BreakerThreshold int
	BreakerCooldown  time.Duration
	IdlePerHour      float64
	IdleMax          float64
	PickGap          time.Duration
	SoftRateBase     time.Duration
	SoftRateMax      time.Duration
	ModelBase        time.Duration
	StateFile        string
	// DegradeThreshold 连败降权阈值：连续这么多次"不知道原因的失败"后临时出池。
	// 0 用默认值。<=0 之外的负值视为"禁用降权"（见 New 的处理）。
	DegradeThreshold int
	// DegradeCooldown 连败降权的时长。
	DegradeCooldown time.Duration
}

// 连败降权的默认参数。
const (
	// defaultDegradeThreshold 连续多少次"不知道原因的失败"后临时出池。
	//
	// 取 5 的理由：要明显高于"一次网络抖动"（1~2 次）与"短暂的连接池问题"
	//（2~3 次），否则会把健康账号误伤；又要低到"持续坏掉的账号"能在
	// 用户察觉之前就被止损。
	defaultDegradeThreshold = 5
	// defaultDegradeCooldown 降权时长。
	//
	// 取 5 分钟：短于账号级熔断（30 分钟），因为这里罚的是"说不清原因的失败"，
	// 证据强度低于明确的上游拒绝 —— 罚得轻一点、给它更快的恢复机会。
	defaultDegradeCooldown = 5 * time.Minute
	// degradeCooldownMax 降权时长上限（防止配置写了个离谱的值把账号永久排除）。
	degradeCooldownMax = time.Hour
)

// New 构造账号池。
func New(opt Options) *Pool {
	if opt.MaxInFlight <= 0 {
		opt.MaxInFlight = 3
	}
	if opt.BreakerThreshold <= 0 {
		opt.BreakerThreshold = 3
	}
	if opt.BreakerCooldown <= 0 {
		opt.BreakerCooldown = 30 * time.Minute
	}
	if opt.SoftRateBase <= 0 {
		opt.SoftRateBase = 10 * time.Minute
	}
	if opt.SoftRateMax <= 0 {
		opt.SoftRateMax = 2 * time.Hour
	}
	if opt.DegradeThreshold == 0 {
		opt.DegradeThreshold = defaultDegradeThreshold
	}
	if opt.DegradeCooldown <= 0 {
		opt.DegradeCooldown = defaultDegradeCooldown
	}
	if opt.ModelBase <= 0 {
		opt.ModelBase = 10 * time.Minute
	}
	return &Pool{
		byUID:            map[string]*Account{},
		maxInFlight:      opt.MaxInFlight,
		breakerThreshold: opt.BreakerThreshold,
		breakerCooldown:  opt.BreakerCooldown,
		idlePerHour:      opt.IdlePerHour,
		idleMax:          opt.IdleMax,
		pickGap:          opt.PickGap,
		softRateBase:     opt.SoftRateBase,
		softRateMax:      opt.SoftRateMax,
		modelBase:        opt.ModelBase,
		degradeThreshold: opt.DegradeThreshold,
		degradeCooldown:  opt.DegradeCooldown,
		rnd:              rand.New(rand.NewSource(time.Now().UnixNano())),
		slotCh:           make(chan struct{}, 1),
		stateFile:        opt.StateFile,
	}
}

// Add 注册账号（重复 UID 覆盖）。
func (p *Pool) Add(a *Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a.Owner == "" {
		a.Owner = "public"
	}
	if a.maxInFlight <= 0 {
		a.maxInFlight = p.maxInFlight
	}
	if a.modelCooling == nil {
		a.modelCooling = map[string]time.Time{}
	}
	if _, ok := p.byUID[a.UID]; !ok {
		p.order = append(p.order, a.UID)
		sort.Strings(p.order)
	}
	p.byUID[a.UID] = a
}

// Remove 移除账号。
func (p *Pool) Remove(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.byUID[uid]; !ok {
		return
	}
	delete(p.byUID, uid)
	out := p.order[:0]
	for _, u := range p.order {
		if u != uid {
			out = append(out, u)
		}
	}
	p.order = out
}

// Get 按 UID 取账号。
func (p *Pool) Get(uid string) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.byUID[uid]
}

// All 返回全部账号（稳定顺序）。
func (p *Pool) All() []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Account, 0, len(p.order))
	for _, u := range p.order {
		if a, ok := p.byUID[u]; ok {
			out = append(out, a)
		}
	}
	return out
}

// ---- 状态查询 ----

// containsOwner 报告 owners 中是否包含 o（大小写不敏感：用户名可能大小写不一）。
func containsOwner(owners []string, o string) bool {
	for _, x := range owners {
		if strings.EqualFold(x, o) {
			return true
		}
	}
	return false
}

// healthyForModel 报告账号当前是否可用于指定模型。
func (a *Account) healthyForModel(now time.Time, model string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.healthyLocked(now, model)
}

func (a *Account) healthyLocked(now time.Time, model string) bool {
	if a.disabled {
		return false
	}
	if now.Before(a.breakerUntil) {
		return false
	}
	// 连败降权：与冷却/熔断并列（或门），生效的自然是三者中最远的那个。
	if now.Before(a.degradeUntil) {
		return false
	}
	if !a.coolingUntil.IsZero() && now.Before(a.coolingUntil) {
		return false
	}
	if model != "" {
		if until, ok := a.modelCooling[model]; ok && now.Before(until) {
			return false
		}
	}
	if a.maxInFlight > 0 && a.inFlight >= a.maxInFlight {
		return false
	}
	return true
}

// weight 计算选号权重。
//
// 公式（**加性**，对齐 workbuddy2api 的设计）：
//
//	weight = 1.0                          ← 基准，保证永不为 0
//	       + creditFactor                 ← 余额因子，0..10
//	       + idleFactor                   ← 空闲补偿，0..idleMax
//
// 为什么是加性而不是乘性：乘性下一个因子为 0（例如余额耗尽、成功率样本为 0）
// 会把整个权重清零，账号被**彻底饿死**——即便它其实是可用的。加性下每个因子
// 只做"加分"，最差情况仍保留基准权重，账号不会被永久排除。
//
// 成功率**不作为权重因子**（workbuddy2api 已把成功率 EMA 删掉）：失败由
// 熔断器与健康过滤负责，让它在权重里再罚一次是重复惩罚，且样本少时会放大噪声。
func (a *Account) weight(now time.Time, idlePerHour, idleMax float64) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	w := 1.0

	// 因子 1：空闲补偿（久未使用加权，避免热点账号被反复打）
	if !a.lastUsed.IsZero() {
		bonus := now.Sub(a.lastUsed).Hours() * idlePerHour
		if bonus < 0 {
			bonus = 0
		}
		if bonus > idleMax {
			bonus = idleMax
		}
		w += bonus
	} else {
		// 从未使用过：给满额空闲补偿，让新号优先被试一次
		w += idleMax
	}

	// 因子 2：**快过期额度优先**（参考实现的三因子之一）。
	//
	// 为什么要它：多个账号时若均匀轮询，快过期的额度会一直没被用到而白白作废。
	// 加上这个因子后，作废风险高的账号会自然被优先选中。
	//
	// 按**占比**而不是绝对量：同样是"3 天后过期 100 分"，
	// 在总额 200 的账号上是 50%（很急），在总额 10000 的账号上是 1%（不急）。
	if a.creditsTotal > 0 && a.expiringCredits > 0 {
		ratio := float64(a.expiringCredits) / float64(a.creditsTotal)
		if ratio > 1 {
			ratio = 1
		}
		w += ratio * 8
	}

	// 因子 3：余额（未知时**不加不减**，而不是当 0 —— 否则新号永远学不到）
	if a.creditsKnown {
		switch {
		case a.credits <= 0:
			w += 0 // 已耗尽：不加分但也不清零（可能只是缓存陈旧）
		case a.creditsTotal > 0:
			// 按剩余比例给分（0..10）
			ratio := float64(a.credits) / float64(a.creditsTotal)
			if ratio > 1 {
				ratio = 1
			}
			w += ratio * 10
		default:
			// 不知道总量时按绝对档位粗估
			switch {
			case a.credits >= 1000:
				w += 10
			case a.credits >= 100:
				w += 5
			default:
				w += 1
			}
		}
	}
	return w
}

// Pick 选号：在 healthy 的候选里按权重随机挑一个。
//
// realm 为空表示不限域；model 为空表示不限模型；owners 为空表示不限归属
// （管理员）。普通用户应传自己的身份集合（ID 与用户名都算），此时只能调度
// public 账号与归属匹配的账号 —— 这就是多租户隔离的落点。
// tried 是本次请求已试过的 UID 集合（轮换时排除）。
func (p *Pool) Pick(realm, model string, owners []string, tried map[string]bool) *Account {
	now := time.Now()

	// 防惊群：若配置了 pickGap，在锁内计算需等待的时间，在锁外 sleep，绝不在持有排他锁时睡眠
	if p.pickGap > 0 {
		var wait time.Duration
		p.mu.Lock()
		if diff := now.Sub(p.lastPick); diff < p.pickGap {
			wait = p.pickGap - diff
			p.lastPick = now.Add(wait)
		} else {
			p.lastPick = now
		}
		p.mu.Unlock()
		if wait > 0 {
			time.Sleep(wait)
			now = time.Now()
		}
	}

	p.mu.RLock()
	cands := make([]*Account, 0, len(p.order))
	for _, u := range p.order {
		if a, ok := p.byUID[u]; ok {
			cands = append(cands, a)
		}
	}
	p.mu.RUnlock()

	type weighted struct {
		a *Account
		w float64
	}
	live := make([]weighted, 0, len(cands))
	var fallback []*Account // 软冷却中但可能已恢复的候选（兜底）

	for _, a := range cands {
		if tried != nil && tried[a.UID] {
			continue
		}
		if realm != "" && a.Realm != realm {
			continue
		}
		if len(owners) > 0 && a.Owner != "public" && !containsOwner(owners, a.Owner) {
			continue // 多租户隔离：非 public 账号只归其 owner
		}
		if a.healthyForModel(now, model) {
			live = append(live, weighted{a: a, w: a.weight(now, p.idlePerHour, p.idleMax)})
			continue
		}
		// 兜底：软冷却/熔断中但**非硬冷却**的账号仍可一试（可能已恢复，失败成本仅一轮换）。
		// 硬冷却（凭证失效/额度耗尽）绝不参与 —— 调了必然失败。
		// 模型级冷却同样必须排除：它的语义是"该账号对该模型不可用"，
		// 若允许兜底参与，模型级冷却就形同虚设（测试抓出过这个 bug）。
		a.mu.Lock()
		isHard := a.coolKind == CoolHard && !a.coolingUntil.IsZero() && now.Before(a.coolingUntil)
		// 连败降权**也不参与兜底**：它的语义是"明确的出池休息"，
		// 而不是"可能已恢复、值得一试"（后者才是软冷却/熔断的兜底理由）。
		// 若允许它兜底，连败兜底就架空了 —— 被反复选中的还是那个一直坏的账号。
		degraded := now.Before(a.degradeUntil)
		modelCooled := false
		if model != "" {
			if until, ok := a.modelCooling[model]; ok && now.Before(until) {
				modelCooled = true
			}
		}
		usable := !a.disabled && !isHard && !degraded && !modelCooled &&
			(a.maxInFlight <= 0 || a.inFlight < a.maxInFlight)
		a.mu.Unlock()
		if usable {
			fallback = append(fallback, a)
		}
	}

	// 加权随机选一个，并打上本次序号（供 LRU 兜底使用）。
	pick := func(a *Account) *Account {
		if a == nil {
			return nil
		}
		seq := p.seq.Add(1)
		a.mu.Lock()
		a.usedSeq = seq
		a.mu.Unlock()
		return a
	}

	if len(live) > 0 {
		total := 0.0
		for _, x := range live {
			total += x.w
		}
		if total <= 0 {
			return pick(live[p.rnd.Intn(len(live))].a)
		}
		r := p.rnd.Float64() * total
		for _, x := range live {
			r -= x.w
			if r <= 0 {
				return pick(x.a)
			}
		}
		return pick(live[len(live)-1].a)
	}

	// 兜底：全部候选都在冷却/熔断中。此时用 **LRU（最久未被选中的）** 而不是随机 ——
	// 随机会反复挑中同一个号，让它在"刚失败→立刻又被选中"之间循环；
	// LRU 至少保证轮着试，尽快把整个池都探一遍。
	// （workbuddy2api 用单调序号 usedSeq 实现同一语义，范围是**全候选**。）
	if len(fallback) > 0 {
		best := fallback[0]
		bestSeq := best.usedSeqLocked()
		for _, a := range fallback[1:] {
			if s := a.usedSeqLocked(); s < bestSeq {
				best, bestSeq = a, s
			}
		}
		return pick(best)
	}
	return nil
}

// usedSeqLocked 读取 usedSeq（加锁）。
func (a *Account) usedSeqLocked() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.usedSeq
}

// Acquire 获取一个在途名额（返回 false 表示已满）。
func (p *Pool) Acquire(uid string) bool {
	a := p.Get(uid)
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.maxInFlight > 0 && a.inFlight >= a.maxInFlight {
		return false
	}
	a.inFlight++
	a.lastUsed = time.Now()
	return true
}

// Release 释放在途名额。
func (p *Pool) Release(uid string) {
	a := p.Get(uid)
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.inFlight > 0 {
		a.inFlight--
	}
	a.lastUsed = time.Now()
	a.mu.Unlock()

	// 通知排队等待的协程立即尝试获取名额
	select {
	case p.slotCh <- struct{}{}:
	default:
	}
}

// SlotNotify 返回名额释放通知 channel（用于事件驱动唤醒排队等待的请求）。
func (p *Pool) SlotNotify() <-chan struct{} {
	return p.slotCh
}

// ---- 冷却 / 熔断 / 健康记账 ----

// Cooldown 账号级冷却（取更晚的到期时间，避免短冷却覆盖长冷却）。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	a := p.Get(uid)
	if a == nil || d <= 0 {
		return
	}
	until := time.Now().Add(d)
	a.mu.Lock()
	defer a.mu.Unlock()
	if until.After(a.coolingUntil) {
		a.coolingUntil = until
		a.coolReason = reason
	}
	a.coolKind = kind
	if kind == CoolHard {
		// 硬冷却：清空模型级豁免（账号整体不可用，豁免没有意义）
		a.modelCooling = map[string]time.Time{}
	}
}

// CooldownSoftRate 限流类软冷却：按连续次数指数退避，封顶 softRateMax。
func (p *Pool) CooldownSoftRate(uid, reason string) time.Duration {
	a := p.Get(uid)
	if a == nil {
		return 0
	}
	a.mu.Lock()
	a.coolStreak++
	streak := a.coolStreak
	a.mu.Unlock()

	d := p.softRateBase
	for i := 1; i < streak; i++ {
		d *= 2
		if d >= p.softRateMax {
			d = p.softRateMax
			break
		}
	}
	p.Cooldown(uid, CoolSoft, d, reason)
	return d
}

// CooldownModel 模型级冷却（只冷却该模型，换模型即豁免）。
func (p *Pool) CooldownModel(uid, model string, d time.Duration, reason string) {
	a := p.Get(uid)
	if a == nil || model == "" || d <= 0 {
		return
	}
	until := time.Now().Add(d)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.modelCooling == nil {
		a.modelCooling = map[string]time.Time{}
	}
	if cur, ok := a.modelCooling[model]; !ok || until.After(cur) {
		a.modelCooling[model] = until
	}
	// 记一次失败计数（模型级错误同样说明该账号对该模型不可用）
	a.errCount++
}

// ClearModelCooldown 解除某模型的冷却（控制台"解除"用）。
func (p *Pool) ClearModelCooldown(uid, model string) {
	a := p.Get(uid)
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.modelCooling, model)
}

// NoteSuccess 记一次成功：清熔断计数与软冷却连击。
func (p *Pool) NoteSuccess(uid string) {
	a := p.Get(uid)
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.okCount++
	a.fails = 0
	a.breakerUntil = time.Time{}
	a.coolStreak = 0
	// 成功即证明账号是好的 → 连败计数与降权一并清零
	a.consecutiveFails = 0
	a.degradeUntil = time.Time{}
}

// NoteFailures 记录一次**"不知道原因的失败"**（不罚号的失败）。
//
// ## 为什么需要它（补上一个设计缺口）
//
// 本项目的原则是"未分类错误不罚号"—— 因为罚号的前提是能把它归类到明确的
// 账号级问题。但那留下一个洞：**一个持续坏掉但每次都报"未分类错误"的账号
// 永远不会被移出池**，只会被反复选中、反复失败。
//
// 所以加一个**连败兜底**：单次不罚，但连续 N 次都是"不知道原因的失败"，
// 就临时把它移出池一段时间。这既守住了"不因一次抖动罚号"，又能在持续坏掉时
// 真正止损。
//
// ## 谁该喂它
//
// **只喂"不知道原因的失败"**：未分类错误、传输层失败。这些是连败兜底的目标形态。
//
// **不该喂**：带权威分类的错误（401/403/429/5xx/请求不合法）—— 它们各有
// "知道原因的惩罚"（冷却/熔断/不罚），再喂连败是**重复计罚**。
func (p *Pool) NoteFailures(uid string) bool {
	if p.degradeThreshold <= 0 {
		return false // 显式禁用（阈值设成负数）
	}
	a := p.Get(uid)
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.consecutiveFails++
	if a.consecutiveFails < p.degradeThreshold {
		return false
	}
	a.consecutiveFails = 0
	now := time.Now()
	// 已在降权期内再次达阈：**不延长、不翻倍**。
	// 否则用户的重试会把降权越堆越厚，最终账号被无限期排除。
	if a.degradeUntil.Before(now) {
		a.degradeUntil = now.Add(p.degradeDuration())
	}
	return true
}

// DegradeCooldown 返回连败降权时长（供日志/展示）。
func (p *Pool) DegradeCooldown() time.Duration { return p.degradeDuration() }

// degradeDuration 返回连败降权的时长。
//
// 不做指数升级：触发计数在达阈时就清零了，档位不可推导。
// 固定时长已满足"临时出池一段时间"的语义 —— 真正持续坏的账号会在
// 下一轮连败里再被降一次。
func (p *Pool) degradeDuration() time.Duration {
	d := p.degradeCooldown
	if d <= 0 {
		d = defaultDegradeCooldown
	}
	if d > degradeCooldownMax {
		d = degradeCooldownMax
	}
	return d
}

// NoteError 记一次失败；达到阈值则熔断。
func (p *Pool) NoteError(uid string) {
	a := p.Get(uid)
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.errCount++
	a.fails++
	if a.fails >= p.breakerThreshold {
		a.breakerUntil = time.Now().Add(p.breakerCooldown)
		a.fails = 0
	}
}

// ClearCooldown 解除账号级冷却与熔断（控制台"一键解除"用）。返回是否确有变更。
func (p *Pool) ClearCooldown(uid string) bool {
	a := p.Get(uid)
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	changed := !a.coolingUntil.IsZero() || !a.breakerUntil.IsZero() || len(a.modelCooling) > 0
	a.coolingUntil = time.Time{}
	a.coolReason = ""
	a.coolStreak = 0
	a.breakerUntil = time.Time{}
	a.fails = 0
	a.modelCooling = map[string]time.Time{}
	return changed
}

// ReviveChange 描述一次"复活"实际清掉了哪几样东西。
//
// 为什么要逐项回报而不是只回一个 bool：控制台需要向用户交代
// "这个账号原本到底卡在哪"，而"清了 0 项"与"清掉了模型级冷却"是完全不同的两件事。
type ReviveChange struct {
	Cooldown          bool `json:"cooldown"`
	Breaker           bool `json:"breaker"`
	Fails             bool `json:"fails"`
	Degrade           bool `json:"degrade"`
	ModelCooldown     bool `json:"model_cooldown"`
	UnclassifiedFails int  `json:"unclassified_fails"`
}

// Changed 报告本次复活是否真的改变了任何一项。
func (c ReviveChange) Changed() bool {
	return c.Cooldown || c.Breaker || c.Fails || c.Degrade || c.ModelCooldown ||
		c.UnclassifiedFails > 0
}

// Revive 一次性清掉"这个账号为什么不被选中"的**全部**原因：
// 账号级冷却 + 熔断 + 已分类连败计数(fails) + 连败降权(degradeUntil)
// + 未分类连败计数(consecutiveFails) + 模型级冷却(modelCooling)。
//
// ## 为什么必须是一个原子方法，而不是复用 ClearCooldown
//
// ClearCooldown 只覆盖前三样与模型级冷却，**不动 degradeUntil 与 consecutiveFails**
// —— 一个"连败降权中"的账号调完 clear-cooldown 后 /status 仍是 degraded=true，
// 用户点了"复活"却看到账号还是不可用。而且它的变更判定里也不含降权，
// 于是会返回 changed=false，看起来像"什么都没发生"。
//
// 这几项都是 Account 的**非导出字段**，只有 pool 包内能碰；分成多次调用还会在
// 每次调用之间留下不一致的中间态（并发 Pick 可能正好读到一个"半复活"的账号）。
// 所以这里用**一次加锁**完成全部清理。
//
// 返回第二值为 false 表示 uid 不在池内（不是"没有变更"）。
func (p *Pool) Revive(uid string) (ReviveChange, bool) {
	a := p.Get(uid)
	if a == nil {
		return ReviveChange{}, false
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()

	// 1) 账号级冷却（软/硬都清）。只认"尚未到期"的冷却算变更：
	//    一个已经自然过期的冷却不该被报成"我救活了它"。
	ch := ReviveChange{
		Cooldown: !a.coolingUntil.IsZero() && now.Before(a.coolingUntil),
		// 2) 熔断
		Breaker: !a.breakerUntil.IsZero() && now.Before(a.breakerUntil),
		// 3) 已分类失败计数（熔断的计数来源）
		Fails: a.fails > 0,
		// 4) 连败降权
		Degrade: !a.degradeUntil.IsZero() && now.Before(a.degradeUntil),
		// 5) 模型级冷却
		ModelCooldown:     len(a.modelCooling) > 0,
		UnclassifiedFails: a.consecutiveFails,
	}

	a.coolingUntil = time.Time{}
	a.coolKind = 0 // CoolSoft（零值）；既然冷却已清，kind 只是遗留标签
	a.coolReason = ""
	a.coolStreak = 0
	a.breakerUntil = time.Time{}
	a.fails = 0
	a.degradeUntil = time.Time{}
	a.consecutiveFails = 0
	// 重建而不是 clear：nil map 在 CooldownModel 里会被延迟初始化，
	// 这里显式给一个空 map，语义上就是"没有任何模型在冷却"。
	a.modelCooling = map[string]time.Time{}
	return ch, true
}

// RuntimeOverride 是一组**仅供测试**用来直接摆放账号运行时状态的字段。
//
// 存在的唯一理由：CoolingUntil / BreakerUntil / DegradeUntil 这些都在 Account 的
// 非导出字段里，而"复活"的验收要求"调之前账号确实在冷却/熔断/降权里" ——
// 从 pool 包外无法构造这个前置状态。用公开 API 硬凑也不行：
// 降权要先调 NoteFailures 达到 degradeThreshold 次，冷却时长得靠配置里的
// 极小值凑，测试会变成"配置的测试"而不是"复活的测试"。
//
// 指针语义 = "显式提供才覆盖"，nil 表示不动那一项。
type RuntimeOverride struct {
	CoolingUntil *time.Time
	CoolKind     CoolKind
	CoolReason   string
	BreakerUntil *time.Time
	Fails        int
	DegradeUntil *time.Time
	Consecutive  int
	// ModelCooling 非 nil 时**整体替换**模型级冷却表。
	ModelCooling map[string]time.Time
	Disabled     *bool
}

// SetRuntimeForTest 摆放账号运行时状态。**仅供测试使用**（名字里的 ForTest 是
// 给调用方看的警告：生产路径走 Cooldown / NoteError / NoteFailures / SetDisabled）。
func (p *Pool) SetRuntimeForTest(uid string, o RuntimeOverride) bool {
	a := p.Get(uid)
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if o.CoolingUntil != nil {
		a.coolingUntil = *o.CoolingUntil
		a.coolKind = o.CoolKind
		a.coolReason = o.CoolReason
	}
	if o.BreakerUntil != nil {
		a.breakerUntil = *o.BreakerUntil
	}
	a.fails = o.Fails
	if o.DegradeUntil != nil {
		a.degradeUntil = *o.DegradeUntil
	}
	a.consecutiveFails = o.Consecutive
	if o.ModelCooling != nil {
		a.modelCooling = o.ModelCooling
	}
	if o.Disabled != nil {
		a.disabled = *o.Disabled
	}
	return true
}

// SetDisabled 手动启用/禁用账号。
func (p *Pool) SetDisabled(uid string, disabled bool, reason string) bool {
	a := p.Get(uid)
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.disabled = disabled
	a.disableReason = reason
	return true
}

// SetProxy 动态设置账号绑定的代理地址。
func (a *Account) SetProxy(proxyURL string) {
	a.mu.Lock()
	a.Proxy = strings.TrimSpace(proxyURL)
	store := a.Store
	a.mu.Unlock()
	if store != nil {
		store.SetProxy(proxyURL)
	}
}

// SetProxy 设置指定账号的代理地址。
func (p *Pool) SetProxy(uid, proxyURL string) bool {
	a := p.Get(uid)
	if a == nil {
		return false
	}
	a.SetProxy(proxyURL)
	return true
}

// HealthyIgnoringInflight 返回某域内"健康、但**不看**在途名额"的账号数。
//
// 用途：区分 Pick 返回 nil 的两种原因 ——
//   - 这个数 > 0 而 Pick 返回 nil → 账号健康，只是名额满了 → **值得排队等**
//   - 这个数 == 0                → 账号都在冷却/熔断/停用 → 等也没用
//
// 不做这个区分的话，要么在"账号全冷却 24 小时"时白等，要么在"只是名额满了"时
// 直接给客户端 503（客户端会以为网关坏了）。
func (p *Pool) HealthyIgnoringInflight(realm string) int {
	now := time.Now()
	n := 0
	for _, a := range p.All() {
		if realm != "" && a.Realm != realm {
			continue
		}
		a.mu.Lock()
		ok := !a.disabled &&
			!now.Before(a.breakerUntil) &&
			!now.Before(a.degradeUntil) &&
			(a.coolingUntil.IsZero() || !now.Before(a.coolingUntil))
		a.mu.Unlock()
		if ok {
			n++
		}
	}
	return n
}

// ServableCount 返回某域内**当前可被调度**的账号数。
//
// 与 Healthy 的区别：可调度还要排除"在途已满"（healthy 但此刻没名额）。
// 控制台/仪表盘上"可调度"比"健康"更能反映"现在能不能发请求"。
// realmKey 为空表示统计全部域。
func (p *Pool) ServableCount(realm string) int {
	now := time.Now()
	n := 0
	for _, a := range p.All() {
		if realm != "" && a.Realm != realm {
			continue
		}
		if a.healthyForModel(now, "") {
			n++
		}
	}
	return n
}

// SetCredits 更新账号余额（供选号权重与展示）。总量未知时只记余量。
func (p *Pool) SetCredits(uid string, credits int64) {
	a := p.Get(uid)
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.credits = credits
	a.creditsKnown = true
}

// SetCreditsDetailed 记录余量、总量与**即将过期量**。
//
// expiring 来自上游权益包的 EndTime（见 traectl.SumExpiringCredits）。
// 它让选号能"优先烧快过期的额度"—— 多个账号时，先用作废风险高的那个，
// 而不是均匀轮询到过期为止。
func (p *Pool) SetCreditsDetailed(uid string, remain, total, expiring int64) {
	a := p.Get(uid)
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.credits = remain
	a.creditsTotal = total
	a.expiringCredits = expiring
	a.creditsKnown = true
}

// ---- 状态快照（供 /status 与控制台）----

// Status 是账号的运行时可观测状态。
type Status struct {
	UID           string `json:"uid"`
	Realm         string `json:"realm"`
	Label         string `json:"label"`
	Owner         string `json:"owner"`
	Product       string `json:"product"`
	Disabled      bool   `json:"disabled"`
	DisableReason string `json:"disable_reason,omitempty"`
	Cooling       bool   `json:"cooling"`
	CoolKind      string `json:"cool_kind,omitempty"`
	CoolReason    string `json:"cool_reason,omitempty"`
	CoolRemainSec int64  `json:"cool_remain_seconds,omitempty"`
	Breaker       bool   `json:"breaker"`
	BreakerRemain int64  `json:"breaker_remain_seconds,omitempty"`
	// Degraded 连败降权中（见 NoteFailures）。与冷却/熔断并列，取更长者生效。
	Degraded          bool             `json:"degraded"`
	DegradeRemain     int64            `json:"degrade_remain_seconds,omitempty"`
	UnclassifiedFails int              `json:"unclassified_fails"`
	ModelCooling      map[string]int64 `json:"model_cooling,omitempty"`
	InFlight          int              `json:"in_flight"`
	MaxInFlight       int              `json:"max_in_flight"`
	Healthy           bool             `json:"healthy"`
	Fails             int              `json:"consecutive_fails"`
	OKCount           int64            `json:"ok_count"`
	ErrCount          int64            `json:"err_count"`
	Credits           *int64           `json:"credits,omitempty"`
	LastUsed          string           `json:"last_used,omitempty"`
	TokenExpire       string           `json:"token_expire,omitempty"`
	Proxy             string           `json:"proxy,omitempty"`
}

// Status 返回单账号状态。
func (a *Account) Status(now time.Time) Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := Status{
		UID: a.UID, Realm: a.Realm, Label: a.Label, Owner: a.Owner,
		Product: a.Product.Key, Disabled: a.disabled, DisableReason: a.disableReason,
		Proxy: a.Proxy,
		InFlight: a.inFlight, MaxInFlight: a.maxInFlight,
		Fails: a.fails, OKCount: a.okCount, ErrCount: a.errCount,
		UnclassifiedFails: a.consecutiveFails,
		Degraded:          now.Before(a.degradeUntil),
	}
	if !a.coolingUntil.IsZero() && now.Before(a.coolingUntil) {
		st.Cooling = true
		st.CoolKind = a.coolKind.String()
		st.CoolReason = a.coolReason
		st.CoolRemainSec = int64(time.Until(a.coolingUntil).Seconds()) + 1
	}
	if !a.breakerUntil.IsZero() && now.Before(a.breakerUntil) {
		st.Breaker = true
		st.BreakerRemain = int64(time.Until(a.breakerUntil).Seconds()) + 1
	}
	// 注意：这一条必须**在熔断块之外** —— 降权与熔断是独立的两个截止时刻，
	// 放进熔断块里会导致"只有熔断时才显示降权剩余"，而那正是最不需要它的场景。
	if now.Before(a.degradeUntil) {
		st.DegradeRemain = int64(time.Until(a.degradeUntil).Seconds()) + 1
	}
	if len(a.modelCooling) > 0 {
		mc := map[string]int64{}
		for m, until := range a.modelCooling {
			if now.Before(until) {
				mc[m] = int64(time.Until(until).Seconds()) + 1
			}
		}
		if len(mc) > 0 {
			st.ModelCooling = mc
		}
	}
	if a.creditsKnown {
		c := a.credits
		st.Credits = &c
	}
	if !a.lastUsed.IsZero() {
		st.LastUsed = a.lastUsed.Format(time.RFC3339)
	}
	if a.Store != nil {
		cred, _, _ := a.Store.Snapshot()
		st.TokenExpire = cred.ExpiredAt
	}
	st.Healthy = a.healthyLocked(now, "")
	return st
}

// List 返回全部账号状态。
func (p *Pool) List() []Status {
	now := time.Now()
	accts := p.All()
	out := make([]Status, 0, len(accts))
	for _, a := range accts {
		out = append(out, a.Status(now))
	}
	return out
}

// Counts 汇总（total/healthy/cooling/disabled/inFlightFull）。
func (p *Pool) Counts() (total, healthy, cooling, disabled, inFlightFull int) {
	now := time.Now()
	for _, a := range p.All() {
		total++
		a.mu.Lock()
		switch {
		case a.disabled:
			disabled++
		case !a.coolingUntil.IsZero() && now.Before(a.coolingUntil):
			cooling++
		case a.maxInFlight > 0 && a.inFlight >= a.maxInFlight:
			inFlightFull++
		default:
			healthy++
		}
		a.mu.Unlock()
	}
	return
}

// CountsForRealm 按域汇总。
func (p *Pool) CountsForRealm(realm string) (total, healthy, cooling, disabled, inFlightFull int) {
	now := time.Now()
	for _, a := range p.All() {
		if realm != "" && a.Realm != realm {
			continue
		}
		total++
		a.mu.Lock()
		switch {
		case a.disabled:
			disabled++
		case !a.coolingUntil.IsZero() && now.Before(a.coolingUntil):
			cooling++
		case a.maxInFlight > 0 && a.inFlight >= a.maxInFlight:
			inFlightFull++
		default:
			healthy++
		}
		a.mu.Unlock()
	}
	return
}

// Servable 报告是否存在可服务的账号（realm 为空则不限域）。
func (p *Pool) Servable(realm string) bool {
	now := time.Now()
	for _, a := range p.All() {
		if realm != "" && a.Realm != realm {
			continue
		}
		if a.healthyForModel(now, "") {
			return true
		}
	}
	return false
}

// ---- 状态持久化 ----

// persisted 是落盘的最小状态集。
//
// 只持久化"跨重启仍需保留"的运行时判断：冷却、熔断、手动禁用、计数、余额。
// **不持久化 token/凭据** —— 那些的唯一事实来源是客户端 storage.json，
// 落盘会引入两份可能不一致的副本（refreshToken 是轮换语义，多副本必坏事）。
type persisted struct {
	Version int                      `json:"version"`
	SavedAt string                   `json:"saved_at"`
	Items   map[string]persistedItem `json:"items"`
}

type persistedItem struct {
	Disabled          bool              `json:"disabled,omitempty"`
	DisableReason     string            `json:"disable_reason,omitempty"`
	CoolingUntil      string            `json:"cooling_until,omitempty"`
	CoolKind          CoolKind          `json:"cool_kind,omitempty"`
	CoolReason        string            `json:"cool_reason,omitempty"`
	CoolStreak        int               `json:"cool_streak,omitempty"`
	ModelCooling      map[string]string `json:"model_cooling,omitempty"`
	BreakerUntil      string            `json:"breaker_until,omitempty"`
	Fails             int               `json:"fails,omitempty"`
	DegradeUntil      string            `json:"degrade_until,omitempty"`
	UnclassifiedFails int               `json:"unclassified_fails,omitempty"`
	OKCount           int64             `json:"ok_count,omitempty"`
	ErrCount          int64             `json:"err_count,omitempty"`
	Credits           int64             `json:"credits,omitempty"`
	CreditsKnown      bool              `json:"credits_known,omitempty"`
}

// Save 落盘状态快照（原子写）。
func (p *Pool) Save() error {
	if p.stateFile == "" {
		return nil
	}
	snap := persisted{Version: 1, SavedAt: time.Now().Format(time.RFC3339), Items: map[string]persistedItem{}}
	for _, a := range p.All() {
		a.mu.Lock()
		it := persistedItem{
			Disabled: a.disabled, DisableReason: a.disableReason,
			CoolKind: a.coolKind, CoolReason: a.coolReason, CoolStreak: a.coolStreak,
			Fails: a.fails, OKCount: a.okCount, ErrCount: a.errCount,
			UnclassifiedFails: a.consecutiveFails,
			Credits:           a.credits, CreditsKnown: a.creditsKnown,
		}
		if !a.coolingUntil.IsZero() {
			it.CoolingUntil = a.coolingUntil.Format(time.RFC3339)
		}
		if !a.breakerUntil.IsZero() {
			it.BreakerUntil = a.breakerUntil.Format(time.RFC3339)
		}
		// 必须与熔断**平级**：降权是独立的截止时刻，嵌进熔断块会导致
		// "只有熔断时才保存降权" —— 而连败降权恰恰是熔断未触发时才会发生的。
		if !a.degradeUntil.IsZero() {
			it.DegradeUntil = a.degradeUntil.Format(time.RFC3339)
		}
		if len(a.modelCooling) > 0 {
			it.ModelCooling = map[string]string{}
			for m, until := range a.modelCooling {
				it.ModelCooling[m] = until.Format(time.RFC3339)
			}
		}
		a.mu.Unlock()
		snap.Items[a.UID] = it
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("pool: 序列化状态: %w", err)
	}
	dir := filepath.Dir(p.stateFile)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("pool: 创建状态目录: %w", err)
		}
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("pool: 创建临时文件: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("pool: 写状态: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("pool: 关闭临时文件: %w", err)
	}
	if err := os.Rename(name, p.stateFile); err != nil {
		os.Remove(p.stateFile)
		if err2 := os.Rename(name, p.stateFile); err2 != nil {
			os.Remove(name)
			return fmt.Errorf("pool: 替换状态文件: %w", err2)
		}
	}
	return nil
}

// Restore 从状态文件恢复运行时状态（账号不存在则跳过；未知 UID 不报错）。
func (p *Pool) Restore() error {
	if p.stateFile == "" {
		return nil
	}
	raw, err := os.ReadFile(p.stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("pool: 读取状态: %w", err)
	}
	var snap persisted
	if err := json.Unmarshal(raw, &snap); err != nil {
		return fmt.Errorf("pool: 解析状态: %w", err)
	}
	parse := func(s string) time.Time {
		if strings.TrimSpace(s) == "" {
			return time.Time{}
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}
		}
		return t
	}
	for uid, it := range snap.Items {
		a := p.Get(uid)
		if a == nil {
			continue // 账号已被移除：静默跳过（不报错，便于换号）
		}
		a.mu.Lock()
		a.disabled = it.Disabled
		a.disableReason = it.DisableReason
		a.coolingUntil = parse(it.CoolingUntil)
		a.coolKind = it.CoolKind
		a.coolReason = it.CoolReason
		a.coolStreak = it.CoolStreak
		a.breakerUntil = parse(it.BreakerUntil)
		a.degradeUntil = parse(it.DegradeUntil)
		a.consecutiveFails = it.UnclassifiedFails
		a.fails = it.Fails
		a.okCount = it.OKCount
		a.errCount = it.ErrCount
		a.credits = it.Credits
		a.creditsKnown = it.CreditsKnown
		if len(it.ModelCooling) > 0 {
			a.modelCooling = map[string]time.Time{}
			for m, s := range it.ModelCooling {
				if t := parse(s); !t.IsZero() {
					a.modelCooling[m] = t
				}
			}
		}
		a.mu.Unlock()
	}
	return nil
}
