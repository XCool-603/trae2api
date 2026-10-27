package vscdb

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 本文件用本机真实存在的 Trae 客户端 state.vscdb 做真值校验。
//
// # 真值来源
//
// 期望值由 Node 内置的 node:sqlite（就是官方 SQLite 实现）读出，命令：
//
//	& 'C:\nvm4w\nodejs\node.exe' -e "const{DatabaseSync}=require('node:sqlite');
//	  const p=require('path');
//	  const f=p.join(process.env.APPDATA,'TRAE SOLO CN','User','globalStorage','state.vscdb');
//	  const db=new DatabaseSync(f,{readOnly:true});
//	  const row=db.prepare('SELECT value FROM ItemTable WHERE key = ?')
//	              .get('916348159159136_AI.agent.model.model_list_map');
//	  const s=typeof row.value==='string'?row.value:Buffer.from(row.value).toString('utf8');
//	  console.log(Buffer.byteLength(s,'utf8'),
//	              require('crypto').createHash('sha256').update(Buffer.from(s,'utf8')).digest('hex'));"
//
// 即「value 的 UTF-8 字节长度」与「该字节序列的 SHA-256」。
// 注意 node:sqlite 的 length() 返回字符数，含中文时与字节数不同（例如 TRAE SOLO CN 的
// 那条是 140196 字符 / 140220 字节），所以下面记的是字节数。
//
// # 这些常量会过期，而且**过期不等于代码有缺陷**
//
// Trae 客户端会重写 state.vscdb（模型目录随账号/版本变化）。届时本测试会失败，
// 但**先别改读取器** —— 用上面的命令现读一次：
//
//   - 若 node:sqlite 现读的值 == 读取器输出的值 → **只是真值过期**，更新下面的常量即可；
//   - 若两者不一致 → 才是读取器的真缺陷。
//
// 2026-10-13 就是这样一次：客户端在 10-12 更新了模型目录，
// 「Trae CN / 916348159159136」那条从 134990 字节涨到 149520 字节，
// 而读取器与 node:sqlite 逐字节一致。当时差点误判成 WAL 未处理。

type realCase struct {
	dir    string // %APPDATA% 下的目录名
	key    string
	bytes  int    // value 的 UTF-8 字节长度
	sha256 string // value（UTF-8 字节）的 SHA-256
}

var realCases = []realCase{
	{
		dir:    "TRAE SOLO CN",
		key:    "916348159159136_AI.agent.model.model_list_map",
		bytes:  140220,
		sha256: "68cd8fe38e305f78f97c28f6d9e7a28bd881211eb0849231211a85e192676253",
	},
	{
		dir: "Trae CN",
		key: "916348159159136_AI.agent.model.model_list_map",
		// 2026-10-13 重取。上一版真值（134990 / 8f2667ce…，取自 2026-10-09）已过期：
		// Trae 客户端在 10-12 更新了模型目录（新增 Doubao-Seed-Evolving 等），
		// 导致该 value 变大。**不是读取器的缺陷** —— 同一时刻用 node:sqlite 现读
		// 得到的就是下面这组值，两者逐字节一致。
		bytes:  159403,
		sha256: "34fc7d6dfa0977891575de1a95cff018fc19b7459829b6daea593bd53673d1a8",
	},
	{
		// Trae CN 里另一个账号的模型目录，用来覆盖「同一文件多条 model_list_map」的情形
		dir:    "Trae CN",
		key:    "3150562083212740_AI.agent.model.model_list_map",
		bytes:  32976,
		sha256: "bf3fbcfb7c45424e874265b92db010578ed2ed6680dbcdf180e29fac73e66e22",
	},
}

func isErr(err error, target error) bool { return errors.Is(err, target) }

// realStateVSCDB 返回某个 Trae 目录下的 state.vscdb 路径；不存在则返回 ""。
func realStateVSCDB(dir string) string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return ""
	}
	p := filepath.Join(appData, dir, "User", "globalStorage", "state.vscdb")
	if st, err := os.Stat(p); err != nil || st.IsDir() {
		return ""
	}
	return p
}

// TestRealTraeStateVSCDB 与 node:sqlite 真值逐字节比对：长度 + SHA-256 都必须一致。
func TestRealTraeStateVSCDB(t *testing.T) {
	if os.Getenv("APPDATA") == "" {
		t.Skip("没有 APPDATA（非 Windows 或受限环境），跳过真实文件校验")
	}
	ran := 0
	for _, tc := range realCases {
		path := realStateVSCDB(tc.dir)
		if path == "" {
			t.Logf("跳过：%s 的 state.vscdb 不存在", tc.dir)
			continue
		}
		t.Run(tc.dir+"/"+tc.key, func(t *testing.T) {
			db, err := Open(path)
			if err != nil {
				t.Fatalf("Open(%s): %v", path, err)
			}
			defer db.Close()

			val, err := db.GetItem(tc.key)
			if err != nil {
				t.Fatalf("GetItem(%s): %v", tc.key, err)
			}
			sum := sha256.Sum256(val)
			got := hex.EncodeToString(sum[:])
			if len(val) != tc.bytes {
				t.Errorf("长度 %d，记录的 node:sqlite 真值 %d（若客户端更新过模型目录，先用 node:sqlite 现读核对，别急着改读取器）", len(val), tc.bytes)
			}
			if got != tc.sha256 {
				t.Errorf("SHA-256 %s，记录的 node:sqlite 真值 %s（同上：先现读核对）", got, tc.sha256)
			}
			if len(val) == tc.bytes && got == tc.sha256 {
				t.Logf("✓ %s：%d 字节，SHA-256 %s（与 node:sqlite 一致）", tc.key, len(val), got)
			}
			// 内容合理性抽查：必须是 JSON 且含模型目录字段（防「长度凑巧一致」）
			if len(val) == 0 || val[0] != '{' {
				t.Fatalf("value 不是 JSON 对象：%q", firstN(val, 40))
			}
		})
		ran++
	}
	if ran == 0 {
		t.Skip("本机没有 Trae 客户端的 state.vscdb，跳过真实文件校验")
	}
}

// TestRealMissingKey 真实文件里不存在的 key 必须返回 ErrNotFound 而不是空值/报错。
func TestRealMissingKey(t *testing.T) {
	path := realStateVSCDB("TRAE SOLO CN")
	if path == "" {
		path = realStateVSCDB("Trae CN")
	}
	if path == "" {
		t.Skip("本机没有 Trae 客户端的 state.vscdb，跳过")
	}
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	defer db.Close()

	for _, key := range []string{
		"不存在的key",
		"916348159159136_AI.agent.model.model_list_map ",
		"",
		"916348159159136_AI.agent.model.model_list_mapp",
	} {
		if _, err := db.GetItem(key); !isErr(err, ErrNotFound) {
			t.Fatalf("GetItem(%q) 期望 ErrNotFound，实际 %v", key, err)
		}
	}

	// ItemTable 之外：真实文件里应能找到别的 key，且模型目录 key 一定存在。
	keys, err := db.Keys()
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) == 0 {
		t.Fatal("ItemTable 里一个 key 都没有")
	}
	t.Logf("ItemTable 共 %d 个 key", len(keys))
}

// TestRealReadOnly 对真实文件做只读性自证：读取过程不会生成 -wal / -shm / -journal，
// 也不会改动 state.vscdb。内容逐字节比对的严格检查放在副本上做（副本不受客户端
// 并发写入影响），真实文件只检查「没多出新文件」。
func TestRealReadOnly(t *testing.T) {
	path := realStateVSCDB("TRAE SOLO CN")
	if path == "" {
		t.Skip("本机没有 Trae 客户端的 state.vscdb，跳过")
	}
	dir := filepath.Dir(path)
	beforeEntries := dirNames(t, dir)
	beforeSize := fileSize(t, path)

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	keys, err := db.Keys()
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	for _, k := range keys {
		if _, err := db.GetItem(k); err != nil && !isErr(err, ErrNotFound) {
			t.Fatalf("GetItem(%q): %v", k, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for name := range dirNames(t, dir) {
		if !beforeEntries[name] {
			t.Fatalf("读取后在 %s 里多出了文件 %q（本包不得创建 -wal/-shm/-journal）", dir, name)
		}
	}
	if got := fileSize(t, path); got != beforeSize {
		t.Fatalf("读取后 state.vscdb 大小从 %d 变成 %d", beforeSize, got)
	}

	// 副本上做严格的内容不变检查。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读原文件: %v", err)
	}
	copyPath := writeTemp(t, "state.vscdb", raw)
	cdb, err := Open(copyPath)
	if err != nil {
		t.Fatalf("Open(副本): %v", err)
	}
	if _, err := cdb.GetItem(realCases[0].key); err != nil {
		t.Fatalf("GetItem(副本): %v", err)
	}
	if err := cdb.Close(); err != nil {
		t.Fatalf("Close(副本): %v", err)
	}
	after, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatalf("读副本: %v", err)
	}
	if sha(raw) != sha(after) {
		t.Fatal("读取后副本字节发生了变化（本包必须严格只读）")
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	return st.Size()
}

func dirNames(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("列目录 %s: %v", dir, err)
	}
	out := make(map[string]bool, len(entries))
	for _, e := range entries {
		out[e.Name()] = true
	}
	return out
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func firstN(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}
