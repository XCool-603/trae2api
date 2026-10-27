package traectl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// snapshotVersion 是便携快照的格式版本（便于将来演进时识别旧格式）。
const snapshotVersion = 1

// CredentialSnapshot 是**可移植的完整凭据**。
//
// 为什么需要它：Trae 的凭据不是简单的 token+refreshToken —— 续期要用
// **ECDSA P-256 设备私钥**签名（DeviceProof），而这个私钥存在
// `iCubeAuthInfo://icube-dc:<did>` 里，和 token 在一起。少了它，refreshToken
// 拿到也没用（换不出新 token）。
//
// 所以一份可移植凭据必须同时包含：token 三元组 + 设备密钥对 + 设备指纹。
// 导出成这个格式后，网关可以在**没有安装 Trae 客户端**的机器上运行。
type CredentialSnapshot struct {
	// Version 快照格式版本。
	Version int `json:"version"`
	// Product 产品 key：trae-cn / solo-cn。
	Product string `json:"product"`
	// Label 人类可读标签。
	Label string `json:"label,omitempty"`
	// Proxy 该账号专用的出站代理地址（http/https/socks5）。
	Proxy string `json:"proxy,omitempty"`
	// UserID Trae 账号 ID（数字字符串）。
	UserID string `json:"userId"`

	// Token 访问令牌（JWT）。
	Token string `json:"token"`
	// RefreshToken 刷新令牌（**轮换语义**：每次续期后都会变）。
	RefreshToken string `json:"refreshToken"`
	// ExpiredAt / RefreshExpiredAt ISO8601（毫秒精度 UTC）。
	ExpiredAt        string `json:"expiredAt"`
	RefreshExpiredAt string `json:"refreshExpiredAt"`
	// TokenReleaseAt 最近一次签发时间。
	TokenReleaseAt string `json:"tokenReleaseAt,omitempty"`
	// Host 该账号的上游主机（凭据自带，通常 https://api.trae.cn）。
	Host string `json:"host,omitempty"`

	// DeviceID 数字设备 ID（= iCubeAuthInfo://icube-dc:<此值> 的后缀）。
	// 续期 DeviceInfo.DeviceID 必须用它。
	DeviceID string `json:"deviceId"`
	// MachineID telemetry.machineId（64 位 hex）。续期 DeviceInfo.MachineID 用它。
	MachineID string `json:"machineId"`

	// PrivateKeyPEM 设备 EC P-256 私钥（PKCS#8 PEM），DeviceProof 签名用。
	PrivateKeyPEM string `json:"privateKeyPEM"`
	// PublicKeyPEM 对应公钥（SPKI PEM）。续期 DeviceInfo.DevicePublicKey 用它。
	PublicKeyPEM string `json:"publicKeyPEM"`
}

// ExportSnapshot 导出当前凭据为可移植快照。
func (s *Store) ExportSnapshot() *CredentialSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &CredentialSnapshot{
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
}

// Validate 检查快照是否**可用于续期与调用**（不只是"字段非空"）。
//
// 校验要点：没有设备私钥就无法续期 —— token 一过期这份凭据就废了。
// 所以这里把私钥与设备 ID 也列为必需，避免导入一份"看起来有 token 但用不了"的凭据。
func (c *CredentialSnapshot) Validate() error {
	switch {
	case c.Product == "":
		return fmt.Errorf("缺少 product（trae-cn / solo-cn）")
	case c.Token == "":
		return fmt.Errorf("缺少 token")
	case c.RefreshToken == "":
		return fmt.Errorf("缺少 refreshToken（没有它无法续期）")
	case c.PrivateKeyPEM == "":
		return fmt.Errorf("缺少 privateKeyPEM（没有它无法生成 DeviceProof，token 过期后无法续期）")
	case c.PublicKeyPEM == "":
		return fmt.Errorf("缺少 publicKeyPEM")
	case c.DeviceID == "":
		return fmt.Errorf("缺少 deviceId")
	case c.UserID == "":
		return fmt.Errorf("缺少 userId")
	}
	if _, err := parseECPrivateKey(c.PrivateKeyPEM); err != nil {
		return fmt.Errorf("privateKeyPEM 无效: %w", err)
	}
	return nil
}

// Normalize 补齐可推导字段（host / version / product 别名）。
func (c *CredentialSnapshot) Normalize() {
	if c.Version == 0 {
		c.Version = snapshotVersion
	}
	// 产品别名：兼容外部工具常见的写法
	switch strings.ToLower(strings.TrimSpace(c.Product)) {
	case "trae", "trae-cn", "traecn", "trae_cn", "ide":
		c.Product = "trae-cn"
	case "solo", "solo-cn", "solocn", "solo_cn", "traework", "trae-solo":
		c.Product = "solo-cn"
	}
	// 若存量账号 host 为空或仍指向已下线的 api.trae.com.cn，自动纠偏迁移至 api.trae.cn
	if strings.TrimSpace(c.Host) == "" || strings.Contains(strings.ToLower(c.Host), "api.trae.com.cn") {
		c.Host = DefaultUGHost
	}
	if c.Label == "" {
		if c.UserID != "" {
			c.Label = "Trae " + c.UserID
		} else {
			c.Label = c.Product
		}
	}
}

// WriteSnapshot 把凭据以快照形式写到指定路径（用于导出/导入落盘）。
func WriteSnapshot(path string, snap *CredentialSnapshot) error {
	snap.Normalize()
	if err := snap.Validate(); err != nil {
		return fmt.Errorf("traectl: 快照校验失败: %w", err)
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("traectl: 序列化快照: %w", err)
	}
	return atomicWrite(path, append(b, '\n'))
}

// ---- 导入清洗 ----

// cStyleComment 剥离 C 风格注释（// 行注释与 /* */ 块注释）。
// 注意不能用正则一次性处理字符串内的 //（如 URL），故按行处理并跳过行内已进入字符串的情形。
var (
	blockCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
	lineCommentRe  = regexp.MustCompile(`(?m)^\s*//.*$`)
	trailingComma  = regexp.MustCompile(`,\s*([}\]])`)
)

// CleanRawInput 清洗用户粘贴/导入的原始文本，容忍常见"非标准 JSON"写法。
//
// 处理项（对齐 workbuddy2api 的同类能力）：
//   - UTF-8 BOM
//   - Windows CRLF → LF
//   - 全角引号/逗号/冒号 → 半角（中文输入法极易产生）
//   - 行首 // 注释与 /* */ 块注释
//   - 尾随逗号（`,}` / `,]`）
//   - Markdown 代码块包裹（```json ... ```）
//
// ⚠️ 已知取舍（不是 bug）：
//  1. 全角引号替换是**全局**的，会把字符串内容里的全角引号也换成半角。
//     该启发式针对的是"中文输入法把定界符打成全角"这一高频场景；若内容里
//     本身含全角引号，清洗后 JSON 会不合法 —— 此时报错信息会指出 JSON 解析失败，
//     用户改用正常引号即可。
//  2. 刻意**不**处理字符串内部的 `//` —— 那会破坏 URL（https://...）。
func CleanRawInput(raw []byte) []byte {
	s := string(raw)
	s = strings.TrimPrefix(s, "\ufeff") // BOM
	s = strings.ReplaceAll(s, "\r\n", "\n")
	// 全角引号/逗号/冒号 → 半角
	fullToHalf := strings.NewReplacer(
		"\u201c", `"`, "\u201d", `"`, // “ ”
		"\u2018", `'`, "\u2019", `'`, // ‘ ’
		"\uff1a", ":", "\uff0c", ",",
		"\u3002", ".", "\uff1b", ";",
	)
	s = fullToHalf.Replace(s)
	// 剥 Markdown 代码块围栏
	s = stripCodeFence(s)
	// 注释与尾随逗号
	s = blockCommentRe.ReplaceAllString(s, "")
	s = lineCommentRe.ReplaceAllString(s, "")
	s = trailingComma.ReplaceAllString(s, "$1")
	return []byte(strings.TrimSpace(s))
}

// stripCodeFence 把 ```json ... ``` 之类的围栏去掉，只留内容。
func stripCodeFence(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) == 0 {
		return s
	}
	// 找第一行以 ``` 开头的
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			start = i
			break
		}
	}
	if start < 0 {
		return s
	}
	// 找闭合围栏
	end := -1
	for i := len(lines) - 1; i > start; i-- {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
			end = i
			break
		}
	}
	if end < 0 {
		// 只有开头没有结尾：去掉开头那行即可
		return strings.Join(lines[start+1:], "\n")
	}
	return strings.Join(lines[start+1:end], "\n")
}

// ParseSnapshots 从原始文本解析出一批快照。
//
// 支持的输入形态（自动识别，逐个尝试）：
//
//  1. 单个快照对象          {"token":"...","refreshToken":"...",...}
//  2. 快照数组              [ {...}, {...} ]
//  3. 包装结构              {"accounts":[...]} / {"data":[...]} / {"tokens":[...]} / {"list":[...]}
//  4. Trae 客户端 storage.json 原文（含 iCubeAuthInfo://icube.cloudide 加密键）
//  5. 文本行（卡密格式）    token----refreshToken / token:refreshToken / 纯 token
//
// 返回 (快照列表, 逐行/逐条的错误说明)。有错但不影响成功项 ——
// 导入应当"尽量收下能用的"，而不是一条坏数据全盘失败。
func ParseSnapshots(raw []byte, product string) ([]*CredentialSnapshot, []string) {
	cleaned := CleanRawInput(raw)
	if len(cleaned) == 0 {
		return nil, []string{"输入为空"}
	}

	// 先试 JSON（对象或数组）
	if snaps, errs, ok := parseJSONSnapshots(cleaned, product); ok {
		return snaps, errs
	}
	// 再试逐行文本
	return parseLineSnapshots(cleaned, product)
}

// parseJSONSnapshots 尝试按 JSON 解析；ok=false 表示这不是 JSON（应回退到逐行）。
func parseJSONSnapshots(data []byte, product string) ([]*CredentialSnapshot, []string, bool) {
	trimmed := strings.TrimSpace(string(data))
	looksJSON := strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")
	if !looksJSON {
		return nil, nil, false
	}

	// 形态 1/2：单个对象或数组
	var asArray []json.RawMessage
	if err := json.Unmarshal(data, &asArray); err == nil {
		return snapsFromRawList(asArray, product)
	}
	var asObj map[string]json.RawMessage
	if err := json.Unmarshal(data, &asObj); err != nil {
		return nil, []string{"JSON 解析失败: " + err.Error()}, true
	}

	// 形态 4：客户端 storage.json —— 走 traectl 自己的解密路径
	if _, isStorage := asObj[keyAuth]; isStorage {
		snap, err := SnapshotFromStorageJSON(data, product)
		if err != nil {
			return nil, []string{"解析 storage.json 失败: " + err.Error()}, true
		}
		return []*CredentialSnapshot{snap}, nil, true
	}

	// 形态 3：包装结构
	for _, k := range []string{"accounts", "data", "tokens", "list", "items", "credentials"} {
		if v, ok := asObj[k]; ok {
			var list []json.RawMessage
			if err := json.Unmarshal(v, &list); err == nil {
				return snapsFromRawList(list, product)
			}
			// 也可能是 {accounts: {xxx: {...}}} 这种按 key 索引的对象
			var m map[string]json.RawMessage
			if err := json.Unmarshal(v, &m); err == nil {
				flat := make([]json.RawMessage, 0, len(m))
				for _, vv := range m {
					flat = append(flat, vv)
				}
				return snapsFromRawList(flat, product)
			}
		}
	}

	// 形态 1：单个快照对象
	return snapsFromRawList([]json.RawMessage{data}, product)
}

// snapsFromRawList 逐个解析快照对象，收集错误但不中断。
func snapsFromRawList(list []json.RawMessage, product string) ([]*CredentialSnapshot, []string, bool) {
	out := make([]*CredentialSnapshot, 0, len(list))
	var errs []string
	for i, raw := range list {
		var snap CredentialSnapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			errs = append(errs, fmt.Sprintf("第 %d 条: JSON 解析失败: %v", i+1, err))
			continue
		}
		if snap.Product == "" {
			snap.Product = product
		}
		snap.Normalize()
		if err := snap.Validate(); err != nil {
			errs = append(errs, fmt.Sprintf("第 %d 条（%s）: %v", i+1, snap.Label, err))
			continue
		}
		out = append(out, &snap)
	}
	return out, errs, true
}

// parseLineSnapshots 逐行解析卡密/纯 token 文本。
//
// 支持的每行形态：
//
//	accessToken----refreshToken
//	accessToken:refreshToken
//	accessToken
//
// 说明：**纯 token 行导入的账号无法续期**（没有 refreshToken 与设备私钥），
// 因此只在能拿到 refreshToken 时才认为可用；纯 token 行会被明确报错而不是
// 悄悄收下 —— 收下一份"过几小时就失效"的凭据比拒绝更糟。
func parseLineSnapshots(data []byte, product string) ([]*CredentialSnapshot, []string) {
	lines := strings.Split(string(data), "\n")
	out := make([]*CredentialSnapshot, 0)
	var errs []string
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		at, rt := splitTokenLine(line)
		if at == "" {
			errs = append(errs, fmt.Sprintf("第 %d 行: 无法识别", i+1))
			continue
		}
		if rt == "" {
			errs = append(errs, fmt.Sprintf("第 %d 行: 只有 access token 缺少 refreshToken，"+
				"而 Trae 续期还需要设备私钥 —— 请改用 JSON 快照导入", i+1))
			continue
		}
		snap := &CredentialSnapshot{
			Product: product, Token: at, RefreshToken: rt, Host: DefaultUGHost,
		}
		snap.Normalize()
		if err := snap.Validate(); err != nil {
			errs = append(errs, fmt.Sprintf("第 %d 行: %v", i+1, err))
			continue
		}
		out = append(out, snap)
	}
	if len(out) == 0 && len(errs) == 0 {
		errs = append(errs, "没有解析到任何可用凭据")
	}
	return out, errs
}

// splitTokenLine 按常见分隔符切分一行卡密。
func splitTokenLine(line string) (access, refresh string) {
	for _, sep := range []string{"----", "-----", "|", ",", "\t", ":", " "} {
		if i := strings.Index(line, sep); i > 0 {
			return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+len(sep):])
		}
	}
	return strings.TrimSpace(line), ""
}

// SnapshotFromStorageJSON 把一份客户端 storage.json 原文转成便携快照。
//
// 实现方式：写进临时文件再走 Store 的正规加载路径 —— 复用解密与字段解析，
// 避免把"如何解 tc 信封"的逻辑在导入路径上重写一遍。
func SnapshotFromStorageJSON(raw []byte, product string) (*CredentialSnapshot, error) {
	p, ok := ProductByKey(product)
	if !ok {
		return nil, fmt.Errorf("traectl: 未知产品 %q", product)
	}
	dir, err := os.MkdirTemp("", "trae-import-*")
	if err != nil {
		return nil, fmt.Errorf("traectl: 创建临时目录: %w", err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "storage.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return nil, fmt.Errorf("traectl: 写临时文件: %w", err)
	}
	st, err := OpenStore(p, path)
	if err != nil {
		return nil, err
	}
	snap := st.ExportSnapshot()

	// **按凭据自带的 host 校正版本域**（对齐参考实现"按域后缀判 realm"的做法）。
	//
	// 为什么必须做：用户导入的是"国际版客户端的 storage.json"时，如果只按
	// 调用方传的 product 走，会把它登记成国内账号 —— 于是后续所有请求都打到
	// 国内上游，表现为"账号明明健康却总是失败"，而报错完全指不出根因。
	//
	// 反过来（把国内凭据登记成国际）同样有害。所以以**凭据自己说的**为准。
	if corrected, ok := correctEditionByHost(snap, p); ok {
		snap.Product = corrected.Key
		// 重新打开一次，让 Store 用正确的产品（ClientID / PlatformCode 都不同）
		if st2, err2 := OpenStore(corrected, path); err2 == nil {
			snap = st2.ExportSnapshot()
			snap.Product = corrected.Key
		}
	}
	snap.Normalize()
	return snap, nil
}

// correctEditionByHost 依据凭据的 host 判断它其实属于哪个版本域的产品。
//
// 返回 (正确的产品, 是否需要校正)。判定依据是凭据里的 host 字段 ——
// Trae 的凭据自带 host（实测 CN 凭据可能没有该字段，此时按传入的 product 走）。
func correctEditionByHost(snap *CredentialSnapshot, requested Product) (Product, bool) {
	host := strings.TrimSpace(snap.Host)
	if host == "" {
		return requested, false // 凭据没说是哪个域 → 尊重调用方
	}
	isIntl := IsIntlHost(host)
	reqIntl := requested.Edition == EditionIntl
	if isIntl == reqIntl {
		return requested, false // 一致，无需校正
	}
	// 需要校正：找到同 IsSOLO 的另一个域的产品
	want := EditionCN
	if isIntl {
		want = EditionIntl
	}
	for _, p := range Products {
		if p.Edition == want && p.IsSOLO == requested.IsSOLO {
			return p, true
		}
	}
	// 找不到对应产品（例如国际版未启用）→ 不改，让上层看到原样
	//（导入仍会成功，但用错域时请求会失败，日志里能看到 host）
	return requested, false
}
