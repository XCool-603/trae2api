package server

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"trae2api/internal/pool"
	"trae2api/internal/traectl"
)

// 本文件实现凭据的导入/导出（便携快照）。
//
// 设计取舍（与 workbuddy2api 的差异，刻意为之）：
//   - workbuddy2api 的导入是**同步**完成的（校验+落盘+入池在同一请求内），
//     前端只有一个 spinner。本项目沿用同步，但**逐条返回结果**（成功/失败+原因），
//     让前端能直接渲染"哪几条失败、为什么"，而不必猜。
//   - 导入**不做上游连通性校验**：那会让导入耗时取决于网络（几十秒），
//     且会消耗额度。凭据的有效性由首次真实请求暴露，或在控制台点「刷新额度」时验证。
//     这是刻意的取舍：导入要快、要可预测。

// importRequest 是 JSON 形式的导入请求（也支持 multipart 上传文件）。
type importRequest struct {
	// Text 粘贴的原始文本（JSON / 卡密行 / storage.json 原文）。
	Text string `json:"text"`
	// Product 当条目里没写 product 时使用的默认产品。
	Product string `json:"product"`
	// Label 给本次导入的账号统一加标签前缀（可选）。
	LabelPrefix string `json:"label_prefix"`
}

// importResult 是单条导入结果。
type importResult struct {
	Index   int    `json:"index"`
	OK      bool   `json:"ok"`
	File    string `json:"file,omitempty"`
	Label   string `json:"label,omitempty"`
	Product string `json:"product,omitempty"`
	UserID  string `json:"userId,omitempty"`
	// UID 是池内账号 ID（形如 trae-cn-916...）。
	// 校验任务需要它来定位账号；前端也用它做后续操作（解除冷却等）。
	UID   string `json:"uid,omitempty"`
	Error string `json:"error,omitempty"`
	// Replaced 表示覆盖了同账号的既有快照（refreshToken 是轮换语义，覆盖才对）。
	Replaced bool `json:"replaced,omitempty"`
}

// handleImport POST /admin/accounts/import
//
// 支持三种提交方式：
//  1. JSON body {"text":"...","product":"trae-cn"}
//  2. multipart/form-data，字段名 files（可多选，支持 .json/.txt/.zip）
//  3. 直接 POST 原始文本（Content-Type 非 JSON 且非 multipart）
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	ctype := r.Header.Get("Content-Type")
	var (
		items []importItem
		err   error
	)
	switch {
	case strings.HasPrefix(ctype, "multipart/form-data"):
		items, err = s.collectMultipart(r)
	case strings.HasPrefix(ctype, "application/json"):
		items, err = s.collectJSONBody(r)
	default:
		items, err = s.collectRawBody(r)
	}
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if len(items) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"没有收到任何内容（支持：粘贴 JSON/卡密文本、上传 .json/.txt/.zip）")
		return
	}

	results := make([]importResult, 0, len(items))
	okCount := 0
	for i, it := range items {
		res := s.importOne(it, i+1)
		if res.OK {
			okCount++
		}
		results = append(results, res)
	}
	// 导入成功后**启动异步校验**（续期 + 拉额度），并立刻返回 job_id。
	//
	// 为什么不同步做：校验要打上游，20 个账号串行要一分钟，中途断连就前功尽弃。
	// 异步后导入立即返回，进度可轮询（见 handleImportStatus）。
	var jobID string
	if okCount > 0 && s.imports != nil {
		refs := make([]*poolAccountRef, 0, okCount)
		for _, r := range results {
			if r.OK && r.UID != "" {
				refs = append(refs, &poolAccountRef{UID: r.UID, Label: r.Label})
			}
		}
		if len(refs) > 0 {
			jobID = s.imports.start(refs).ID
		}
	}

	resp := map[string]any{
		"results":        results,
		"total":          len(results),
		"succeed":        okCount,
		"failed":         len(results) - okCount,
		"accounts_total": s.poolAccountCount(),
	}
	if jobID != "" {
		resp["check_job_id"] = jobID
		resp["check_hint"] = "已启动后台校验（续期 + 拉额度）；轮询 " +
			"/admin/accounts/import/status?job_id=" + jobID + " 查看进度"
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleImportStatus GET /admin/accounts/import/status?job_id=...
//
// **恒返 200**（任务不存在也返回 state=unknown）：与 OAuth 轮询同一纪律 ——
// 前端轮询不该因为一次 404/5xx 而中断。
func (s *Server) handleImportStatus(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	id := r.URL.Query().Get("job_id")
	if id == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少 job_id")
		return
	}
	if s.imports == nil {
		writeJSON(w, http.StatusOK, map[string]any{"job_id": id, "state": "unknown"})
		return
	}
	job := s.imports.get(id)
	if job == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"job_id": id, "state": "unknown",
			"error": "校验任务不存在或已过期（结果保留 30 分钟）",
		})
		return
	}
	writeJSON(w, http.StatusOK, job.status())
}

// importItem 是一条待导入内容（可能来自文件，也可能是粘贴的文本）。
type importItem struct {
	name    string
	product string
	label   string
	data    []byte
}

func (s *Server) collectRawBody(r *http.Request) ([]importItem, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20)) // 32MB 上限
	if err != nil {
		return nil, fmt.Errorf("读取请求体: %w", err)
	}
	return []importItem{{name: "（粘贴内容）", product: "trae-cn", data: raw}}, nil
}

func (s *Server) collectJSONBody(r *http.Request) ([]importItem, error) {
	var req importRequest
	if err := decodeJSON(r, &req); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Text) == "" {
		return nil, fmt.Errorf("text 为空")
	}
	prod := strings.TrimSpace(req.Product)
	if prod == "" {
		prod = "trae-cn"
	}
	return []importItem{{
		name: "（粘贴内容）", product: prod, label: req.LabelPrefix, data: []byte(req.Text),
	}}, nil
}

// collectMultipart 读取上传的文件；ZIP 在内存里解开（不落盘）。
func (s *Server) collectMultipart(r *http.Request) ([]importItem, error) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		return nil, fmt.Errorf("解析 multipart: %w", err)
	}
	var items []importItem
	// 表单字段：product / label_prefix（可选）
	prod := strings.TrimSpace(r.FormValue("product"))
	if prod == "" {
		prod = "trae-cn"
	}
	labelPrefix := strings.TrimSpace(r.FormValue("label_prefix"))

	// 粘贴文本也允许与文件同时提交
	if txt := r.FormValue("text"); strings.TrimSpace(txt) != "" {
		items = append(items, importItem{
			name: "（粘贴内容）", product: prod, label: labelPrefix, data: []byte(txt),
		})
	}

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		// 兼容字段名 file / accounts
		for _, k := range []string{"file", "accounts", "upload"} {
			if fs := r.MultipartForm.File[k]; len(fs) > 0 {
				files = fs
				break
			}
		}
	}
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			return nil, fmt.Errorf("打开上传文件 %s: %w", fh.Filename, err)
		}
		data, err := io.ReadAll(io.LimitReader(f, 32<<20))
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("读取上传文件 %s: %w", fh.Filename, err)
		}
		name := filepath.Base(fh.Filename)
		// 过滤 macOS 压缩包里的元数据（否则会当成坏条目刷屏）
		if strings.HasPrefix(name, "._") || strings.HasPrefix(name, "__MACOSX") {
			continue
		}
		if strings.HasSuffix(strings.ToLower(name), ".zip") {
			sub, err := unzipItems(data, prod, labelPrefix)
			if err != nil {
				items = append(items, importItem{
					name: name, product: prod, label: labelPrefix, data: data,
				})
				continue
			}
			items = append(items, sub...)
			continue
		}
		items = append(items, importItem{name: name, product: prod, label: labelPrefix, data: data})
	}
	return items, nil
}

// unzipItems 在内存里解压 ZIP，返回其中的文本/JSON 条目。
//
// 只取 .json/.txt/.jsonc（无扩展名的也试），跳过目录与 macOS 元数据。
func unzipItems(data []byte, product, labelPrefix string) ([]importItem, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	var out []importItem
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		base := filepath.Base(f.Name)
		if strings.HasPrefix(base, "._") || strings.Contains(f.Name, "__MACOSX/") {
			continue
		}
		ext := strings.ToLower(filepath.Ext(base))
		if ext != "" && ext != ".json" && ext != ".txt" && ext != ".jsonc" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(rc, 8<<20))
		rc.Close()
		if err != nil || len(bytes.TrimSpace(b)) == 0 {
			continue
		}
		out = append(out, importItem{name: f.Name, product: product, label: labelPrefix, data: b})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("zip 内没有可识别的 .json/.txt")
	}
	return out, nil
}

// importOne 导入一条内容：解析 → 落盘 → 注册进池。
func (s *Server) importOne(it importItem, idx int) importResult {
	res := importResult{Index: idx}
	snaps, errs := traectl.ParseSnapshots(it.data, it.product)
	if len(snaps) == 0 {
		res.Error = strings.Join(errs, "；")
		if res.Error == "" {
			res.Error = "未解析到可用凭据"
		}
		return res
	}
	// 一条内容里可能有多份快照（JSON 数组）：全部收下，错误汇总到同一条结果里
	var lastFile string
	var lastSnap *traectl.CredentialSnapshot
	for _, snap := range snaps {
		if it.label != "" {
			snap.Label = it.label + " " + snap.Label
		}
		file, err := s.accounts.Add(snap)
		if err != nil {
			res.Error = err.Error()
			continue
		}
		lastFile, lastSnap = file, snap
		if err := s.registerAccount(snap, filepath.Join(s.accounts.Dir(), file)); err != nil {
			res.Error = err.Error()
			continue
		}
	}
	if lastSnap == nil {
		if res.Error == "" {
			res.Error = "全部条目导入失败"
		}
		return res
	}
	res.OK = true
	res.File = lastFile
	res.Label = lastSnap.Label
	res.Product = lastSnap.Product
	res.UserID = lastSnap.UserID
	// 池内 UID：校验任务与前端后续操作都要用它定位账号
	if prod, ok := traectl.ProductByKey(lastSnap.Product); ok {
		res.UID = AccountUID(prod, lastSnap.UserID)
	}
	if len(errs) > 0 {
		// 部分成功：把没进来的那些原因也带上，避免用户以为全成功
		res.Error = "部分条目失败：" + strings.Join(errs, "；")
	}
	return res
}

// registerAccount 把一个账号注册进池与对应渠道（运行时热添加）。
func (s *Server) registerAccount(snap *traectl.CredentialSnapshot, path string) error {
	prod, ok := traectl.ProductByKey(snap.Product)
	if !ok {
		return fmt.Errorf("未知产品 %q", snap.Product)
	}
	rk := RealmOfProduct(prod.Key)
	if rk == "" {
		return fmt.Errorf("产品 %q 没有对应渠道", prod.Key)
	}
	store, err := traectl.OpenStore(prod, path)
	if err != nil {
		return fmt.Errorf("打开凭据失败: %w", err)
	}
	cred, _, _ := store.Snapshot()
	uid := AccountUID(prod, cred.UserID)

	ch := s.channels[rk]
	if ch == nil {
		// 该 realm 还没有渠道（例如启动时没有任何账号）：新建一个
		catalog, cerr := traectl.LoadCatalog(prod, traectl.StateDBPathBeside(path))
		if cerr != nil {
			catalog = traectl.DefaultCatalog(prod)
		}
		ch = &Channel{
			Realm: rk, Product: prod, Catalog: catalog,
			accounts: map[string]*pool.Account{},
			// 用统一构造函数：否则这里会漏掉 IdleTimeout 等字段
			//（曾经就是这样 —— 运行时热导入的账号完全没有流中空闲监控）。
			client: newTraeClient(s.cfg, prod),
		}
		s.channels[rk] = ch
	}
	if ch.Account(uid) != nil {
		// 已存在：只更新凭据（refreshToken 轮换后重新导入是常见操作）
		if a := ch.Account(uid); a != nil && a.Store != nil {
			if snap.Proxy != "" {
				a.SetProxy(snap.Proxy)
			}
			if err := a.Store.Load(); err != nil {
				return fmt.Errorf("刷新既有账号凭据失败: %w", err)
			}
		}
		return nil
	}
	if snap.Proxy != "" {
		store.SetProxy(snap.Proxy)
	}
	a := &pool.Account{
		UID: uid, Realm: rk, Label: snap.Label, Product: prod, Owner: "public", Store: store, Proxy: snap.Proxy,
	}
	s.pool.Add(a)
	ch.mu.Lock()
	ch.accounts[uid] = a
	ch.mu.Unlock()
	s.logf("INFO: 导入账号 %s（%s, %s）", uid, prod.Label, snap.Label)
	return nil
}

// handleAccountsList GET /admin/accounts —— 列出全部账号（池内 + 快照）。
func (s *Server) handleAccountsList(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	entries, skipped := s.accounts.List()
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"file":             e.Name,
			"product":          e.Snap.Product,
			"label":            e.Snap.Label,
			"proxy":            e.Snap.Proxy,
			"userId":           e.Snap.UserID,
			"deviceId":         e.Snap.DeviceID,
			"expiredAt":        e.Snap.ExpiredAt,
			"refreshExpiredAt": e.Snap.RefreshExpiredAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["file"].(string) < out[j]["file"].(string)
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list", "data": out, "count": len(out),
		"skipped": skipped,
		"dir":     s.accounts.Dir(),
	})
}

// handleAccountDelete DELETE /admin/accounts/{name} —— 删除一份快照。
//
// 注意：只会从**快照目录**删除并解除池内注册。若该账号同时出现在
// config.trae.accounts 里（指向客户端 storage.json），删除快照不会影响那一份。
func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少快照文件名")
		return
	}
	// 先读出 userId，才能从池里摘掉对应账号
	var uid string
	if entries, _ := s.accounts.List(); len(entries) > 0 {
		for _, e := range entries {
			if e.Name == name {
				if p, ok := traectl.ProductByKey(e.Snap.Product); ok {
					uid = AccountUID(p, e.Snap.UserID)
				}
				break
			}
		}
	}
	if err := s.accounts.Delete(name); err != nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if uid != "" {
		s.pool.Remove(uid)
		for _, ch := range s.channels {
			ch.mu.Lock()
			delete(ch.accounts, uid)
			ch.mu.Unlock()
		}
		s.logf("INFO: 已删除账号 %s（快照 %s）", uid, name)
	}
	_ = s.pool.Save()
	writeJSON(w, http.StatusOK, map[string]any{"file": name, "deleted": true, "uid": uid})
}

// handleExport GET /admin/accounts/export —— 打包全部快照为 ZIP。
//
// 安全提示：ZIP 内含 refreshToken 与设备私钥，等同于账号的长期凭证。
// 因此本端点**要求管理员权限**，且响应头明确 no-store（不落任何缓存）。
// （对照：workbuddy2api 的 /ui/dsh/export 不校验角色就返回 api_key —— 这个坑不抄。）
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	entries, _ := s.accounts.List()
	if len(entries) == 0 {
		writeOpenAIError(w, http.StatusNotFound, "no_accounts", "没有可导出的快照账号")
		return
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		fw, err := zw.Create(e.Name)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "zip_error", err.Error())
			return
		}
		raw, err := os.ReadFile(e.Path)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "read_error", err.Error())
			return
		}
		if _, err := fw.Write(raw); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "zip_write_error", err.Error())
			return
		}
	}
	if err := zw.Close(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "zip_close_error", err.Error())
		return
	}
	name := fmt.Sprintf("trae2api-accounts-%d.zip", len(entries))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// poolAccountCount 返回池内账号数（导入结果里回显，便于前端确认）。
func (s *Server) poolAccountCount() int { return len(s.pool.List()) }
