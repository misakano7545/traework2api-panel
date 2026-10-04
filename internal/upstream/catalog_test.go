package upstream

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"traework2api/internal/auth"
)

func mkParam(t *testing.T, raw string) paramConfig {
	t.Helper()
	var ci paramConfig
	if err := json.Unmarshal([]byte(raw), &ci); err != nil {
		t.Fatalf("造 paramConfig 失败: %v", err)
	}
	return ci
}

// TestParamConfigMetaVisibility 客户端选择器的四条尺子（缺一条就会多出一批上代/内部条目）。
func TestParamConfigMetaVisibility(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantVisible bool
		wantWhy     string
		wantRate    float64
		wantHasRate bool
		wantImage   bool
	}{
		{
			name: "在售模型：max 1M + 倍率 + 多模态",
			raw: `{"config_name":"glm-5.2","usage":"chat_completion",
			  "context_window_tokens":{"dev":200000,"max":1000000},
			  "display_config":{"display_name":"GLM-5.2","multimodal":true},
			  "display_contact_config":"{\"consumption_rate\":{\"enable\":true,\"data\":{\"rate\":0.78}}}"}`,
			wantVisible: true, wantRate: 0.78, wantHasRate: true, wantImage: true,
		},
		{
			name: "上代：max 缺失（dev 有值也没用）",
			raw: `{"config_name":"kimi-k2.6","usage":"chat_completion",
			  "context_window_tokens":{"dev":116000},
			  "display_config":{"display_name":"Kimi-K2.6"}}`,
			wantVisible: false, wantWhy: "上代（客户端代际阈值 max<250k）",
		},
		{
			name: "上代：max 恰好低于阈值",
			raw: `{"config_name":"qwen-3.6-plus","usage":"chat_completion",
			  "context_window_tokens":{"dev":200000,"max":249999},
			  "display_config":{"display_name":"Qwen3.6-Plus"}}`,
			wantVisible: false, wantWhy: "上代（客户端代际阈值 max<250k）",
		},
		{
			name: "内部子代理：前缀命中",
			raw: `{"config_name":"search_agent_v2","context_window_tokens":{"max":1000000},
			  "display_config":{"display_name":"x"}}`,
			wantVisible: false, wantWhy: "内部配置",
		},
		{
			name: "内部配置：精确名",
			raw: `{"config_name":"aquila","context_window_tokens":{"max":1000000},
			  "display_config":{"display_name":"Aquila"}}`,
			wantVisible: false, wantWhy: "内部配置",
		},
		{
			name: "自定义槽位回显",
			raw: `{"config_name":"custom_claude","usage":"custom_model","context_window_tokens":{"max":1000000},
			  "display_config":{"display_name":"Claude"}}`,
			wantVisible: false, wantWhy: "自定义模型回显",
		},
		{
			name: "服务端自定义预设回显（custom_models 非空）",
			raw: `{"config_name":"glm-5.3-flash","usage":"chat_completion","custom_models":[{"a":1}],
			  "context_window_tokens":{"max":1000000},"display_config":{"display_name":"GLM-5.3-Flash"}}`,
			wantVisible: false, wantWhy: "自定义模型回显",
		},
		{
			name: "客户端标记不可见",
			raw: `{"config_name":"glm-5.1","usage":"chat_completion","is_invisible_to_user":true,
			  "context_window_tokens":{"max":1000000},"display_config":{"display_name":"GLM-5.1"}}`,
			wantVisible: false, wantWhy: "客户端标记不可见",
		},
		{
			name: "客户端关了开关",
			raw: `{"config_name":"glm-5.1","usage":"chat_completion","config_switch":false,
			  "context_window_tokens":{"max":1000000},"display_config":{"display_name":"GLM-5.1"}}`,
			wantVisible: false, wantWhy: "客户端已关闭",
		},
		{
			name: "无展示名",
			raw: `{"config_name":"mystery","usage":"chat_completion","context_window_tokens":{"max":1000000},
			  "display_config":{"display_name":"  "}}`,
			wantVisible: false, wantWhy: "无展示名",
		},
	}
	for _, c := range cases {
		got := mkParam(t, c.raw).meta()
		if got.Visible != c.wantVisible || got.Why != c.wantWhy {
			t.Errorf("%s: visible=%v why=%q，期望 %v/%q", c.name, got.Visible, got.Why, c.wantVisible, c.wantWhy)
		}
		if got.HasRate != c.wantHasRate || (c.wantHasRate && got.Rate != c.wantRate) {
			t.Errorf("%s: rate=%v has=%v，期望 %v/%v", c.name, got.Rate, got.HasRate, c.wantRate, c.wantHasRate)
		}
		if got.HasImage != c.wantImage {
			t.Errorf("%s: image has=%v，期望 %v", c.name, got.HasImage, c.wantImage)
		}
	}
	// 坏倍率/零倍率都不能算「有倍率」——宁可显示 —，不编数
	if _, ok := contactRate(`{"consumption_rate":{"data":{"rate":0}}}`); ok {
		t.Error("rate=0 不该算有倍率")
	}
	if _, ok := contactRate("not json"); ok {
		t.Error("坏 JSON 不该算有倍率")
	}
	if _, ok := contactRate(""); ok {
		t.Error("空串不该算有倍率")
	}
	// __dev 条目 = 可调用实证
	if m := mkParam(t, `{"config_name":"x","model_detail_list":[{"model_name":"x__dev"}]}`).meta(); !m.HasDevEntry {
		t.Error("__dev 条目没被识别")
	}
}

// TestFetchModelsMergesBatchCatalog 端到端：单 function 视图与客户端批量视图并存时，
// FetchModels 用批量视图补齐新模型、标记上代、带上倍率与上下文双口径。
func TestFetchModelsMergesBatchCatalog(t *testing.T) {
	var sawBatch bool
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, EpModelsBatch) {
			sawBatch = true
			return jsonResp(200, `{"function_configs":[
			  {"function":"solo_agent","config_info_list":[
			    {"config_name":"glm-5.2","usage":"chat_completion",
			     "context_window_tokens":{"dev":200000,"max":1000000},
			     "display_config":{"display_name":"GLM-5.2","multimodal":true},
			     "display_contact_config":"{\"consumption_rate\":{\"enable\":true,\"data\":{\"rate\":0.78}}}",
			     "model_detail_list":[{"model_name":"glm-5.2__dev","max_tokens":32000}]},
			    {"config_name":"kimi-k2.8-preview","usage":"chat_completion",
			     "context_window_tokens":{"dev":200000,"max":1000000},
			     "display_config":{"display_name":"Kimi-K2.8-Preview"},
			     "display_contact_config":"{\"consumption_rate\":{\"data\":{\"rate\":0.98}}}"},
			    {"config_name":"glm-5.1","usage":"chat_completion","is_invisible_to_user":true,
			     "context_window_tokens":{"max":1000000},
			     "display_config":{"display_name":"GLM-5.1"}},
			    {"config_name":"custom_x","usage":"custom_model",
			     "context_window_tokens":{"max":1000000},
			     "display_config":{"display_name":"Custom"}}]}]}`), nil
		}
		// 单 function 视图（本仓老路径）：只有 glm-5.2 与 glm-5.1 两条
		return jsonResp(200, `{"config_info_list":[
		  {"config_name":"glm-5.2","usage":"chat_completion",
		   "context_window_tokens":{"dev":200000},
		   "display_config":{"display_name":"GLM-5.2(旧视图)"}},
		  {"config_name":"glm-5.1","usage":"chat_completion",
		   "context_window_tokens":{"dev":200000},
		   "display_config":{"display_name":"GLM-5.1"}}]}`), nil
	})
	list, err := c.FetchModels(&auth.Auth{UID: "u1", AccessToken: "at", Domain: "trae.cn"})
	if err != nil {
		t.Fatal(err)
	}
	if !sawBatch {
		t.Fatal("没有调用批量端点")
	}
	byID := map[string]ModelInfo{}
	for _, m := range list {
		byID[m.ID] = m
	}
	if got, ok := byID["kimi-k2.8-preview"]; !ok {
		t.Fatalf("批量视图可见的新模型没被补进来: %v", list)
	} else if got.Function != Function || got.Name != "Kimi-K2.8-Preview" || got.Rate == nil {
		t.Errorf("补进来的模型字段不对: %+v", got)
	}
	// 客户端已隐藏的上代条目仍然保留（mapModel 会把不在表里的判 400），只标记
	if legacy, ok := byID["glm-5.1"]; !ok || !legacy.Legacy || legacy.LegacyWhy == "" {
		t.Fatalf("上代条目应保留并标记 legacy: %+v (存在=%v)", legacy, ok)
	}
	g := byID["glm-5.2"]
	if g.Rate == nil || *g.Rate != 0.78 {
		t.Errorf("倍率没取到: %v", g.Rate)
	}
	if g.ContextMax != 1000000 || g.ContextWindow != 200000 {
		t.Errorf("上下文双口径不对: dev=%d max=%d", g.ContextWindow, g.ContextMax)
	}
	if g.Name != "GLM-5.2" {
		t.Errorf("展示名应取权威视图，实得 %q", g.Name)
	}
	if g.SupportsImage == nil || !*g.SupportsImage {
		t.Error("多模态标志没取到")
	}
	if g.Legacy {
		t.Error("在售模型不该被标记上代")
	}
	if _, ok := byID["custom_x"]; ok {
		t.Error("自定义槽位不该进模型表")
	}
}
