package traectl

import (
	"os"
	"strings"
	"testing"
)

// TestIsCustomModel 用户自建渠道模型必须被排除 —— 否则网关会变成开放代理，
// 把用户自己的 OpenAI 兼容端点转发出去。
func TestIsCustomModel(t *testing.T) {
	custom := []string{
		"zai-plan//glm-5.3",
		"custom_openai_compatible//newapi_channel_conn",
		"custom_anything",
		"some-provider//some-model",
	}
	for _, c := range custom {
		if !isCustomModel(c) {
			t.Errorf("isCustomModel(%q) = false，应为 true", c)
		}
	}
	official := []string{"glm-5.3", "glm-5.2", "kimi-k3", "DeepSeek-V4-Pro-Official", "Doubao-Seed-2.1-Pro", "qwen3.8-max"}
	for _, o := range official {
		if isCustomModel(o) {
			t.Errorf("isCustomModel(%q) = true，应为 false", o)
		}
	}
}

// TestLoadCatalogReal 用本机真实 state.vscdb 验证模型目录加载。
//
// 期望的模型数量与「用 node:sqlite 直接查询后人工过滤自建渠道」的结果一致：
// Trae CN chat_v3 18 条 → 过滤 2 条自建 → 16；
// TRAE SOLO CN solo_work_lite 17 条 → 过滤 2 条自建 → 15。
func TestLoadCatalogReal(t *testing.T) {
	wantCount := map[string]int{"trae-cn": 21, "solo-cn": 15}
	tested := 0
	for _, p := range Products {
		dbPath, err := DefaultStateDBPath(p)
		if err != nil {
			t.Skip("无 APPDATA")
		}
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		t.Run(p.Key, func(t *testing.T) {
			cat, err := LoadCatalog(p, "")
			if err != nil {
				t.Skipf("加载失败（可能未登录/未拉取过模型列表）: %v", err)
			}
			if cat.Function != p.Function {
				t.Errorf("Function = %q want %q", cat.Function, p.Function)
			}
			if len(cat.Models) == 0 {
				t.Fatal("模型列表为空")
			}
			if want, ok := wantCount[p.Key]; ok && len(cat.Models) != want {
				t.Errorf("模型数 = %d want %d（若服务端目录已变化，请更新此期望值）", len(cat.Models), want)
			}
			for _, m := range cat.Models {
				if m.ID == "" {
					t.Error("存在 ID 为空的模型")
				}
				if strings.Contains(m.ID, "//") {
					t.Errorf("自建渠道模型未被过滤: %q", m.ID)
				}
				if m.Function != p.Function {
					t.Errorf("模型 %q 的 Function = %q", m.ID, m.Function)
				}
				if m.Display == "" {
					t.Errorf("模型 %q 的 Display 为空", m.ID)
				}
			}
			// 逐个能力断言：glm-5.2 是两套产品都有的官方模型
			gm, ok := cat.ByID("glm-5.2")
			if !ok {
				t.Errorf("未找到 glm-5.2；现有：%s", strings.Join(cat.IDs(), ", "))
			} else {
				if !gm.NativeFC {
					t.Error("glm-5.2 应支持原生 function calling（实测 chat_v3 官方模型全为 true）")
				}
				if gm.ParallelFC {
					t.Error("glm-5.2 不应支持并行工具调用（实测全部为 false）")
				}
				if gm.ContextWindow <= 0 {
					t.Errorf("glm-5.2 的 ContextWindow = %d，应 > 0", gm.ContextWindow)
				}
			}
			// 多模态模型应存在（如 kimi-k3 / Doubao 系）
			hasMulti := false
			for _, m := range cat.Models {
				if m.Multimodal {
					hasMulti = true
					break
				}
			}
			if !hasMulti {
				t.Error("未发现任何多模态模型，可疑")
			}
			// 诊断信息：池子清单
			if len(cat.AllPools) == 0 {
				t.Error("AllPools 为空")
			}
			t.Logf("%s: %d 个模型, 池子=%v, 首个=%s(ctx=%d, 多模态=%v)",
				p.Label, len(cat.Models), cat.AllPools, cat.Models[0].ID,
				cat.Models[0].ContextWindow, cat.Models[0].Multimodal)
		})
		tested++
	}
	if tested == 0 {
		t.Skip("本机未安装任何 Trae 产品")
	}
}

// TestLoadCatalogMissingDB 不存在的 db 路径必须返回错误而不是 panic。
func TestLoadCatalogMissingDB(t *testing.T) {
	if _, err := LoadCatalog(Products[0], t.TempDir()+`\nope.vscdb`); err == nil {
		t.Error("不存在的 state.vscdb 应返回错误")
	}
}

// TestCatalogByID 查找语义。
func TestCatalogByID(t *testing.T) {
	c := &Catalog{Models: []Model{{ID: "a"}, {ID: "b"}}}
	if m, ok := c.ByID("b"); !ok || m.ID != "b" {
		t.Error("ByID 命中失败")
	}
	if _, ok := c.ByID("z"); ok {
		t.Error("ByID 不应命中不存在的 ID")
	}
	if got := c.IDs(); len(got) != 2 || got[0] != "a" {
		t.Errorf("IDs() = %v", got)
	}
}
