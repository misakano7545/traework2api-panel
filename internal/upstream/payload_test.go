package upstream

import (
	"encoding/json"
	"testing"
)

// 显式 config_name 不能被 PrepareBody 覆盖：国际版槽位旁路靠它区分「槽位」和「具体模型」。
func TestPrepareBodyKeepsExplicitConfigName(t *testing.T) {
	// 槽位形态：config_name=槽位，model=槽位里具体那个模型
	src := []byte(`{"config_name":"custom_model_gpt-5","model":"openai/gpt-5.6-sol","messages":[],"stream":false}`)
	var obj map[string]any
	if err := json.Unmarshal(PrepareBody(src), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["config_name"] != "custom_model_gpt-5" {
		t.Errorf("config_name 被覆盖成 %v", obj["config_name"])
	}
	if obj["model"] != "openai/gpt-5.6-sol" {
		t.Errorf("model 被改写成 %v", obj["model"])
	}
	if obj["function"] != Function || obj["stream"] != true {
		t.Errorf("常规改写丢了: function=%v stream=%v", obj["function"], obj["stream"])
	}

	// 常规形态：PrepareBody **不再**补 config_name——由 handler 的 setModelInBody 与 model
	// 一起设（有的模型带上 config_name 上游就回 4001，只有 handler 知道谁属于那一类）。
	src = []byte(`{"model":"glm-5.2","messages":[]}`)
	obj = map[string]any{}
	if err := json.Unmarshal(PrepareBody(src), &obj); err != nil {
		t.Fatal(err)
	}
	if _, has := obj["config_name"]; has {
		t.Errorf("PrepareBody 不该再补 config_name，得到 %v", obj["config_name"])
	}
	if obj["model"] != "glm-5.2" {
		t.Errorf("model 应保持 glm-5.2，得到 %v", obj["model"])
	}
}

// 显式 function 不能被覆盖：coder 通道的模型（gemini-3.1-pro 等）要带 function=solo_coder，
// 发在 Work 通道上游回 4001。
func TestPrepareBodyKeepsExplicitFunction(t *testing.T) {
	var obj map[string]any
	if err := json.Unmarshal(PrepareBody([]byte(`{"function":"solo_coder","model":"gemini-3.1-pro","messages":[]}`)), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["function"] != "solo_coder" {
		t.Errorf("function 被覆盖成 %v", obj["function"])
	}
	// 常规（客户端不给 function）：仍是 Work 通道
	obj = map[string]any{}
	if err := json.Unmarshal(PrepareBody([]byte(`{"model":"glm-5.2","messages":[]}`)), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["function"] != Function {
		t.Errorf("默认 function = %v want %s", obj["function"], Function)
	}
}

// 槽位旁路表：只登记实测能出正文的组合（改表前先真发一遍）。
func TestIntlSlotRoutes(t *testing.T) {
	for model, want := range map[string]SlotRoute{
		"gpt-6-sol":  {"custom_model_gpt-5", "openai/gpt-5.6-sol"},
		"gpt-6-luna": {"custom_model_gpt-5", "openai/gpt-5.6-luna"},
	} {
		got, ok := IntlSlotRoute(model)
		if !ok || got != want {
			t.Errorf("IntlSlotRoute(%q) = %+v,%v want %+v", model, got, ok, want)
		}
	}
	if _, ok := IntlSlotRoute("gpt-5.2"); ok {
		t.Error("普通模型（直接能调的）不该有槽位旁路")
	}
}
