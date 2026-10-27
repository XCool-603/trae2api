package traectl

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// 续期端点与常量。
//
// 权威来源：本机客户端 out/main.js 的 exchangeTokenByRefreshToken() 与
// product.json 的 authConfig。**旧的 /cloudide/api/v3/trae/oauth/ExchangeToken
// 在当前客户端中已完全不存在**（只有 ClearRefreshToken），故不再实现。
const (
	// DefaultUGHost 是 CN 区的 OAuth/权益主机。凭据 JSON 自带的 host 字段也指向它。
	DefaultUGHost = "https://api.trae.cn"
	// exchangePath 续期路径（注意：不是 /cloudide/...）。
	exchangePath = "/trae/api/v3/oauth/ExchangeToken"
	// appVersion 是 DeviceInfo.ClientVersion / IDEVersion 的取值。
	// Trae.ai 官方后端要求 ide_required_version (实测当前最低为 3.5.20)。
	appVersion = "3.5.20"
)

// deviceInfo 是续期请求的 DeviceInfo（字段名必须 PascalCase，服务端按此解析）。
//
// 实测结论：服务端**并未严格比对全部硬件字段** —— 使用
// OSInfo="Windows" + 硬件字段全空 + OSVersion=真实系统版本的「宽松集」
// 一次通过（未出现 20403/20405）。故这里默认采用该宽松集。
type deviceInfo struct {
	DeviceID        string `json:"DeviceID"`
	MachineID       string `json:"MachineID"`
	PlatformCode    string `json:"PlatformCode"`
	DeviceType      string `json:"DeviceType"`
	DeviceName      string `json:"DeviceName"`
	DeviceModel     string `json:"DeviceModel"`
	ClientVersion   string `json:"ClientVersion"`
	DevicePublicKey string `json:"DevicePublicKey"`
	DeviceBrand     string `json:"DeviceBrand"`
	DeviceCPU       string `json:"DeviceCPU"`
	OSInfo          string `json:"OSInfo"`
	OSVersion       string `json:"OSVersion"`
}

// deviceProof 是 ECDSA 设备证明。字段名必须 PascalCase ——
// 小写会被服务端当作「未提供证明」而返回 20405。
type deviceProof struct {
	Signature string `json:"Signature"`
	Timestamp int64  `json:"Timestamp"`
	Nonce     string `json:"Nonce"`
}

type exchangeRequest struct {
	ClientID     string      `json:"ClientID"`
	ClientSecret string      `json:"ClientSecret"`
	RefreshToken string      `json:"RefreshToken"`
	DeviceInfo   deviceInfo  `json:"DeviceInfo"`
	DeviceProof  deviceProof `json:"DeviceProof"`
	IDEVersion   string      `json:"IDEVersion"`
}

type exchangeResponse struct {
	ResponseMetadata struct {
		Error *struct {
			Code    int    `json:"Code"`
			Message string `json:"Message"`
		} `json:"Error"`
	} `json:"ResponseMetadata"`
	Result *struct {
		Token            string `json:"Token"`
		RefreshToken     string `json:"RefreshToken"`
		TokenExpireAt    int64  `json:"TokenExpireAt"`
		RefreshExpireAt  int64  `json:"RefreshExpireAt"`
		BoundDeviceID    string `json:"BoundDeviceID"`
		DeviceBindStatus any    `json:"DeviceBindStatus"`
	} `json:"Result"`
}

// 已知续期错误码语义（用于把失败映射成明确的处置动作）。
const (
	// ErrCodeDeviceMismatch 设备不匹配：通常意味着注册时的设备指纹与当前不一致
	// （例如 Windows 大版本升级改变了 os.release()）。处置：重新读取 storage.json；
	// 仍失败则需用户在客户端重新登录。
	ErrCodeDeviceMismatch = 20403
	// ErrCodeDeviceProofRequired 需要设备证明：签名缺失/字段名小写/私钥不对。
	ErrCodeDeviceProofRequired = 20405
)

// RefreshError 携带上游业务错误码，便于上层做分级处置。
type RefreshError struct {
	HTTPStatus int
	Code       int
	Message    string
	Body       string
}

func (e *RefreshError) Error() string {
	return fmt.Sprintf("traectl: 续期失败 HTTP %d code=%d: %s", e.HTTPStatus, e.Code, e.Message)
}

// IsDeviceError 报告该错误是否属于「设备指纹/证明」类（可重读凭据或提示重登）。
func (e *RefreshError) IsDeviceError() bool {
	return e.Code == ErrCodeDeviceMismatch || e.Code == ErrCodeDeviceProofRequired
}

// httpClient 续期/签到/额度专用（超时 30s，与客户端一致）。
//
// 用 newHTTPClient 而不是裸 &http.Client{}：后者用标准库默认 Transport，
// 没有 TLS 握手超时、没有短 keepalive、且会经 ALPN 协商出 h2
// （半死 h2 流复用是"莫名卡住"的常见来源）。详见 transport.go。
var httpClient = newHTTPClient(30 * time.Second)

var (
	refreshProxyMu      sync.RWMutex
	refreshProxyClients = map[string]*http.Client{}
)

func proxyRefreshClient(proxyURL string) *http.Client {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		gp := GlobalProxy()
		if gp == "" {
			return httpClient
		}
		proxyURL = gp
	}
	refreshProxyMu.RLock()
	cl, ok := refreshProxyClients[proxyURL]
	refreshProxyMu.RUnlock()
	if ok {
		return cl
	}
	refreshProxyMu.Lock()
	defer refreshProxyMu.Unlock()
	if cl, ok := refreshProxyClients[proxyURL]; ok {
		return cl
	}
	cl = newHTTPClientWithProxy(30*time.Second, proxyURL)
	refreshProxyClients[proxyURL] = cl
	return cl
}

func (s *Store) httpClient() *http.Client {
	return proxyRefreshClient(s.Proxy())
}

// refreshMu 保证同一 Store 的续期串行 —— refreshToken 是**轮换语义**，
// 并发刷新会让后一次用已失效的旧 refreshToken 去换，导致整个账号掉线。
// 键为 *Store，值为 *sync.Mutex（按 Store 惰性创建）。
var refreshMu sync.Map

func (s *Store) refreshLock() *sync.Mutex {
	m, _ := refreshMu.LoadOrStore(s, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期）。
func (s *Store) NeedsRefresh(within time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.needsRefreshLocked(within)
}

func (s *Store) needsRefreshLocked(within time.Duration) bool {
	if s.Cred.Token == "" {
		return true
	}
	t, err := parseTraeTime(s.Cred.ExpiredAt)
	if err != nil {
		return true // 解析不了就刷新，宁多刷一次
	}
	return time.Now().Add(within).After(t)
}

// Token 返回当前 access token。
func (s *Store) Token() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Cred.Token
}

// Snapshot 返回凭据副本与设备信息（供上游请求构造使用）。
func (s *Store) Snapshot() (Credentials, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Cred, s.MachineID, s.DeviceID
}

// Refresh 执行 token 续期，成功后把新凭据写回 storage.json。
//
// 流程完全对齐客户端实现：
//
//	canonical = "POST\n<exchangePath>\n<ClientID>\n<RefreshToken>\n<unix秒>\n<32hex nonce>"
//	Signature = base64(ECDSA-P256-SHA256-DER(canonical, 设备私钥))
//	POST https://api.trae.cn/trae/api/v3/oauth/ExchangeToken
//	  headers: Content-Type: application/json ; x-cloudide-token: ""（必须空串）
func (s *Store) Refresh(ctx context.Context) error {
	mu := s.refreshLock()
	mu.Lock()
	defer mu.Unlock()

	s.mu.Lock()
	cred := s.Cred
	dev := s.Device
	deviceID := s.DeviceID
	machineID := s.MachineID
	s.mu.Unlock()

	if cred.RefreshToken == "" {
		return fmt.Errorf("traectl: %s 无 refreshToken", s.Product.Label)
	}
	if dev.PrivateKeyPEM == "" {
		return fmt.Errorf("traectl: %s 缺少设备私钥（iCubeAuthInfo://icube-dc:*），无法生成设备证明", s.Product.Label)
	}
	host := strings.TrimRight(cred.Host, "/")
	// 若存量账号 host 仍指向已下线的 api.trae.com.cn，自动纠偏迁移至 api.trae.cn
	if host == "" || strings.Contains(strings.ToLower(host), "api.trae.com.cn") {
		host = DefaultUGHost
	}

	proof, err := signDeviceProof(s.Product.ClientID, cred.RefreshToken, dev.PrivateKeyPEM)
	if err != nil {
		return err
	}
	reqBody := exchangeRequest{
		ClientID:     s.Product.ClientID,
		ClientSecret: "",
		RefreshToken: cred.RefreshToken,
		DeviceInfo: deviceInfo{
			DeviceID:        deviceID,
			MachineID:       machineID,
			PlatformCode:    s.Product.PlatformCode,
			DeviceType:      "PC",
			DeviceName:      deviceName(),
			DeviceModel:     "",
			ClientVersion:   appVersion,
			DevicePublicKey: dev.PublicKeyPEM,
			DeviceBrand:     "",
			DeviceCPU:       "",
			OSInfo:          "Windows",
			OSVersion:       osRelease(),
		},
		DeviceProof: *proof,
		IDEVersion:  appVersion,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("traectl: 序列化续期请求: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+exchangePath, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("traectl: 构造续期请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// 实测：该端点必须收到**空**的 x-cloudide-token；带旧 access token 会被
	// 服务端判成另一条鉴权分支并返回 20405。
	req.Header.Set("x-cloudide-token", "")

	cl := s.httpClient()
	resp, err := cl.Do(req)
	if err != nil {
		// 必须是 TransportError：上层据此判定"这是网络问题、不是账号问题"，
		// 从而不去硬冷却账号。返回普通 error 会丢掉这个语义。
		closeIdleOnFailure(cl.Transport)
		return &TransportError{Op: "refresh", Err: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out exchangeResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return &RefreshError{HTTPStatus: resp.StatusCode, Body: string(body),
			Message: "响应不是合法 JSON"}
	}
	if out.ResponseMetadata.Error != nil && out.ResponseMetadata.Error.Code != 0 {
		e := out.ResponseMetadata.Error
		return &RefreshError{HTTPStatus: resp.StatusCode, Code: e.Code, Message: e.Message, Body: string(body)}
	}
	if resp.StatusCode != http.StatusOK {
		return &RefreshError{HTTPStatus: resp.StatusCode, Body: string(body), Message: "非 200"}
	}
	if out.Result == nil || out.Result.Token == "" {
		return &RefreshError{HTTPStatus: resp.StatusCode, Body: string(body), Message: "响应缺少 Result.Token"}
	}

	s.mu.Lock()
	s.Cred.Token = out.Result.Token
	if out.Result.RefreshToken != "" {
		s.Cred.RefreshToken = out.Result.RefreshToken
	}
	if out.Result.TokenExpireAt > 0 {
		s.Cred.ExpiredAt = formatTraeTime(msToTime(out.Result.TokenExpireAt))
	}
	if out.Result.RefreshExpireAt > 0 {
		s.Cred.RefreshExpiredAt = formatTraeTime(msToTime(out.Result.RefreshExpireAt))
	}
	s.Cred.TokenReleaseAt = formatTraeTime(time.Now())
	s.mu.Unlock()

	return s.Save()
}

// buildCanonical 复刻客户端的签名原文构造：
//
//	[method, path, clientID, refreshToken, unix秒, 32位hex nonce].join("\n")
//
// 抽成独立函数是为了可测 —— 这个字符串里任何一个分隔符或顺序错了，
// 服务端都会以 20405「需要设备证明」拒绝，且报错完全不指向根因。
func buildCanonical(method, path, clientID, refreshToken string, ts int64, nonce string) string {
	return strings.Join([]string{
		method, path, clientID, refreshToken,
		fmt.Sprintf("%d", ts), nonce,
	}, "\n")
}

// signDeviceProof 复刻客户端的 _Te()：
//
//	canonical = [method, path, clientID, refreshToken, unix秒, 32位hex nonce].join("\n")
//	signature = base64(ECDSA-P256-SHA256(canonical))  —— DER 编码
//
// 注意 crypto.Sign 对 *ecdsa.PrivateKey 默认输出 ASN.1 DER，
// 与 Node 的 crypto.sign(..., PEM) 默认行为一致（都不是 ieee-p1363）。
func signDeviceProof(clientID, refreshToken, privateKeyPEM string) (*deviceProof, error) {
	key, err := parseECPrivateKey(privateKeyPEM)
	if err != nil {
		return nil, err
	}
	ts := time.Now().Unix()
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return nil, fmt.Errorf("traectl: 生成 nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes)

	canonical := buildCanonical(http.MethodPost, exchangePath, clientID, refreshToken, ts, nonce)
	digest := sha256.Sum256([]byte(canonical))
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		return nil, fmt.Errorf("traectl: ECDSA 签名: %w", err)
	}
	return &deviceProof{
		Signature: base64.StdEncoding.EncodeToString(sig),
		Timestamp: ts,
		Nonce:     nonce,
	}, nil
}

// parseECPrivateKey 解析 PEM（PKCS#8 或 SEC1）为 *ecdsa.PrivateKey。
func parseECPrivateKey(pemStr string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemStr)))
	if block == nil {
		return nil, fmt.Errorf("traectl: 设备私钥不是合法 PEM")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if ec, ok := k.(*ecdsa.PrivateKey); ok {
			return ec, nil
		}
		return nil, fmt.Errorf("traectl: 设备私钥不是 EC 密钥")
	}
	if ec, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return ec, nil
	}
	return nil, fmt.Errorf("traectl: 无法解析设备私钥（PKCS#8 / SEC1 均失败）")
}

// deviceName 复刻客户端的 A_e()：设备显示名取系统用户名。
func deviceName() string {
	for _, k := range []string{"USERNAME", "USER"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return "user"
}

// osVersionOnce 缓存探测到的系统版本（续期是低频操作，但没必要每次起进程）。
var (
	osVersionOnce sync.Once
	osVersionVal  string
)

// osRelease 返回与 Node `os.release()` 等价的系统版本串（Windows 形如 "10.0.26100"）。
//
// 该值参与续期 DeviceInfo，服务端理论上可能比对注册时指纹，故尽量取真实值。
// 可用环境变量 TRAE_OS_VERSION 覆盖（例如 Windows 大版本升级后需要固定旧值）。
func osRelease() string {
	osVersionOnce.Do(func() {
		if v := strings.TrimSpace(os.Getenv("TRAE_OS_VERSION")); v != "" {
			osVersionVal = v
			return
		}
		osVersionVal = detectOSRelease()
	})
	return osVersionVal
}

func detectOSRelease() string {
	if runtime.GOOS != "windows" {
		return runtime.GOOS
	}
	// 优先注册表（与 Node os.release() 同源：内核版本 10.0.<build>）
	out, err := exec.Command("reg", "query",
		`HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion`, "/v", "CurrentBuildNumber").Output()
	if err == nil {
		if b := parseRegBuildNumber(string(out)); b != "" {
			return "10.0." + b
		}
	}
	// 回退：ver 输出 "Microsoft Windows [Version 10.0.26100.1742]" → 取前三段
	out, err = exec.Command("cmd", "/c", "ver").Output()
	if err == nil {
		if v := parseVerOutput(string(out)); v != "" {
			return v
		}
	}
	return "10.0"
}

// parseRegBuildNumber 从 reg query 输出里取 CurrentBuildNumber 的值。
func parseRegBuildNumber(out string) string {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) >= 3 && strings.EqualFold(f[0], "CurrentBuildNumber") {
			return strings.TrimSpace(f[len(f)-1])
		}
	}
	return ""
}

// parseVerOutput 从 `ver` 输出里取 "10.0.26100"（丢弃第 4 段 UBR）。
func parseVerOutput(out string) string {
	i := strings.Index(out, "[Version ")
	if i < 0 {
		return ""
	}
	rest := out[i+len("[Version "):]
	if j := strings.IndexByte(rest, ']'); j >= 0 {
		rest = rest[:j]
	}
	parts := strings.Split(strings.TrimSpace(rest), ".")
	if len(parts) >= 3 {
		return strings.Join(parts[:3], ".")
	}
	return ""
}

// ---- 时间格式 ----

// parseTraeTime 解析凭据里的 ISO8601 时间（如 2026-10-23T05:10:11.618Z）。
func parseTraeTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, fmt.Errorf("空时间")
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析时间 %q", v)
}

// formatTraeTime 输出与客户端一致的毫秒精度 UTC 串。
func formatTraeTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// msToTime 把毫秒时间戳转 time.Time。
func msToTime(ms int64) time.Time { return time.UnixMilli(ms) }
