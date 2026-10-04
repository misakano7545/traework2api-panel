package server

import (
	"encoding/json"
	"testing"
)

// TestSetModelInBodyConfigName 出站 model/config_name 的分工：
//   - 普通模型（聊天配置表里有它）→ config_name=model（照旧，不动现有报文形状）
//   - 批量视图独有模型 → **不带** config_name（传了上游回 4001「param is invalid」，实测）
//   - 槽位旁路已经显式设了 config_name → 不覆盖（国际版 config_name=槽位 + model=具体模型）
func TestSetModelInBodyConfigName(t *testing.T) {
	obj := func(raw string, pickOnly bool) map[string]any {
		var m map[string]any
		if err := json.Unmarshal(setModelInBody([]byte(raw), "glm-5.2", pickOnly), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	normal := obj(`{"model":"x","messages":[]}`, false)
	if normal["config_name"] != "glm-5.2" || normal["model"] != "glm-5.2" {
		t.Errorf("普通模型应带上 config_name：%v", normal)
	}
	pick := obj(`{"model":"x","messages":[]}`, true)
	if _, has := pick["config_name"]; has {
		t.Errorf("批量视图独有模型不该带 config_name：%v", pick)
	}
	if pick["model"] != "glm-5.2" {
		t.Errorf("model 仍要设：%v", pick)
	}
	slot := obj(`{"config_name":"custom_model_gpt-5","model":"x"}`, false)
	if slot["config_name"] != "custom_model_gpt-5" {
		t.Errorf("槽位旁路的显式 config_name 被覆盖了：%v", slot)
	}
}
