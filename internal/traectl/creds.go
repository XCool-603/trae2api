package traectl

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// 凭据在 storage.json 中的键名（Trae CN 与 TRAE SOLO CN 完全一致）。
const (
	keyAuth       = "iCubeAuthInfo://icube.cloudide"
	keyDevicePfx  = "iCubeAuthInfo://icube-dc:" // 后接数字设备 ID
	keyMachineID  = "telemetry.machineId"
	keyServerData = "iCubeServerData://icube.cloudide"
)

// Product 描述一个 Trae 产品的渠道常量。
//
// 全部取值来自本机客户端逆向 + 实测（见 Trae2Api-阶段一验证报告.md）：
//   - ClientID 由 product.json 的 authConfig.{TRAE,SOLO}.stable 决定，**不是**由 CN/国际决定；
//   - PlatformCode 由客户端源码 `gr(t) ? "SOLO_PC" : "IDE_PC"` 决定；
//   - Function 是 chat 请求体的 function 字段，决定使用哪个模型池。
type Product struct {
	// Key 稳定标识（配置与日志用）：trae-cn / solo-cn。
	Key string
	// Label 人类可读名（日志与 UI 用）。
	Label string
	// DataDir %APPDATA% 下的目录名。
	DataDir string
	// IsSOLO 决定 ClientID 与 PlatformCode 的选取。
	IsSOLO bool
	// ClientID 续期用的 OAuth ClientID（authConfig.*.stable）。
	ClientID string
	// PlatformCode 续期 DeviceInfo 中的平台码：IDE_PC / SOLO_PC。
	PlatformCode string
	// Edition 版本域：cn（国内，*.trae.cn）/ intl（国际，*.trae.ai）。
	//
	// 为什么它是一等公民而不是"多一个产品"：国内版与国际版是**两套独立的
	// 账号体系与端点**，凭据不能互换。混在一个域里会让选号把国内账号派给
	// 国际请求（或反过来），表现为"账号明明健康却总是失败"。
	Edition Edition
	// AuthFrom OAuth 登录 URL 里的 auth_from 参数：trae / solo。
	// 取自客户端 loginUrlBuilder：`Su(app)===SOLO_Lite ? "solo" : "trae"`。
	AuthFrom string
	// Function chat 请求体的 function 字段：chat_v3 / solo_work_lite。
	Function string
	// ModelMapKeyPrefix 模型目录在 state.vscdb 中的键前缀形式。
	// 两种形式都出现过（下划线 / 冒号），读取时都要尝试。
	ModelMapKeys func(uid string) []string
}

// Products 是本项目支持的两个 Trae 产品。
//
// 注意：两者**共享同一账号与同一额度池**（实测 ide_user_ent_usage 返回完全相同的数值），
// 所以它们不是「两份额度」，而是同一份额度上的两个入口（模型池与排队体验不同）。
var Products = []Product{
	{
		Key:          "trae-cn",
		Label:        "Trae CN",
		DataDir:      "Trae CN",
		IsSOLO:       false,
		Edition:      EditionCN,
		ClientID:     "ono9krqynydwx5",
		PlatformCode: "IDE_PC",
		AuthFrom:     "trae",
		Function:     "chat_v3",
		ModelMapKeys: func(uid string) []string {
			return []string{uid + "_AI.agent.model.model_list_map", uid + ":AI.agent.model.model_list_map"}
		},
	},
	{
		Key:          "solo-cn",
		Label:        "TRAE SOLO CN",
		DataDir:      "TRAE SOLO CN",
		IsSOLO:       true,
		Edition:      EditionCN,
		ClientID:     "en1oxy7wnw8j9n",
		PlatformCode: "SOLO_PC",
		AuthFrom:     "solo",
		Function:     "solo_work_lite",
		ModelMapKeys: func(uid string) []string {
			// SOLO 的专属池（solo_work_lite 等）在**带冒号**的键里，
			// 下划线键只有 builder/chat_v3 一类。两个都试，命中即用。
			return []string{uid + ":AI.agent.model.model_list_map", uid + "_AI.agent.model.model_list_map"}
		},
	},
}

// Edition 是产品的版本域。
type Edition string

const (
	// EditionCN 国内版（trae.cn / trae.com.cn）。
	EditionCN Edition = "cn"
	// EditionIntl 国际版（trae.ai）。
	//
	// ⚠️ 国际版的**实际端点无法从国内客户端推导**（实测：api.trae.ai 对
	// Trae 的 API 路径返回 nginx 404，而真 API 会返回 JSON 错误信封）。
	// 所以国际版的 host 必须由用户在配置里提供（见 config.intl），
	// 本包只负责把它接上，不猜默认值。
	EditionIntl Edition = "intl"
)

// IsIntlHost 判定主机是否属于国际版域。
//
// 对齐参考实现的做法（它按 domain 后缀判 realm，而不是查硬编码主机表）：
// 从**凭据自带的 host** 推断版本域，这样导入一份国际版凭据时能自动识别，
// 不需要用户额外声明。
func IsIntlHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimPrefix(h, "https://")
	h = strings.TrimPrefix(h, "http://")
	if i := strings.IndexByte(h, '/'); i >= 0 {
		h = h[:i]
	}
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return h == "trae.ai" || strings.HasSuffix(h, ".trae.ai")
}

// IntlConfig 是国际版的运行时配置。
//
// ⚠️ 为什么每一项都要用户提供：国际版的端点与 ClientID **无法从国内客户端推导**
// （实测：api.trae.ai 对 Trae 的 API 路径返回 nginx 404，而真 API 会返回
// JSON 错误信封）。本项目**不猜**这些值 —— 猜错的表现是"账号明明健康却总是失败"，
// 而且报错完全指不出根因。
//
// 用户可以从国际版客户端的 resources/app/product.json 里读到：
//
//	bootConfig.ug.trae.normal      → UGHost
//	bootConfig.agent.trae.normal   → ChatHost
//	iCubeApp.authConfig.TRAE.stable → TraeClientID
//	iCubeApp.authConfig.SOLO.stable → SoloClientID
type IntlConfig struct {
	Enabled      bool
	UGHost       string
	ChatHost     string
	TraeClientID string
	SoloClientID string
}

// ConfigureIntl 依据配置注册国际版产品（幂等：重复调用会替换上一次的）。
//
// 未启用或缺少必需项时**什么都不注册** —— 这样上层会明确看到"没有 intl 渠道"，
// 而不是拿到一个指向错误主机的渠道（那会导致每个请求都失败且难以定位）。
func ConfigureIntl(c IntlConfig) error {
	// 先移除上一次注册的（幂等）
	base := Products[:0:0]
	for _, p := range Products {
		if p.Edition != EditionIntl {
			base = append(base, p)
		}
	}
	Products = base

	if !c.Enabled {
		return nil
	}
	var missing []string
	if strings.TrimSpace(c.UGHost) == "" {
		missing = append(missing, "intl.ug_host")
	}
	if strings.TrimSpace(c.ChatHost) == "" {
		missing = append(missing, "intl.chat_host")
	}
	if strings.TrimSpace(c.TraeClientID) == "" {
		missing = append(missing, "intl.trae_client_id")
	}
	if strings.TrimSpace(c.SoloClientID) == "" {
		missing = append(missing, "intl.solo_client_id")
	}
	if len(missing) > 0 {
		return fmt.Errorf("traectl: 已启用国际版但缺少配置项 %s。"+
			"这些值可从国际版客户端的 resources/app/product.json 读取："+
			"bootConfig.ug.trae.normal / bootConfig.agent.trae.normal / "+
			"iCubeApp.authConfig.{TRAE,SOLO}.stable", strings.Join(missing, "、"))
	}

	trae := Product{
		Key: "trae-intl", Label: "Trae International", DataDir: "Trae",
		IsSOLO: false, Edition: EditionIntl,
		ClientID: c.TraeClientID, PlatformCode: "IDE_PC", AuthFrom: "trae",
		Function: "chat_v3",
		ModelMapKeys: func(uid string) []string {
			return []string{uid + "_AI.agent.model.model_list_map", uid + ":AI.agent.model.model_list_map"}
		},
	}
	solo := Product{
		Key: "solo-intl", Label: "TRAE SOLO International", DataDir: "TRAE SOLO",
		IsSOLO: true, Edition: EditionIntl,
		ClientID: c.SoloClientID, PlatformCode: "SOLO_PC", AuthFrom: "solo",
		Function: "solo_work_lite",
		ModelMapKeys: func(uid string) []string {
			return []string{uid + ":AI.agent.model.model_list_map", uid + "_AI.agent.model.model_list_map"}
		},
	}
	Products = append(Products, trae, solo)
	return nil
}

// ProductByKeyOrZero 按 Key 查找产品；找不到时返回一个**空 Product**。
//
// 只给"找不到就该用零值兜底"的场景用（如测试里取 trae-cn）。
// 生产代码应当用 ProductByKey 并处理 not-found —— 静默用零值会让
// 版本域、ClientID 全部为空，请求必然失败且难以定位。
func ProductByKeyOrZero(key string) Product {
	p, _ := ProductByKey(key)
	return p
}

// ProductByKey 按 Key 查找产品。
func ProductByKey(key string) (Product, bool) {
	for _, p := range Products {
		if p.Key == key {
			return p, true
		}
	}
	return Product{}, false
}

// Credentials 是 iCubeAuthInfo://icube.cloudide 解密后的结构（只列本渠道用到的字段，
// 其余字段通过 Store.extra 原样保留并写回）。
type Credentials struct {
	Token            string `json:"token"`
	RefreshToken     string `json:"refreshToken"`
	ExpiredAt        string `json:"expiredAt"`
	RefreshExpiredAt string `json:"refreshExpiredAt"`
	TokenReleaseAt   string `json:"tokenReleaseAt"`
	UserID           string `json:"userId"`
	Host             string `json:"host"`
}

// DeviceKeyPair 是 iCubeAuthInfo://icube-dc:<did> 解密后的结构（EC P-256 密钥对）。
type DeviceKeyPair struct {
	PrivateKeyPEM string `json:"privateKeyPEM"`
	PublicKeyPEM  string `json:"publicKeyPEM"`
}

// StoreMode 是凭据文件的形态。
//
// 同一个 Store 支持两种文件，**按内容自动识别**（不需要调用方声明）：
//   - ModeStorageJSON：Trae 桌面客户端的 storage.json（凭据藏在
//     iCubeAuthInfo://icube.cloudide 键里，外面还包着一堆窗口/遥测状态）
//   - ModeSnapshot：本网关自己的便携快照（顶层直接是 token/refreshToken/密钥对），
//     用于导入导出与「网关机器上不装 Trae 客户端」的部署方式
type StoreMode int

const (
	// ModeStorageJSON 客户端 storage.json。
	ModeStorageJSON StoreMode = iota
	// ModeSnapshot 便携快照（可直接拷贝到另一台机器使用）。
	ModeSnapshot
)

func (m StoreMode) String() string {
	if m == ModeSnapshot {
		return "snapshot"
	}
	return "storage.json"
}

// Store 是某个 Trae 账号的凭据存储句柄。
//
// 并发安全：mu 串行化 Load/Save，防止并发续期把文件写坏。
// 对 storage.json 模式**不做跨进程锁** —— Trae 桌面客户端可能同时在写同一个文件。
// 为把冲突面压到最小，Save 采用「重新读取最新文件 → 只替换本渠道拥有的键 → 原子写回」，
// 而不是把内存里的整份快照写回去。
type Store struct {
	Product Product

	mu   sync.Mutex
	path string
	mode StoreMode

	// 以下为 Load 的结果（mu 保护）
	Cred      Credentials
	Device    DeviceKeyPair
	DeviceID  string // icube-dc 的数字后缀，续期 DeviceInfo.DeviceID 用它
	MachineID string // telemetry.machineId
	envType   EnvelopeType

	// extraAuth 保存主凭据 JSON 中本包未建模的字段，写回时原样带上，
	// 避免把上游新增字段（如 account / userRegion）丢掉。
	extraAuth map[string]json.RawMessage

	// label 仅快照模式使用（多账号时便于区分）。
	label string

	// proxy 出站代理地址（http/https/socks5；空 = 走全局/直连）。
	proxy string
}

// Mode 返回凭据文件形态（Load 之后有效）。
func (s *Store) Mode() StoreMode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

// Label 返回快照里的标签（storage.json 模式恒为空）。
func (s *Store) Label() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.label
}

// Proxy 返回快照或配置绑定的代理地址。
func (s *Store) Proxy() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proxy
}

// SetProxy 动态设置该凭据存储绑定的代理地址。
func (s *Store) SetProxy(proxyURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proxy = strings.TrimSpace(proxyURL)
}

// AppDataDir 返回本机 %APPDATA% 路径。
func AppDataDir() (string, error) {
	d := os.Getenv("APPDATA")
	if d == "" {
		return "", fmt.Errorf("traectl: 环境变量 APPDATA 为空（非 Windows 或受限环境）")
	}
	return d, nil
}

// DefaultStoragePath 返回某产品的 storage.json 默认路径。
func DefaultStoragePath(p Product) (string, error) {
	appData, err := AppDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(appData, p.DataDir, "User", "globalStorage", "storage.json"), nil
}

// DefaultStateDBPath 返回某产品的 state.vscdb（模型目录）默认路径。
//
// 注意：容器/自定义部署下 APPDATA 往往不存在，应改用 StateDBPathBeside
// 从实际凭据路径推导（两者永远是同一目录下的兄弟文件）。
func DefaultStateDBPath(p Product) (string, error) {
	appData, err := AppDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(appData, p.DataDir, "User", "globalStorage", "state.vscdb"), nil
}

// StateDBPathBeside 返回与给定 storage.json **同目录**的 state.vscdb 路径。
//
// 为什么需要它：两者永远是 `<User>/globalStorage/` 下的兄弟文件。
// 从凭据路径推导，可以让「把整个 globalStorage 目录挂进容器」这种部署方式
// 自然成立 —— 否则容器里没有 %APPDATA%，默认路径推导必然失败。
func StateDBPathBeside(storagePath string) string {
	return filepath.Join(filepath.Dir(storagePath), "state.vscdb")
}

// OpenStore 打开并解密某产品的凭据。
//
// path 为空时用默认路径。找不到文件或未登录（缺 iCubeAuthInfo://icube.cloudide）
// 都返回错误 —— 渠道初始化时应据此跳过该产品而不是让整个网关起不来。
func OpenStore(p Product, path string) (*Store, error) {
	if path == "" {
		var err error
		path, err = DefaultStoragePath(p)
		if err != nil {
			return nil, err
		}
	}
	s := &Store{Product: p, path: path}
	if err := s.Load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path 返回底层 storage.json 路径。
func (s *Store) Path() string { return s.path }

// Load 读取并解密凭据文件，刷新内存中的凭据。
//
// 按文件**内容**自动识别形态（storage.json / 便携快照），调用方无需声明。
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("traectl: 读取 %s: %w", s.path, err)
	}
	var st map[string]json.RawMessage
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("traectl: %s 不是合法 JSON: %w", s.path, err)
	}
	// 形态识别：有 iCubeAuthInfo://icube.cloudide 键 → 客户端 storage.json；
	// 否则若顶层就有 token/refreshToken → 便携快照。
	if _, isStorage := st[keyAuth]; isStorage {
		s.mode = ModeStorageJSON
		return s.loadStorageJSONLocked(st)
	}
	s.mode = ModeSnapshot
	return s.loadSnapshotLocked(st)
}

// loadSnapshotLocked 解析便携快照（顶层直接是凭据字段 + 设备密钥对）。
func (s *Store) loadSnapshotLocked(st map[string]json.RawMessage) error {
	plain, _ := json.Marshal(st)
	var snap CredentialSnapshot
	if err := json.Unmarshal(plain, &snap); err != nil {
		return fmt.Errorf("traectl: 解析快照 %s: %w", s.path, err)
	}
	if snap.Token == "" {
		return fmt.Errorf("traectl: %s 既不是 storage.json（缺 %s）也不是快照（缺 token）",
			s.path, keyAuth)
	}
	host := snap.Host
	if strings.Contains(strings.ToLower(host), "api.trae.com.cn") {
		host = DefaultUGHost
	}
	s.Cred = Credentials{
		Token:            snap.Token,
		RefreshToken:     snap.RefreshToken,
		ExpiredAt:        snap.ExpiredAt,
		RefreshExpiredAt: snap.RefreshExpiredAt,
		TokenReleaseAt:   snap.TokenReleaseAt,
		UserID:           snap.UserID,
		Host:             host,
	}
	s.Device = DeviceKeyPair{PrivateKeyPEM: snap.PrivateKeyPEM, PublicKeyPEM: snap.PublicKeyPEM}
	s.DeviceID = snap.DeviceID
	s.MachineID = snap.MachineID
	s.label = snap.Label
	s.proxy = snap.Proxy
	// 快照固定用 AES 信封（本网关自己的导出格式）
	s.envType = EnvAES
	s.extraAuth = nil
	return nil
}

// loadStorageJSONLocked 解析客户端 storage.json（原有路径）。
func (s *Store) loadStorageJSONLocked(st map[string]json.RawMessage) error {
	// ---- 主凭据 ----
	var authRaw string
	if v, ok := st[keyAuth]; ok {
		if err := json.Unmarshal(v, &authRaw); err != nil {
			return fmt.Errorf("traectl: %s 字段类型异常: %w", keyAuth, err)
		}
	}
	if authRaw == "" {
		return fmt.Errorf("traectl: %s 缺少 %s（未登录该产品？）", s.path, keyAuth)
	}

	plain, envType, err := decodePossiblyPlain(authRaw)
	if err != nil {
		return err
	}
	s.envType = envType

	var all map[string]json.RawMessage
	if err := json.Unmarshal(plain, &all); err != nil {
		return fmt.Errorf("traectl: 主凭据明文不是合法 JSON: %w", err)
	}
	// 建模字段
	var cred Credentials
	if err := json.Unmarshal(plain, &cred); err != nil {
		return fmt.Errorf("traectl: 解析凭据字段: %w", err)
	}
	// 其余字段原样保留
	delete(all, "token")
	delete(all, "refreshToken")
	delete(all, "expiredAt")
	delete(all, "refreshExpiredAt")
	delete(all, "tokenReleaseAt")
	delete(all, "userId")
	delete(all, "host")
	s.extraAuth = all
	s.Cred = cred

	// ---- 设备密钥对 ----
	s.Device = DeviceKeyPair{}
	s.DeviceID = ""
	for k, v := range st {
		if !strings.HasPrefix(k, keyDevicePfx) {
			continue
		}
		s.DeviceID = strings.TrimPrefix(k, keyDevicePfx)
		var devRaw string
		if err := json.Unmarshal(v, &devRaw); err != nil {
			continue
		}
		devPlain, _, err := decodePossiblyPlain(devRaw)
		if err != nil {
			// 设备密钥解不开不应让整个渠道失败（续期时才会用到）
			continue
		}
		var kp DeviceKeyPair
		if err := json.Unmarshal(devPlain, &kp); err == nil {
			s.Device = kp
		}
	}

	// ---- machineId ----
	s.MachineID = ""
	if v, ok := st[keyMachineID]; ok {
		var mid string
		if err := json.Unmarshal(v, &mid); err == nil {
			s.MachineID = mid
		}
	}
	return nil
}

// decodePossiblyPlain 解开 tc 信封；若是明文 JSON（Trae 国际版）则原样返回。
func decodePossiblyPlain(v string) ([]byte, EnvelopeType, error) {
	trimmed := strings.TrimSpace(v)
	if strings.HasPrefix(trimmed, "{") {
		return []byte(trimmed), EnvAES, nil
	}
	plain, err := DecryptTC(trimmed)
	if err != nil {
		return nil, 0, err
	}
	// 明文回探信封类型（写回时保持一致）
	raw, derr := base64.StdEncoding.DecodeString(trimmed)
	et := EnvAES
	if derr == nil && len(raw) >= headerLen {
		if t, e := detectEnvelope(raw[:headerLen]); e == nil {
			et = t
		}
	}
	return plain, et, nil
}

// Save 把内存中的凭据写回原文件（自动按形态走对应分支）。
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.mode == ModeSnapshot {
		return s.saveSnapshotLocked()
	}
	return s.saveStorageJSONLocked()
}

// saveSnapshotLocked 覆盖写便携快照（快照文件完全由本网关拥有，无需 merge）。
func (s *Store) saveSnapshotLocked() error {
	snap := CredentialSnapshot{
		Version:          snapshotVersion,
		Product:          s.Product.Key,
		Label:            s.label,
		Proxy:            s.proxy,
		UserID:           s.Cred.UserID,
		Token:            s.Cred.Token,
		RefreshToken:     s.Cred.RefreshToken,
		ExpiredAt:        s.Cred.ExpiredAt,
		RefreshExpiredAt: s.Cred.RefreshExpiredAt,
		TokenReleaseAt:   s.Cred.TokenReleaseAt,
		Host:             s.Cred.Host,
		DeviceID:         s.DeviceID,
		MachineID:        s.MachineID,
		PrivateKeyPEM:    s.Device.PrivateKeyPEM,
		PublicKeyPEM:     s.Device.PublicKeyPEM,
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("traectl: 序列化快照: %w", err)
	}
	return atomicWrite(s.path, append(b, '\n'))
}

// saveStorageJSONLocked 写回客户端 storage.json。
//
// 写回策略（重要）：**重新读取磁盘上的最新内容**，只替换本渠道拥有的两个键
// （iCubeAuthInfo://icube.cloudide 与设备密钥对不动），然后原子替换文件。
// 这样即使 Trae 客户端在期间写了别的键（窗口状态、遥测等），也不会被我们覆盖。
func (s *Store) saveStorageJSONLocked() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("traectl: 写回前读取 %s: %w", s.path, err)
	}
	var st map[string]json.RawMessage
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("traectl: 写回前解析 %s: %w", s.path, err)
	}

	// 重建主凭据 JSON：建模字段 + 保留的其余字段
	merged := make(map[string]json.RawMessage, len(s.extraAuth)+7)
	for k, v := range s.extraAuth {
		merged[k] = v
	}
	put := func(k string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		merged[k] = b
		return nil
	}
	if err := put("token", s.Cred.Token); err != nil {
		return err
	}
	if err := put("refreshToken", s.Cred.RefreshToken); err != nil {
		return err
	}
	if err := put("expiredAt", s.Cred.ExpiredAt); err != nil {
		return err
	}
	if err := put("refreshExpiredAt", s.Cred.RefreshExpiredAt); err != nil {
		return err
	}
	if err := put("tokenReleaseAt", s.Cred.TokenReleaseAt); err != nil {
		return err
	}
	if err := put("userId", s.Cred.UserID); err != nil {
		return err
	}
	if err := put("host", s.Cred.Host); err != nil {
		return err
	}
	plain, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("traectl: 序列化凭据: %w", err)
	}
	enc, err := EncryptTC(plain, s.envType)
	if err != nil {
		return err
	}
	encJSON, err := json.Marshal(enc)
	if err != nil {
		return err
	}
	st[keyAuth] = encJSON

	out, err := json.MarshalIndent(st, "", "\t")
	if err != nil {
		return fmt.Errorf("traectl: 序列化 storage.json: %w", err)
	}
	return atomicWrite(s.path, out)
}

// atomicWrite 同目录写临时文件后 rename，避免半写坏文件。
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".traectl-*.tmp")
	if err != nil {
		return fmt.Errorf("traectl: 创建临时文件: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// 成功路径已 rename，这里只清理失败残留
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("traectl: 写临时文件: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("traectl: fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("traectl: 关闭临时文件: %w", err)
	}
	// Windows 上 rename 到已存在文件会失败，先删目标（有极短窗口，但 storage.json
	// 的读方是客户端，最坏情况它会读到"文件不存在"并回退到内存态，不会读到半写内容）。
	if err := os.Rename(tmpName, path); err != nil {
		if rmErr := os.Remove(path); rmErr == nil {
			err = os.Rename(tmpName, path)
		}
		if err != nil {
			return fmt.Errorf("traectl: 替换 %s: %w", path, err)
		}
	}
	return nil
}
