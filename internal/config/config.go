// Package config 加载 Trae2Api 的 JSON 配置。
//
// 设计取向（借鉴 workbuddy2api）：缺省即可跑 —— 所有字段都有合理默认值，
// 用户只需写想改的部分；配置缺失时自动生成模板。
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"trae2api/internal/prompt"
)

// Account 是一个 Trae 账号的凭据来源。
type Account struct {
	// Product 产品 key：trae-cn（Trae CN）或 solo-cn（TRAE SOLO CN）。
	Product string `json:"product"`
	// StoragePath 该账号的 storage.json 路径；空 = %APPDATA% 默认路径。
	StoragePath string `json:"storage_path"`
	// Label 人类可读标签（控制台与日志用）。
	Label string `json:"label"`
	// Owner 归属用户（多租户隔离）；空 = public（全员可用）。
	Owner string `json:"owner"`
	// MaxInFlight 该账号的在途并发上限；0 = 用 pool.max_in_flight。
	MaxInFlight int `json:"max_in_flight"`
	// Disabled 初始禁用（保留凭证但不参与选号）。
	Disabled bool `json:"disabled"`
	// Proxy 该账号绑定的出站代理（支持 http/https/socks5，如 socks5://127.0.0.1:1080）。空 = 继承全局代理或系统代理。
	Proxy string `json:"proxy"`
}

// Config 是 Trae2Api 的完整配置。
type Config struct {
	// Listen HTTP 监听地址，默认 127.0.0.1:7870。
	Listen string `json:"listen"`
	// APIKey 访问 /v1/* 所需的 Bearer Token；空 = 不鉴权（单机模式）。
	APIKey string `json:"api_key"`
	// WebPassword 控制台密码；空 = 免密。
	WebPassword string `json:"web_password"`
	// TrustProxyHeaders 是否采信 X-Forwarded-For / X-Real-IP 作为真实来源 IP。
	//
	// ⚠️ **默认 false，且只在确实位于可信反向代理后面时才开**。
	// 开启后，攻击者可以伪造这两个头来绕过登录限流（每次换一个假 IP）。
	// 关着的时候代理后的所有请求会算作同一个 IP（限流偏严）—— 宁可偏严，
	// 也不要"看起来有限流、实际等于没有"。
	TrustProxyHeaders bool `json:"trust_proxy_headers"`
	// DataDir 状态与日志目录，默认 ./data。
	DataDir string `json:"data_dir"`

	Pool struct {
		// MaxInFlight 单账号在途并发上限（默认 3）。
		MaxInFlight int `json:"max_in_flight"`
		// BreakerThreshold 连续失败熔断阈值（默认 3）。
		BreakerThreshold int `json:"breaker_threshold"`
		// BreakerCooldown 熔断冷却时长（默认 "30m"）。
		BreakerCooldown string `json:"breaker_cooldown"`
		// IdleWeightPerHour 每空闲 1 小时的权重加成（默认 0.5）。
		IdleWeightPerHour float64 `json:"idle_weight_per_hour"`
		// IdleWeightMax 空闲权重上限（默认 5.0）。
		IdleWeightMax float64 `json:"idle_weight_max"`
		// PickGap 两次选号之间的最小间隔，防惊群（默认 "100ms"）。
		PickGap string `json:"pick_gap"`
	} `json:"pool"`

	Cooldown struct {
		// SoftRate 限流类软冷却基数（默认 "600s"）。
		SoftRate string `json:"soft_rate"`
		// SoftRateMax 软冷却指数退避封顶（默认 "2h"）。
		SoftRateMax string `json:"soft_rate_max"`
		// ModelCooldown 模型级冷却基数（默认 "600s"）。
		ModelCooldown string `json:"model_cooldown"`
	} `json:"cooldown"`

	Trae struct {
		// Accounts 账号清单。空 = 自动探测本机已登录的产品（各一个账号）。
		Accounts []Account `json:"accounts"`
		// ChatBase 覆盖 chat 上游 base；空 = https://trae-api-cn.mchost.guru。
		ChatBase string `json:"chat_base"`
		// UserAgent 覆盖出站 User-Agent。**强烈建议留空** —— 内置值取自官方客户端
		// 内置 node-fetch 的默认 UA；Go 默认 UA（Go-http-client/1.1）会被上游按
		// 机器人识别并返回 HTTP 200 + 业务码 4011「超出限流」，错误信息完全指向
		// 限流，与真实原因无关，极难排查。
		UserAgent string `json:"user_agent"`
		// IDEVersion / IDEVersionCode 出站请求头覆盖。
		// version-code 必须纯数字：点分 build 串会被上游以 4001 拒绝。
		IDEVersion     string `json:"ide_version"`
		IDEVersionCode string `json:"ide_version_code"`
		// IdleTimeoutSeconds 流中空闲阈值（秒）：SSE 流超过这么久没吐数据就断流。
		//
		// 为什么需要它：ResponseHeaderTimeout 只管到"响应头到达"为止。头一到，
		// 上游就可以**永远不吐数据**而不触发任何超时 —— 连接占着、账号租约占着、
		// 客户端干等。0 = 禁用（不推荐）。
		//
		// ⚠️ 不要用"整体请求超时"代替：那会把推理模型思考几分钟这种正常长回复误杀。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
		// StrictModels 模型名必须存在于本地目录（默认 **true**）。
		//
		// 为什么默认严格：实测上游对**未知模型名**会静默回退到某个默认模型并正常
		// 返回 HTTP 200。于是用户把模型名打错一个字，拿到的是**另一个模型**的回答，
		// 而且照样消耗额度 —— 这种静默错配比一个明确的 400 糟糕得多。
		//
		// 何时该关掉：上游新增了模型、而客户端还没刷新模型目录（本地目录陈旧）时。
		// 关掉后未知模型会透传（并记 WARN）。
		StrictModels *bool `json:"strict_models"`
		// VisionEnabled 是否允许图片输入（**默认 false**）。
		//
		// 为什么默认关闭：实测上游的 chat 接口**不接受内联 base64 图片** ——
		// 11 种出站片段形态全部被业务码 4001 拒绝，只有纯文本能通
		//（见 internal/traectl/probe_image_test.go，需 TRAE_LIVE_IMAGE=1）。
		// 官方客户端的做法是先把图片上传到对象存储、再以 URI 引用，本网关未实现。
		//
		// 关闭时收到图片会**明确报错**（400 vision_unavailable + 原因），
		// 而不是转上去让上游回一个笼统的 4001 —— 后者会让用户完全看不出
		// 问题出在图片上。
		VisionEnabled bool `json:"vision_enabled"`
		// Proxy 全局出站代理（可选，支持 http/https/socks5，例如 "socks5://127.0.0.1:1080"）。
		// 账号未单独配置 proxy 时默认使用此代理；留空则按环境变量系统代理。
		Proxy string `json:"proxy"`
	} `json:"trae"`

	Schedule struct {
		// CheckinEnabled 每日自动签到（默认 true）。
		CheckinEnabled bool `json:"checkin_enabled"`
		// CheckinHours 签到执行的小时列表（24 小时制，默认 [9, 21]）。
		CheckinHours []int `json:"checkin_hours"`
	} `json:"schedule"`

	SessionSticky struct {
		// Enabled 会话粘性（同一多轮对话固定绑同一账号，最大化上游 prompt cache 命中）。
		Enabled bool `json:"enabled"`
		// TTL 绑定有效期，默认 "30m"。
		TTL string `json:"ttl"`
	} `json:"session_sticky"`

	Metrics struct {
		// RetainDays byDay 分桶的保留天数，默认 14。
		//
		// 为什么要保留窗口：workbuddy2api 的 byDay 是无窗口的 map，长期运行会无限增长
		// （它自己的注释也标注了"每次全量序列化，会变慢"）。本项目默认 14 天，
		// 并在落盘与恢复时双向惰性剔除超期分桶。
		RetainDays int `json:"retain_days"`
		// Enabled 是否采集指标，默认 true。
		Enabled bool `json:"enabled"`
	} `json:"metrics"`

	Prompt struct {
		// Mode 改写模式：passthrough（默认，不动客户端）/ custom（替换）/ append（追加）。
		Mode string `json:"mode"`
		// Text custom/append 使用的提示词文本。
		Text string `json:"text"`
	} `json:"prompt"`

	Login struct {
		// LoginHost 授权页面主机，默认 https://www.trae.cn。
		LoginHost string `json:"login_host"`
		// APIHost ExchangeToken 主机，默认 https://api.trae.cn。
		APIHost string `json:"api_host"`
	} `json:"login"`

	Intl struct {
		// Enabled 是否启用国际版（trae.ai）。**默认 true**。
		//
		// 为什么默认开：国内版与国际版是**两套独立的账号体系**，用户可能只装了
		// 其中一个、也可能两个都装。默认全开才能"装了哪个就自动认哪个" ——
		// 网关会去 `%APPDATA%\Trae` 与 `%APPDATA%\TRAE SOLO` 找国际版凭据，
		// 找不到就记一条 INFO（「未检测到客户端」）跳过，不影响国内版。
		//
		// 为什么默认开是安全的：注册了产品**不等于**建了渠道 —— 只有真的找到
		// 凭据时才会建渠道并进池。所以开着的代价仅仅是"多尝两次目录"，
		// 而不开则让境外用户白白多一步手工配置。
		//
		// 什么时候仍然需要一个版本：网络受限、只想要单一版本域时，
		// 显式设成 false 即可（显式配置优先于默认值）。
		Enabled bool `json:"enabled"`
		// UGHost 国际版 OAuth/权益主机，默认 `https://ug-normal.trae.ai`。
		//
		// 这个值是**实测出来的，不是猜的**。判据：真 Trae API 会返回 JSON 错误信封
		// （`{"ResponseMetadata":{"Error":{"Code":"10101",...}}}`），
		// 而非 API 返回 nginx/Tengine 的 404 HTML。
		// 同族的 `ug-normal.us.trae.ai` 也通过同一判据。
		//
		// 来源：国际站 www.trae.ai 的 main.js 里的主机清单
		//（`getAccountUrl()` 返回的 loginHost/consoleHost/apiHost 一族）。
		//
		// ⚠️ `api.trae.ai` **不是** API 主机（它对 Trae 的 API 路径返回 404 HTML）。
		// 当初差点被"域名看着像"骗过去 —— 所以这里记录判据，而不只记结论。
		UGHost string `json:"ug_host"`
		// LoginHost 国际版的**登录页主机**，默认 `https://www.trae.ai`。
		//
		// ⚠️ 为什么它是独立配置项，而不是从 UGHost 派生：
		// 国内版能用 `api.trae.cn` → `www.trae.cn`（把 `api.` 换成 `www.`）这条规则，
		// 但**它不能推广**。国际版的 UG 主机是 `ug-normal.trae.ai`，
		// 机械套用会得到 `www.ug-normal.trae.ai` —— 一个**不存在**的主机，
		// 表现是"登录页连不上"，而且看起来像是网络问题（实测踩过）。
		//
		// 正确值是 `www.trae.ai`（实测 HTTP 200）。这种事没有可靠的通用规则，
		// 所以显式写出来。
		LoginHost string `json:"login_host"`
		// ChatHost 国际版 chat 上游主机，默认 `https://core-normal.trae.ai`。
		//
		// 判据：它对 `/api/agent/v3/llm_utils_chat` 的响应与 CN 的
		// `trae-api-cn.mchost.guru` **逐字一致**
		//（都是 `HTTP 401 {"code":1001,"message":"We're sorry, but we are not able
		// to authenticate you..."}`），即同一套服务的不同区域入口。
		ChatHost string `json:"chat_host"`
		// TraeClientID / SoloClientID 国际版的 OAuth ClientID。
		//
		// 实测**与国内版共用同一对**：把它们发给国际版 UG 得到的是字段级错误
		//（`Invalid param`），而换一个不存在的 ID 会立刻变成 `Invalid client.`
		// —— 说明这两个 ID 在国际版侧是被承认的。
		//
		// 留空则用内置默认（即国内版那一对）。
		TraeClientID string `json:"trae_client_id"`
		SoloClientID string `json:"solo_client_id"`
	} `json:"intl"`

	Files struct {
		// Enabled 是否允许展开客户端消息里的文件引用，默认 **false**。
		//
		// ⚠️ 开启前请确认：这会让网关按请求内容读取本机文件。
		// 没有 AllowedRoots 时会被强制关闭（fail-closed）。
		Enabled bool `json:"enabled"`
		// AllowedRoots 允许读取的根目录（绝对路径）。空 = 功能关闭。
		AllowedRoots []string `json:"allowed_roots"`
		// MaxFileBytes 单文件上限，默认 1 MiB。
		MaxFileBytes int64 `json:"max_file_bytes"`
		// MaxTotalBytes 单次请求累计上限，默认 4 MiB。
		MaxTotalBytes int64 `json:"max_total_bytes"`
		// MaxFiles 单次请求文件数上限，默认 16。
		MaxFiles int `json:"max_files"`
	} `json:"files"`

	// ModelAliases 模型别名与重定向映射（例如 "gpt-4o": "trae:glm-5.3", "claude-3-7-sonnet": "trae:kimi-k3"）。
	ModelAliases map[string]string `json:"model_aliases"`

	Webhook struct {
		// Enabled 是否启用告警推送。
		Enabled bool `json:"enabled"`
		// URL 接收 Webhook 通知的地址（兼容飞书、企业微信、钉钉机器人或标准 HTTP POST）。
		URL string `json:"url"`
		// Secret 签名密钥（可选）。
		Secret string `json:"secret"`
	} `json:"webhook"`

	Update struct {
		// Enabled 是否启用自动更新检测（默认 true）。
		Enabled bool `json:"enabled"`
		// Repo GitHub 仓库名（形如 owner/repo，默认 "XCool-603/trae2api"）。
		Repo string `json:"repo"`
	} `json:"update"`
}

// Default 返回内置默认配置。
func Default() *Config {
	c := &Config{
		Listen:  "127.0.0.1:7870",
		DataDir: "./data",
	}
	c.Pool.MaxInFlight = 8
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.Pool.PickGap = "0s"
	c.Cooldown.SoftRate = "600s"
	c.Cooldown.SoftRateMax = "2h"
	c.Cooldown.ModelCooldown = "600s"
	c.Schedule.CheckinEnabled = true
	c.Schedule.CheckinHours = []int{9, 21}
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.Metrics.Enabled = true
	c.Metrics.RetainDays = 14
	// Prompt 默认 passthrough：不配置就完全不干预客户端的 system。
	// custom/append 必须显式开启 —— 网关悄悄改用户的 system 是越界行为。
	c.Prompt.Mode = "passthrough"
	c.Login.LoginHost = "https://www.trae.cn"
	c.Login.APIHost = "https://api.trae.cn"
	// 流中空闲 120s：实测成功请求的 TTFB 最坏到过 16s，静默 2 分钟基本可断定
	// 上游卡住了。这个值**不影响长回复** —— 只要还在吐数据就一直续命。
	c.Trae.IdleTimeoutSeconds = 120
	// 默认严格：静默回退到别的模型比明确的 400 糟糕得多
	c.Trae.StrictModels = boolPtr(true)

	// 国际版端点：**实测得到的内置默认值**（判据见 Intl 结构体的注释）。
	//
	// 之前这四个值是空的、要求用户自己填 —— 因为当时够不到国际站，只能"不猜"。
	// 现在从国际站的 main.js 拿到了主机清单，并用"真 API 返回 JSON 错误信封 vs
	// 非 API 返回 404 HTML"这个判据逐个验过，所以可以给默认值了。
	//
	// 仍留可覆盖：万一上游换域名，用户改配置即可，不必等新版本。
	c.Intl.UGHost = "https://ug-normal.trae.ai"
	c.Intl.LoginHost = "https://www.trae.ai"
	c.Intl.ChatHost = "https://core-normal.trae.ai"
	// 国际版与国内版共用同一对 ClientID（差分实测：有效 ID 得到字段级错误
	// `Invalid param`，假 ID 立刻变成 `Invalid client.`）。
	c.Intl.TraeClientID = "ono9krqynydwx5"
	c.Intl.SoloClientID = "en1oxy7wnw8j9n"
	// **默认开启**：国内版与国际版是两套账号体系，用户可能只装了其中一个、
	// 也可能两个都装。默认全开才能"装了哪个就自动认哪个"，不用手工切换。
	//
	// 为什么默认开是安全的：国际版产品注册后，只有在**真的找到国际版凭据**时
	// 才会建渠道；找不到就记一条 WARN 跳过（BuildChannels 的既有行为）。
	// 也就是说开着的代价仅仅是"多尝一次目录"，而不开则让境外用户多一步配置。
	c.Intl.Enabled = true
	c.Update.Enabled = true
	c.Update.Repo = "XCool-603/trae2api"
	return c
}

// Load 读取配置文件；文件不存在时写入默认模板并返回默认配置。
//
// 解析策略：先用默认值填充，再用文件内容覆盖 —— 因此旧配置文件缺少新字段时
// 自动获得默认值，不需要用户手动补全（前向兼容）。
func Load(path string) (*Config, error) {
	cfg := Default()
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("config: 读取 %s: %w", path, err)
		}
		// 首次运行：落一份模板，方便用户改
		if werr := writeTemplate(path, cfg); werr != nil {
			// 写模板失败不该阻止启动（可能只读目录）
			fmt.Fprintf(os.Stderr, "WARN: 写入默认配置模板失败: %v\n", werr)
		}
		return cfg, nil
	}
	if err := json.Unmarshal(stripBOM(raw), cfg); err != nil {
		return nil, fmt.Errorf("config: 解析 %s: %w", path, err)
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// MinWebPasswordLen 是控制台密码最短长度（与 server 侧保持一致）。
//
// 放在 config 里而不是 import server：config 不该依赖 server。
// 两处都定义是刻意的 —— 校验发生在配置加载时，反馈更早；server 侧那份用于
// 运行期（将来的改密端点）。
const MinWebPasswordLen = 6

// ValidateWebPassword 校验控制台密码。空串合法（= 免密）。
func ValidateWebPassword(pw string) error {
	pw = strings.TrimSpace(pw)
	if pw == "" {
		return nil
	}
	// 按**字符数**而不是字节数：中文密码按字节算会莫名其妙地"够长"
	if len([]rune(pw)) < MinWebPasswordLen {
		return fmt.Errorf("web_password 至少需要 %d 位（当前 %d 位）；留空则恢复单机免密",
			MinWebPasswordLen, len([]rune(pw)))
	}
	return nil
}

// boolPtr 便于给"默认 true"的开关留出"未设置"与"显式 false"的区分。
func boolPtr(b bool) *bool { return &b }

// StrictModelsOn 返回是否启用严格模型名校验（未设置时默认 true）。
func (c *Config) StrictModelsOn() bool {
	return c.Trae.StrictModels == nil || *c.Trae.StrictModels
}

// stripBOM 去掉 UTF-8 BOM。
//
// 为什么必须容忍它：Windows 上用记事本编辑 config.json 会自动写入 BOM，
// PowerShell 的 `Set-Content -Encoding UTF8` 也会（5.1 及更早版本必加）。
// 不处理的话 Go 的 json 会报 `invalid character '\ufeff' looking for beginning of value`
// —— 这个报错完全看不出"是编辑器加的 BOM"，用户会去检查 JSON 语法然后一无所获。
// 一行 TrimPrefix 换掉一整类求助。
func stripBOM(b []byte) []byte {
	return bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
}

// normalize 校验并补齐派生字段。
func (c *Config) normalize() error {
	if strings.TrimSpace(c.Listen) == "" {
		c.Listen = "127.0.0.1:7870"
	}
	if strings.TrimSpace(c.DataDir) == "" {
		c.DataDir = "./data"
	}
	if c.Pool.MaxInFlight <= 0 {
		c.Pool.MaxInFlight = 3
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	if _, err := c.BreakerCooldownDur(); err != nil {
		return err
	}
	if _, err := c.SoftRateDur(); err != nil {
		return err
	}
	if _, err := c.SoftRateMaxDur(); err != nil {
		return err
	}
	if _, err := c.ModelCooldownDur(); err != nil {
		return err
	}
	if _, err := c.PickGapDur(); err != nil {
		return err
	}
	if _, err := c.StickyTTL(); err != nil {
		return err
	}
	// 代理 URL 校验（全局与账号级）
	if err := validateProxyURL(c.Trae.Proxy); err != nil {
		return fmt.Errorf("config: trae.proxy 非法: %w", err)
	}
	// 账号合法性：产品 key 必须是已知值
	for i := range c.Trae.Accounts {
		a := &c.Trae.Accounts[i]
		a.Product = strings.TrimSpace(a.Product)
		switch a.Product {
		case "trae-cn", "solo-cn":
		case "":
			return fmt.Errorf("config: trae.accounts[%d] 缺少 product（可选 trae-cn / solo-cn）", i)
		default:
			return fmt.Errorf("config: trae.accounts[%d] 未知 product %q（可选 trae-cn / solo-cn）", i, a.Product)
		}
		if a.Owner == "" {
			a.Owner = "public"
		}
		if err := validateProxyURL(a.Proxy); err != nil {
			return fmt.Errorf("config: trae.accounts[%d].proxy 非法: %w", i, err)
		}
	}
	// 控制台密码长度：太短的密码等于没有保护，且给了用户"我设了密码"的错觉
	if err := ValidateWebPassword(c.WebPassword); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	// Prompt 模式必须合法：非法值报错而不是静默回落
	//（静默回落会让用户以为配置生效了，实际没改）
	if _, err := prompt.ParseMode(c.Prompt.Mode); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if strings.TrimSpace(c.Update.Repo) == "" {
		c.Update.Repo = "XCool-603/trae2api"
	}
	return nil
}

// validateProxyURL 校验代理 URL 格式与协议（支持 http/https/socks5）。
func validateProxyURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL 解析失败: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("不支持的代理协议 %q (仅支持 http / https / socks5)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("代理 URL 缺少主机地址")
	}
	return nil
}

// 时长字段解析（空值回落默认，非法值报错 —— 与 workbuddy2api 同风格：
// 静默忽略非法配置会让用户以为生效了）。
func (c *Config) dur(v, def, name string) (time.Duration, error) {
	s := strings.TrimSpace(v)
	if s == "" {
		return time.ParseDuration(def)
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q 不是合法时长: %w", name, v, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("config: %s=%q 必须为正", name, v)
	}
	return d, nil
}

// BreakerCooldownDur 熔断冷却时长。
func (c *Config) BreakerCooldownDur() (time.Duration, error) {
	return c.dur(c.Pool.BreakerCooldown, "30m", "pool.breaker_cooldown")
}

// SoftRateDur 限流软冷却基数。
func (c *Config) SoftRateDur() (time.Duration, error) {
	return c.dur(c.Cooldown.SoftRate, "600s", "cooldown.soft_rate")
}

// SoftRateMaxDur 软冷却指数退避封顶。
func (c *Config) SoftRateMaxDur() (time.Duration, error) {
	return c.dur(c.Cooldown.SoftRateMax, "2h", "cooldown.soft_rate_max")
}

// ModelCooldownDur 模型级冷却基数。
func (c *Config) ModelCooldownDur() (time.Duration, error) {
	return c.dur(c.Cooldown.ModelCooldown, "600s", "cooldown.model_cooldown")
}

// PickGapDur 选号最小间隔（防惊群）。0 或空表示不设间隔（降低延迟）。
func (c *Config) PickGapDur() (time.Duration, error) {
	v := strings.TrimSpace(c.Pool.PickGap)
	if v == "" || v == "0" || v == "0s" || v == "0ms" {
		return 0, nil
	}
	return c.dur(c.Pool.PickGap, "0s", "pool.pick_gap")
}

// StickyTTL 会话粘性有效期。
func (c *Config) StickyTTL() (time.Duration, error) {
	return c.dur(c.SessionSticky.TTL, "30m", "session_sticky.ttl")
}

// StateFile 状态快照路径。
func (c *Config) StateFile() string {
	return filepath.Join(c.DataDir, "state.json")
}

// UsersFile 用户与 API Key 存储路径（多租户 RBAC）。
//
// 与 state.json 分开：用户文件含长期有效的 API Key，属于凭证类数据，
// 混在运行时状态里容易被"清空状态"一类操作误删。
func (c *Config) UsersFile() string {
	return filepath.Join(c.DataDir, "users.json")
}

// AccountsDir 便携凭据快照目录（导入/导出的落盘位置）。
//
// 单独一个目录：快照里含 refreshToken 与设备私钥，属于最高敏感度的凭证，
// 目录权限设为 0700（见 accountstore），且与运行时状态分开便于单独备份。
func (c *Config) AccountsDir() string {
	return filepath.Join(c.DataDir, "accounts")
}

// MetricsFile 指标快照路径。
//
// 与 state.json 分开：指标是**可丢的观测数据**，用户想重置统计时应当能单独删它，
// 而不影响账号池的冷却/熔断状态。
func (c *Config) MetricsFile() string {
	return filepath.Join(c.DataDir, "metrics.json")
}

// writeTemplate 写入带注释说明的默认配置模板。
func writeTemplate(path string, cfg *Config) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}
