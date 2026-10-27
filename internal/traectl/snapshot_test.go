package traectl

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCleanRawInput 清洗常见"非标准 JSON"写法。
func TestCleanRawInput(t *testing.T) {
	// 注意：全角引号在这里是**定界符**（中文输入法把 " 打成 “ ”），
	// 这正是该启发式要修的场景。若全角引号出现在字符串**内容**里，
	// 转换反而会破坏 JSON —— 这是已知取舍，见 CleanRawInput 的注释。
	in := "\ufeff{\r\n  // 这是注释\r\n  \u201ca\u201d: 1, /* 块注释 */\r\n  \u201cb\u201d: \u201cx\u201d,\r\n}\r\n"
	got := string(CleanRawInput([]byte(in)))
	if strings.Contains(got, "\ufeff") {
		t.Error("BOM 未剥离")
	}
	if strings.Contains(got, "\r") {
		t.Error("CRLF 未归一")
	}
	if strings.Contains(got, "//") || strings.Contains(got, "/*") {
		t.Errorf("注释未剥离: %s", got)
	}
	if strings.Contains(got, "\u201c") || strings.Contains(got, "\u201d") {
		t.Errorf("全角引号未转换: %s", got)
	}
	if strings.Contains(got, ",}") || strings.Contains(got, ",]") {
		t.Errorf("尾随逗号未清理: %s", got)
	}
	// 清洗后必须是合法 JSON
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("清洗后仍不是合法 JSON: %v\n%s", err, got)
	}
	if m["a"] != float64(1) || m["b"] != "x" {
		t.Errorf("内容不符: %v", m)
	}
}

// TestCleanRawInputPreservesURL 关键：不能把 URL 里的 // 当注释删掉。
func TestCleanRawInputPreservesURL(t *testing.T) {
	in := `{"host":"https://api.trae.cn","x":1}`
	got := string(CleanRawInput([]byte(in)))
	if !strings.Contains(got, "https://api.trae.cn") {
		t.Fatalf("URL 被破坏: %s", got)
	}
}

// TestCleanRawInputCodeFence 剥 Markdown 代码块围栏。
func TestCleanRawInputCodeFence(t *testing.T) {
	in := "好的，这是配置：\n```json\n{\"a\":1}\n```\n以上。"
	got := string(CleanRawInput([]byte(in)))
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		// 围栏外还有中文说明，解析失败是预期的；至少围栏标记应被去掉
		if strings.Contains(got, "```") {
			t.Errorf("代码围栏未剥离: %s", got)
		}
		return
	}
	if m["a"] != float64(1) {
		t.Errorf("内容不符: %v", m)
	}
}

// TestParseSnapshotsArray 数组形态。
func TestParseSnapshotsArray(t *testing.T) {
	raw := `[
	  {"product":"trae-cn","userId":"u1","token":"t1","refreshToken":"r1",
	   "deviceId":"d1","machineId":"m1","privateKeyPEM":"PRIV1","publicKeyPEM":"PUB1"}
	]`
	// 私钥是假的不该通过校验 —— 这里先验证"会被校验拦住"
	snaps, errs := ParseSnapshots([]byte(raw), "trae-cn")
	if len(snaps) != 0 {
		t.Fatalf("假私钥不应通过校验，却收下了 %d 条", len(snaps))
	}
	if len(errs) == 0 || !strings.Contains(errs[0], "privateKeyPEM") {
		t.Fatalf("错误说明应指出私钥无效: %v", errs)
	}
}

// TestParseSnapshotsWrapped 各种包装结构。
func TestParseSnapshotsWrapped(t *testing.T) {
	body := `{"userId":"u1","token":"t1","refreshToken":"r1","deviceId":"d1",
	          "machineId":"m1","privateKeyPEM":"PRIV","publicKeyPEM":"PUB"}`
	for _, wrapper := range []string{`{"accounts":[%s]}`, `{"data":[%s]}`, `{"tokens":[%s]}`,
		`{"list":[%s]}`, `[%s]`} {
		raw := strings.Replace(wrapper, "%s", body, 1)
		_, errs := ParseSnapshots([]byte(raw), "trae-cn")
		// 私钥是假的 → 必然报错；重点是**能被识别为一条记录**（而不是"输入无法识别"）
		if len(errs) == 0 {
			t.Errorf("包装 %s 应产生校验错误", wrapper)
			continue
		}
		if !strings.Contains(errs[0], "privateKeyPEM") {
			t.Errorf("包装 %s 未被识别为快照记录: %v", wrapper, errs)
		}
	}
}

// TestParseSnapshotsLineFormats 逐行卡密形态。
func TestParseSnapshotsLineFormats(t *testing.T) {
	cases := []struct{ name, line string }{
		{"四横线", "acc----ref"},
		{"冒号", "acc:ref"},
		{"竖线", "acc|ref"},
		{"制表符", "acc\tref"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errs := ParseSnapshots([]byte(c.line), "trae-cn")
			// 卡密行只有 token，没有设备私钥 → 必然被拒；重点是错误信息要**说清原因**
			if len(errs) == 0 {
				t.Fatal("只有 token 的行不应通过（无设备私钥无法续期）")
			}
			joined := strings.Join(errs, " ")
			if !strings.Contains(joined, "refreshToken") && !strings.Contains(joined, "privateKeyPEM") {
				t.Errorf("错误说明未点出缺什么: %v", errs)
			}
		})
	}
}

// TestParseSnapshotsBareToken 纯 token 行必须明确拒绝并说明原因。
func TestParseSnapshotsBareToken(t *testing.T) {
	_, errs := ParseSnapshots([]byte("eyJhbGciOiJIUzI1NiJ9.abc.def"), "trae-cn")
	if len(errs) == 0 {
		t.Fatal("纯 token 行应被拒绝")
	}
	if !strings.Contains(strings.Join(errs, " "), "refreshToken") {
		t.Errorf("应说明缺少 refreshToken: %v", errs)
	}
}

// TestParseSnapshotsEmpty 空输入。
func TestParseSnapshotsEmpty(t *testing.T) {
	snaps, errs := ParseSnapshots([]byte("   \n  "), "trae-cn")
	if len(snaps) != 0 || len(errs) == 0 {
		t.Errorf("空输入应报错: snaps=%d errs=%v", len(snaps), errs)
	}
}

// TestParseSnapshotsPartial 一条坏一条好时，应尽量收下能用的。
func TestParseSnapshotsPartial(t *testing.T) {
	// 用真实设备私钥生成一条合法快照，另一条故意缺字段
	priv, pub, err := generateTestKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	good := CredentialSnapshot{
		Product: "trae-cn", UserID: "u1", Token: "t1", RefreshToken: "r1",
		DeviceID: "d1", MachineID: "m1", PrivateKeyPEM: priv, PublicKeyPEM: pub,
	}
	gb, _ := json.Marshal(good)
	raw := "[" + string(gb) + `,{"product":"trae-cn","token":"only-token"}]`
	snaps, errs := ParseSnapshots([]byte(raw), "trae-cn")
	if len(snaps) != 1 {
		t.Fatalf("应收下 1 条合法快照，得到 %d", len(snaps))
	}
	if len(errs) != 1 {
		t.Fatalf("应有 1 条错误说明，得到 %v", errs)
	}
	if !strings.Contains(errs[0], "第 2 条") {
		t.Errorf("错误应指明是第 2 条: %v", errs)
	}
}

// TestSnapshotRoundTrip 导出 → 写盘 → 以快照模式重新打开。
func TestSnapshotRoundTrip(t *testing.T) {
	for _, p := range Products {
		src, err := DefaultStoragePath(p)
		if err != nil {
			t.Skip("无 APPDATA")
		}
		if _, err := os.Stat(src); err != nil {
			continue
		}
		t.Run(p.Key, func(t *testing.T) {
			// 1) 从真实 storage.json 打开并导出快照
			orig, err := OpenStore(p, "")
			if err != nil {
				t.Skipf("打开真实凭据失败: %v", err)
			}
			if orig.Mode() != ModeStorageJSON {
				t.Errorf("storage.json 模式识别错误: %v", orig.Mode())
			}
			snap := orig.ExportSnapshot()
			if err := snap.Validate(); err != nil {
				t.Fatalf("导出的快照未通过校验: %v", err)
			}

			// 2) 写到独立文件
			dst := filepath.Join(t.TempDir(), "snap.json")
			if err := WriteSnapshot(dst, snap); err != nil {
				t.Fatalf("WriteSnapshot: %v", err)
			}

			// 3) 以快照模式重新打开，字段必须逐项一致
			re, err := OpenStore(p, dst)
			if err != nil {
				t.Fatalf("以快照模式打开失败: %v", err)
			}
			if re.Mode() != ModeSnapshot {
				t.Fatalf("快照模式识别错误: %v", re.Mode())
			}
			reCred, reMachine, reDevice := re.Snapshot()
			origCred, origMachine, origDevice := orig.Snapshot()
			if reCred.Token != origCred.Token {
				t.Error("token 不一致")
			}
			if reCred.RefreshToken != origCred.RefreshToken {
				t.Error("refreshToken 不一致")
			}
			if reMachine != origMachine {
				t.Errorf("machineId 不一致: %q vs %q", reMachine, origMachine)
			}
			if reDevice != origDevice {
				t.Errorf("deviceId 不一致: %q vs %q", reDevice, origDevice)
			}
			if re.Device.PrivateKeyPEM != orig.Device.PrivateKeyPEM {
				t.Error("设备私钥不一致 —— 这会导致续期失败")
			}

			// 4) 快照模式下的续期签名必须能生成（证明私钥可用）
			if _, err := signDeviceProof(p.ClientID, reCred.RefreshToken, re.Device.PrivateKeyPEM); err != nil {
				t.Errorf("快照模式下无法生成 DeviceProof: %v", err)
			}

			// 5) 改字段后 Save 必须写回快照文件本身（而不是当成 storage.json 写坏）
			re.Cred.Token = "MODIFIED-TOKEN"
			if err := re.Save(); err != nil {
				t.Fatalf("快照模式 Save: %v", err)
			}
			re2, err := OpenStore(p, dst)
			if err != nil {
				t.Fatalf("重新打开失败: %v", err)
			}
			if re2.Cred.Token != "MODIFIED-TOKEN" {
				t.Errorf("快照写回未生效: %q", re2.Cred.Token)
			}
			// 写回后仍应是快照（不能变成 storage.json 结构）
			rawAfter, _ := os.ReadFile(dst)
			if strings.Contains(string(rawAfter), keyAuth) {
				t.Error("快照文件被写成了 storage.json 结构")
			}
		})
	}
}

// TestSnapshotValidate 校验必须拦住"能用但会很快失效"的凭据。
func TestSnapshotValidate(t *testing.T) {
	priv, pub, _ := generateTestKeyPair()
	base := func() *CredentialSnapshot {
		return &CredentialSnapshot{
			Product: "trae-cn", UserID: "u", Token: "t", RefreshToken: "r",
			DeviceID: "d", MachineID: "m", PrivateKeyPEM: priv, PublicKeyPEM: pub,
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("完整快照应通过: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*CredentialSnapshot)
		want string
	}{
		{"缺 product", func(c *CredentialSnapshot) { c.Product = "" }, "product"},
		{"缺 token", func(c *CredentialSnapshot) { c.Token = "" }, "token"},
		{"缺 refreshToken", func(c *CredentialSnapshot) { c.RefreshToken = "" }, "refreshToken"},
		{"缺私钥", func(c *CredentialSnapshot) { c.PrivateKeyPEM = "" }, "privateKeyPEM"},
		{"缺 deviceId", func(c *CredentialSnapshot) { c.DeviceID = "" }, "deviceId"},
		{"缺 userId", func(c *CredentialSnapshot) { c.UserID = "" }, "userId"},
		{"私钥格式错", func(c *CredentialSnapshot) { c.PrivateKeyPEM = "not a pem" }, "privateKeyPEM"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mut(c)
			err := c.Validate()
			if err == nil {
				t.Fatal("应报错")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误应提到 %q: %v", tc.want, err)
			}
		})
	}
}

// TestSnapshotNormalize 产品别名与默认值补齐。
func TestSnapshotNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"trae", "trae-cn"}, {"trae-cn", "trae-cn"}, {"TRAE_CN", "trae-cn"}, {"ide", "trae-cn"},
		{"solo", "solo-cn"}, {"solo-cn", "solo-cn"}, {"traework", "solo-cn"}, {"trae-solo", "solo-cn"},
	}
	for _, c := range cases {
		s := &CredentialSnapshot{Product: c.in, UserID: "u"}
		s.Normalize()
		if s.Product != c.want {
			t.Errorf("Normalize(%q) = %q want %q", c.in, s.Product, c.want)
		}
	}
	s := &CredentialSnapshot{Product: "trae-cn", UserID: "u1"}
	s.Normalize()
	if s.Host != DefaultUGHost {
		t.Errorf("host 未补默认值: %q", s.Host)
	}
	if s.Version != snapshotVersion {
		t.Errorf("version 未补: %d", s.Version)
	}
	if s.Label == "" {
		t.Error("label 未补默认值")
	}

	// 存量账号使用了已下线的 api.trae.com.cn 时必须自动纠偏迁移至 api.trae.cn
	sOldHost := &CredentialSnapshot{Product: "trae-cn", UserID: "u2", Host: "https://api.trae.com.cn"}
	sOldHost.Normalize()
	if sOldHost.Host != DefaultUGHost {
		t.Errorf("api.trae.com.cn 未自动迁移至 %q: %q", DefaultUGHost, sOldHost.Host)
	}
}

// TestSnapshotFromStorageJSON 从 storage.json 原文转快照（走解密路径）。
func TestSnapshotFromStorageJSON(t *testing.T) {
	src, err := DefaultStoragePath(Products[0])
	if err != nil || func() bool { _, e := os.Stat(src); return e != nil }() {
		t.Skip("本机无 Trae CN 凭据")
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Skip(err)
	}
	snap, err := SnapshotFromStorageJSON(raw, "trae-cn")
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if snap.Token == "" || snap.RefreshToken == "" || snap.DeviceID == "" || snap.PrivateKeyPEM == "" {
		t.Fatalf("转换结果字段缺失: %+v", snap)
	}
	if snap.Product != "trae-cn" {
		t.Errorf("product = %q", snap.Product)
	}
}

// TestSnapshotLiveRefresh 真实联调：**只凭一份导出的快照**完成续期。
//
// 这是"快照是否真正可移植"的终极验证 —— 如果能做到，就说明网关可以完全
// 脱离 Trae 客户端运行（把快照拷到另一台机器即可）。
//
// 会轮换 refreshToken，故默认跳过：TRAE_LIVE_SNAPSHOT=1 启用。
func TestSnapshotLiveRefresh(t *testing.T) {
	if os.Getenv("TRAE_LIVE_SNAPSHOT") != "1" {
		t.Skip("需 TRAE_LIVE_SNAPSHOT=1（会轮换 refreshToken）")
	}
	for _, p := range Products {
		t.Run(p.Key, func(t *testing.T) {
			orig, err := OpenStore(p, "")
			if err != nil {
				t.Skipf("打开真实凭据失败: %v", err)
			}
			// 关键：先把**原文件**的最新 token 写回，保证两边同步，
			// 然后只在快照副本上操作 —— 这样不会破坏客户端凭据。
			snap := orig.ExportSnapshot()
			dst := filepath.Join(t.TempDir(), "snap.json")
			if err := WriteSnapshot(dst, snap); err != nil {
				t.Fatal(err)
			}
			st, err := OpenStore(p, dst)
			if err != nil {
				t.Fatal(err)
			}
			before := st.Cred.ExpiredAt
			if err := st.Refresh(context.Background()); err != nil {
				t.Fatalf("只凭快照续期失败（说明快照不可移植）: %v", err)
			}
			after := st.Cred.ExpiredAt
			t.Logf("快照续期成功: %s → %s", before, after)
			if after == before {
				t.Error("续期后过期时间未变化，可疑")
			}
			// 快照文件本身必须已被更新
			re, err := OpenStore(p, dst)
			if err != nil {
				t.Fatal(err)
			}
			if re.Cred.ExpiredAt != after {
				t.Errorf("快照文件未写回: %q vs %q", re.Cred.ExpiredAt, after)
			}
		})
	}
}
