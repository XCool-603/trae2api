package server

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"trae2api/internal/config"
)

// TestIsNewerVersion 测试语义化版本号比较。
func TestIsNewerVersion(t *testing.T) {
	cases := []struct {
		latest  string
		current string
		want    bool
	}{
		{"0.2.0", "0.1.0", true},
		{"v0.2.0", "0.1.0", true},
		{"0.1.1", "0.1.0", true},
		{"1.0.0", "0.9.9", true},
		{"0.1.0", "0.1.0", false},
		{"v0.1.0", "0.1.0", false},
		{"0.1.0", "0.2.0", false},
		{"0.0.9", "0.1.0", false},
		{"0.2.0-beta", "0.1.0", true},
		{"0.1.0-rc1", "0.1.0", false},
		{"", "0.1.0", false},
	}

	for _, c := range cases {
		got := isNewerVersion(c.latest, c.current)
		if got != c.want {
			t.Errorf("isNewerVersion(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}

// TestExtractExecutableFromZip 测试从 zip 压缩包中解压出 trae2api 可执行文件。
func TestExtractExecutableFromZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// 放入 README.md
	f1, err := zw.Create("README.md")
	if err != nil {
		t.Fatal(err)
	}
	f1.Write([]byte("docs"))

	// 放入可执行文件
	exeName := "trae2api"
	if runtime.GOOS == "windows" {
		exeName = "trae2api-windows-amd64.exe"
	}
	f2, err := zw.Create(exeName)
	if err != nil {
		t.Fatal(err)
	}
	expectedContent := []byte("fake binary executable content")
	f2.Write(expectedContent)

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	extracted, err := ExtractExecutable(buf.Bytes())
	if err != nil {
		t.Fatalf("ExtractExecutable 失败: %v", err)
	}
	if !bytes.Equal(extracted, expectedContent) {
		t.Errorf("提取内容不匹配: got %q, want %q", string(extracted), string(expectedContent))
	}
}

// TestExtractExecutableFromTarGz 测试从 tar.gz 压缩包中解压出 trae2api 可执行文件。
func TestExtractExecutableFromTarGz(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	expectedContent := []byte("linux binary content")
	hdr := &tar.Header{
		Name: "trae2api-linux-amd64",
		Mode: 0755,
		Size: int64(len(expectedContent)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(expectedContent); err != nil {
		t.Fatal(err)
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	extracted, err := ExtractExecutable(buf.Bytes())
	if err != nil {
		t.Fatalf("ExtractExecutable 失败: %v", err)
	}
	if !bytes.Equal(extracted, expectedContent) {
		t.Errorf("提取内容不匹配: got %q, want %q", string(extracted), string(expectedContent))
	}
}

// TestCheckUpdateMockServer 使用 mock server 测试完整的更新检测逻辑。
func TestCheckUpdateMockServer(t *testing.T) {
	mockRel := ghRelease{
		TagName:     "v0.9.0",
		Name:        "Trae2Api v0.9.0",
		Body:        "## 更新说明\n- 支持自动更新\n- 性能优化",
		HTMLURL:     "https://github.com/XCool-603/trae2api/releases/tag/v0.9.0",
		PublishedAt: "2026-10-15T12:00:00Z",
		Assets: []ghReleaseAsset{
			{
				Name:               "trae2api-windows-amd64.zip",
				BrowserDownloadURL: "https://example.com/trae2api-windows-amd64.zip",
				Size:               10240,
			},
			{
				Name:               "trae2api-linux-amd64.tar.gz",
				BrowserDownloadURL: "https://example.com/trae2api-linux-amd64.tar.gz",
				Size:               10240,
			},
			{
				Name:               "trae2api-darwin-arm64.tar.gz",
				BrowserDownloadURL: "https://example.com/trae2api-darwin-arm64.tar.gz",
				Size:               10240,
			},
		},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mockRel)
	}))
	defer ts.Close()

	t.Setenv("TRAE2API_UPDATE_URL", ts.URL)

	res, err := CheckUpdate(context.Background(), "test/repo", "0.1.0")
	if err != nil {
		t.Fatalf("CheckUpdate 出错: %v", err)
	}

	if !res.Supported {
		t.Error("res.Supported 应为 true")
	}
	if !res.HasUpdate {
		t.Error("res.HasUpdate 应为 true（最新版 0.9.0 > 当前 0.1.0）")
	}
	if res.Latest != "0.9.0" {
		t.Errorf("res.Latest = %q, want 0.9.0", res.Latest)
	}
	if res.DownloadURL == "" {
		t.Error("res.DownloadURL 不应为空")
	}
	if res.ReleaseNotes != mockRel.Body {
		t.Errorf("res.ReleaseNotes = %q, want %q", res.ReleaseNotes, mockRel.Body)
	}
}

// TestUpdateApplyRequireAdmin 测试更新应用接口权限。
func TestUpdateApplyRequireAdmin(t *testing.T) {
	s := newDSHTestServer(t)
	s.cfg = &config.Config{APIKey: "secret-key"}
	h := s.Handler()

	// 未提供 Key -> 401
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/system/update/apply", nil)
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("POST /admin/system/update/apply 未鉴权应为 401，实际为 %d", w.Code)
	}

	// GET 检查更新公开只读 -> 200
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/admin/system/update/check", nil)
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("GET /admin/system/update/check 应为 200，实际为 %d", w2.Code)
	}
}
