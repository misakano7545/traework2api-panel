package upstream

import "testing"

// TestMergeCatalogMarksPickOnly 只有批量视图可见的模型要被标 PickOnly（出站据此去 config_name）。
func TestMergeCatalogMarksPickOnly(t *testing.T) {
	list := []ModelInfo{{ID: "glm-5.2", Name: "GLM-5.2", Function: Function}}
	cat := map[string]catalogMeta{
		"glm-5.2":           {ID: "glm-5.2", Label: "GLM-5.2", Visible: true, Dev: 200000, Max: 1000000},
		"kimi-k2.8-preview": {ID: "kimi-k2.8-preview", Label: "Kimi-K2.8-Preview", Visible: true, Dev: 200000, Max: 1000000},
	}
	out := mergeCatalog(list, cat, nil)
	byID := map[string]ModelInfo{}
	for _, m := range out {
		byID[m.ID] = m
	}
	if byID["glm-5.2"].PickOnly {
		t.Error("聊天配置表里有的模型不该标 PickOnly")
	}
	if !byID["kimi-k2.8-preview"].PickOnly {
		t.Fatal("批量视图独有的新模型应标 PickOnly")
	}
}
