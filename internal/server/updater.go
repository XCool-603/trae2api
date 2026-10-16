package server

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// UpdateCheckResponse 是 GET /admin/system/update/check 的标准响应结构。
type UpdateCheckResponse struct {
	Supported    bool   `json:"supported"`
	Current      string `json:"current"`
	Latest       string `json:"latest,omitempty"`
	HasUpdate    bool   `json:"has_update"`
	ReleaseNotes string `json:"release_notes,omitempty"`
	DownloadURL  string `json:"download_url,omitempty"`
	AssetName    string `json:"asset_name,omitempty"`
	HTMLURL      string `json:"html_url,omitempty"`
	PublishedAt  string `json:"published_at,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

type ghReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

type ghRelease struct {
	TagName     string           `json:"tag_name"`
	Name        string           `json:"name"`
	Body        string           `json:"body"`
	HTMLURL     string           `json:"html_url"`
	PublishedAt string           `json:"published_at"`
	Assets      []ghReleaseAsset `json:"assets"`
}

// CheckUpdate 从 GitHub Releases 检查最新可用版本。
func CheckUpdate(ctx context.Context, repo, currentVer string) (*UpdateCheckResponse, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		repo = "XCool-603/trae2api"
	}

	apiURL := os.Getenv("TRAE2API_UPDATE_URL")
	if apiURL == "" {
		apiURL = fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造更新检查请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "trae2api-updater/"+Version)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return &UpdateCheckResponse{
			Supported: true,
			Current:   currentVer,
			HasUpdate: false,
			Reason:    fmt.Sprintf("连接发布服务器失败: %v", err),
		}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return &UpdateCheckResponse{
			Supported: true,
			Current:   currentVer,
			Latest:    currentVer,
			HasUpdate: false,
			Reason:    fmt.Sprintf("仓库 %s 暂无已发布的 Release 版本", repo),
		}, nil
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &UpdateCheckResponse{
			Supported: true,
			Current:   currentVer,
			HasUpdate: false,
			Reason:    fmt.Sprintf("检查更新失败 (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body))),
		}, nil
	}

	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析 Release 响应失败: %w", err)
	}

	latestVer := strings.TrimPrefix(strings.TrimSpace(rel.TagName), "v")
	hasUpdate := isNewerVersion(latestVer, currentVer)

	var matchedAsset *ghReleaseAsset
	matchedAsset = findMatchingAsset(rel.Assets, runtime.GOOS, runtime.GOARCH)

	res := &UpdateCheckResponse{
		Supported:    true,
		Current:      currentVer,
		Latest:       latestVer,
		HasUpdate:    hasUpdate,
		ReleaseNotes: rel.Body,
		HTMLURL:      rel.HTMLURL,
		PublishedAt:  rel.PublishedAt,
	}

	if matchedAsset != nil {
		res.DownloadURL = matchedAsset.BrowserDownloadURL
		res.AssetName = matchedAsset.Name
	} else if hasUpdate {
		res.Reason = fmt.Sprintf("发现新版本 %s，但未找到匹配当前平台 (%s/%s) 的构建产物", latestVer, runtime.GOOS, runtime.GOARCH)
	}

	return res, nil
}

// findMatchingAsset 根据操作系统和架构从 assets 中挑选最匹配的包。
func findMatchingAsset(assets []ghReleaseAsset, targetOS, targetArch string) *ghReleaseAsset {
	var candidates []ghReleaseAsset
	for _, a := range assets {
		name := strings.ToLower(a.Name)
		if strings.Contains(name, targetOS) && strings.Contains(name, targetArch) {
			candidates = append(candidates, a)
		}
	}

	if len(candidates) == 0 {
		return nil
	}

	// 优先选压缩包或直接可执行文件
	for _, c := range candidates {
		name := strings.ToLower(c.Name)
		if strings.HasSuffix(name, ".zip") || strings.HasSuffix(name, ".tar.gz") {
			return &c
		}
	}
	return &candidates[0]
}

// isNewerVersion 比较两版本号，若 latest > current 则返回 true。
func isNewerVersion(latest, current string) bool {
	latest = strings.TrimPrefix(strings.TrimSpace(latest), "v")
	current = strings.TrimPrefix(strings.TrimSpace(current), "v")
	if latest == "" || latest == current {
		return false
	}

	lParts := splitVersion(latest)
	cParts := splitVersion(current)

	maxLen := len(lParts)
	if len(cParts) > maxLen {
		maxLen = len(cParts)
	}

	for i := 0; i < maxLen; i++ {
		lNum := 0
		if i < len(lParts) {
			lNum = lParts[i]
		}
		cNum := 0
		if i < len(cParts) {
			cNum = cParts[i]
		}
		if lNum > cNum {
			return true
		}
		if lNum < cNum {
			return false
		}
	}
	return false
}

func splitVersion(v string) []int {
	parts := strings.Split(v, ".")
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		// 截掉可能存在的 pre-release 尾巴（如 0.2.0-beta）
		if idx := strings.IndexAny(p, "-+"); idx >= 0 {
			p = p[:idx]
		}
		n, _ := strconv.Atoi(p)
		nums = append(nums, n)
	}
	return nums
}

// DownloadAndExtractAsset 下载并解压出新的可执行文件字节。
func DownloadAndExtractAsset(ctx context.Context, downloadURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造下载请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "trae2api-updater/"+Version)

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载更新包失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载更新包返回 HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取更新包数据失败: %w", err)
	}

	return ExtractExecutable(data)
}

// ExtractExecutable 从原始数据中提取可执行文件内容（兼容 zip、tar.gz 或纯二进制）。
func ExtractExecutable(data []byte) ([]byte, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("数据长度不足")
	}

	// 1. zip 文件（魔数 PK\x03\x04）
	if bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, fmt.Errorf("解析 zip 文件失败: %w", err)
		}
		for _, f := range zr.File {
			name := strings.ToLower(f.Name)
			if f.FileInfo().IsDir() {
				continue
			}
			if strings.Contains(name, "trae2api") && (strings.HasSuffix(name, ".exe") || !strings.Contains(name, ".")) {
				rc, err := f.Open()
				if err != nil {
					return nil, fmt.Errorf("读取 zip 中可执行文件失败: %w", err)
				}
				defer rc.Close()
				return io.ReadAll(rc)
			}
		}
		return nil, fmt.Errorf("zip 包中未找到 trae2api 可执行文件")
	}

	// 2. tar.gz 文件（gzip 魔数 \x1f\x8b）
	if bytes.HasPrefix(data, []byte("\x1f\x8b")) {
		gr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("解析 gzip 文件失败: %w", err)
		}
		defer gr.Close()

		tr := tar.NewReader(gr)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("读取 tar 归档失败: %w", err)
			}
			name := strings.ToLower(hdr.Name)
			if !hdr.FileInfo().IsDir() && strings.Contains(name, "trae2api") {
				return io.ReadAll(tr)
			}
		}
		return nil, fmt.Errorf("tar.gz 包中未找到 trae2api 可执行文件")
	}

	// 3. 直接就是二进制文件
	return data, nil
}

// ReplaceCurrentExecutable 将当前运行的可执行文件热替换为新版本内容。
func ReplaceCurrentExecutable(newBinary []byte) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取当前程序路径失败: %w", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return fmt.Errorf("解析当前程序路径符号链接失败: %w", err)
	}

	dir := filepath.Dir(exePath)
	tmpFile, err := os.CreateTemp(dir, "trae2api-new-*")
	if err != nil {
		return fmt.Errorf("创建临时更新文件失败: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(newBinary); err != nil {
		tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("写入新可执行文件失败: %w", err)
	}
	tmpFile.Close()

	if err := os.Chmod(tmpPath, 0755); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("修改新文件执行权限失败: %w", err)
	}

	// 在 Windows 上，正在运行的 exe 无法直接被覆盖写入，但允许被重命名
	oldPath := exePath + ".old"
	_ = os.Remove(oldPath) // 清除历史残留

	if err := os.Rename(exePath, oldPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("备份当前可执行文件失败: %w", err)
	}

	if err := os.Rename(tmpPath, exePath); err != nil {
		// 替换失败，尝试回滚原文件
		_ = os.Rename(oldPath, exePath)
		_ = os.Remove(tmpPath)
		return fmt.Errorf("应用新可执行文件失败（已尝试回滚）: %w", err)
	}

	return nil
}

// RestartSelf 尝试拉起更新后的新程序进程并退出当前进程。
func RestartSelf() error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return err
	}

	cmd := exec.Command(exePath, os.Args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动新版本进程失败: %w", err)
	}

	go func() {
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	}()

	return nil
}
