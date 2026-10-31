package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/upstream"
)

// installCatalog 直接灌入模型表缓存（同包白盒），免得为测一条出站规则去伪造整棵
// get_detail_param / batch 响应。灌进去的形状与 fetchDynamicModels 写的完全一致。
func installCatalog(t *testing.T, models ...upstream.ModelInfo) {
	t.Helper()
	realmByModel := map[string]string{}
	fnByModel := map[string]string{}
	pickOnlyByModel := map[string]bool{}
	maxVariantByModel := map[string]string{}
	paidOnlyByModel := map[string]bool{}
	for _, m := range models {
		// 缓存键是「地区/模型」：同名模型两个地区各有一条，裸名会互相覆盖。
		realm := m.Realm
		if realm == "" {
			realm = auth.RealmCN
		}
		key := realm + "/" + m.ID
		if _, ok := realmByModel[m.ID]; !ok {
			realmByModel[m.ID] = realm // 裸名归属只记先到的（与 fetchDynamicModels 一致）
		}
		fnByModel[key] = m.Function
		if m.PickOnly {
			pickOnlyByModel[key] = true
		}
		if m.PaidOnly {
			paidOnlyByModel[key] = true
		}
		if m.MaxAvailable() {
			maxVariantByModel[key] = m.MaxModelName
		}
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = models
	dynamicModelsCache.realmByModel = realmByModel
	dynamicModelsCache.fnByModel = fnByModel
	dynamicModelsCache.pickOnlyByModel = pickOnlyByModel
	dynamicModelsCache.maxVariantByModel = maxVariantByModel
	dynamicModelsCache.paidOnlyByModel = paidOnlyByModel
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.realmByModel = nil
		dynamicModelsCache.fnByModel = nil
		dynamicModelsCache.pickOnlyByModel = nil
		dynamicModelsCache.maxVariantByModel = nil
		dynamicModelsCache.paidOnlyByModel = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	})
}

// maxModel / plainModel 上游表里同一模型的两种形态（真夹具里的口径）：
// chat_v3 面 dev=116000 max=1000000 且带 __max 变体名；solo_work_lite 面只有 __dev。
func maxModel() upstream.ModelInfo {
	return upstream.ModelInfo{
		ID: "glm-5.2", Name: "GLM-5.2", Function: "solo_work_lite",
		ContextWindow: 116000, ContextMax: 1000000,
		DevModelName: "glm-5.2__dev", MaxModelName: "glm-5.2__max",
	}
}

func plainModel() upstream.ModelInfo {
	return upstream.ModelInfo{
		ID: "glm-5.2", Name: "GLM-5.2", Function: "solo_work_lite",
		ContextWindow: 116000, // 无 ctx_max、无 __max 变体名 = 上游没给 Max
		DevModelName:  "glm-5.2__dev",
	}
}

// chatWith 发一次 /v1/chat/completions，返回出站（改写后）的请求体。
//
// captured 由假上游填充：网关把 body 交给 upstream 之后我们才能看到最终报文，
// 断言写在这一层才是「真的发出去了什么」。
func chatWith(t *testing.T, cfg Config, body string) map[string]any {
	t.Helper()
	var captured map[string]any
	if cfg.Upstream == nil {
		cfg.Upstream = &upstream.Client{
			HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &captured)
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(soloSSE)),
				}, nil
			})},
			AgentHost: "https://fake.example", UgHost: "https://fake.example",
			OAuthHost: "https://fake.example", ClientID: upstream.ClientID,
		}
	}
	if cfg.Pool == nil {
		cfg.Pool = testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	}
	if cfg.MaxFunction == "" {
		cfg.MaxFunction = DefaultMaxFunction
	}
	h := NewHandler(cfg)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("入站请求没成功: code=%d body=%s", rec.Code, rec.Body)
	}
	if captured == nil {
		t.Fatal("假上游没收到请求，出站报文无从断言")
	}
	return captured
}

// TestMaxContextFieldSwitchesModelAndFunction max_context:true 时出站要同时换
// model（→ __max 变体）与 function（→ Max 面）。
//
// 换面是必须的：夹具实测 solo_work_lite 面上游没声明大窗口（12 个 __max 名、0 个更大），
// chat_v3 才有（20/20）。只换 model 不换面，等于发了个上游当 dev 处理的名字。
func TestMaxContextFieldSwitchesModelAndFunction(t *testing.T) {
	installCatalog(t, maxModel())
	got := chatWith(t, Config{}, `{"model":"glm-5.2","max_context":true,
		"messages":[{"role":"user","content":"hi"}]}`)

	if got["model"] != "glm-5.2__max" {
		t.Errorf("出站 model=%v，期望 glm-5.2__max", got["model"])
	}
	if got["function"] != "chat_v3" {
		t.Errorf("出站 function=%v，期望 chat_v3（Max 面）", got["function"])
	}
	// config_name 保持裸名：上游是「config_name 找配置、model 找配置里的变体」这个形状。
	if got["config_name"] != "glm-5.2" {
		t.Errorf("出站 config_name=%v，期望裸名 glm-5.2", got["config_name"])
	}
}

// TestMaxSuffixInModelNameTriggersMax 客户端原生写法（模型名带 __max 后缀）等价。
func TestMaxSuffixInModelNameTriggersMax(t *testing.T) {
	installCatalog(t, maxModel())
	got := chatWith(t, Config{}, `{"model":"cn:glm-5.2__max",
		"messages":[{"role":"user","content":"hi"}]}`)

	if got["model"] != "glm-5.2__max" || got["function"] != "chat_v3" {
		t.Errorf("后缀开法没生效: model=%v function=%v", got["model"], got["function"])
	}
}

// TestMaxIgnoredWhenUpstreamHasNoMax 上游没给 Max 时如实忽略：不改 model、不改 function、
// 请求照样成功（不 400）。夹具里 solo_work_lite 面的 glm-5.2 就是这种。
func TestMaxIgnoredWhenUpstreamHasNoMax(t *testing.T) {
	installCatalog(t, plainModel())
	got := chatWith(t, Config{}, `{"model":"glm-5.2","max_context":true,
		"messages":[{"role":"user","content":"hi"}]}`)

	if got["model"] != "glm-5.2" {
		t.Errorf("没有 Max 却改了 model: %v", got["model"])
	}
	if got["function"] != "solo_work_lite" {
		t.Errorf("没有 Max 却换了 function: %v", got["function"])
	}
}

// TestNoMaxFlagKeepsCurrentWireShape 不开 Max 时报文与改动前一致（零回归）。
func TestNoMaxFlagKeepsCurrentWireShape(t *testing.T) {
	installCatalog(t, maxModel())
	got := chatWith(t, Config{}, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)

	if got["model"] != "glm-5.2" || got["function"] != "solo_work_lite" {
		t.Errorf("默认档报文变了: model=%v function=%v", got["model"], got["function"])
	}
}

// TestMaxFunctionConfigurable Max 面可按配置改（solo_agent 面上 dev 200k + max 1M）。
func TestMaxFunctionConfigurable(t *testing.T) {
	installCatalog(t, maxModel())
	got := chatWith(t, Config{MaxFunction: "solo_agent"}, `{"model":"glm-5.2","max_context":true,
		"messages":[{"role":"user","content":"hi"}]}`)

	if got["function"] != "solo_agent" {
		t.Errorf("max_function 没生效: function=%v", got["function"])
	}
	if got["model"] != "glm-5.2__max" {
		t.Errorf("model=%v", got["model"])
	}
}

// TestDevSuffixStillMapsBack __dev 后缀仍按老行为剥回裸名（不能因为加 Max 把它打坏）。
func TestDevSuffixStillMapsBack(t *testing.T) {
	installCatalog(t, maxModel())
	got := chatWith(t, Config{}, `{"model":"glm-5.2__dev","messages":[{"role":"user","content":"hi"}]}`)

	if got["model"] != "glm-5.2" {
		t.Errorf("__dev 后缀没剥回裸名: %v", got["model"])
	}
	if got["function"] != "solo_work_lite" {
		t.Errorf("__dev 不该换 function: %v", got["function"])
	}
}

// TestModelListExposesMaxAvailability /v1/models 要如实报可用性，供客户端决定开不开 Max。
func TestModelListExposesMaxAvailability(t *testing.T) {
	installCatalog(t, maxModel())
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: &upstream.Client{}})
	var withMax map[string]any
	for _, m := range h.Models() {
		if m["id"] == "cn:glm-5.2" {
			withMax = m
		}
	}
	if withMax == nil {
		t.Fatal("/v1/models 里没有 cn:glm-5.2")
	}
	if withMax["max_available"] != true || withMax["max_model"] != "glm-5.2__max" {
		t.Errorf("Max 可用性没暴露: %v", withMax)
	}

	installCatalog(t, plainModel())
	var noMax map[string]any
	for _, m := range h.Models() {
		if m["id"] == "cn:glm-5.2" {
			noMax = m
		}
	}
	if _, has := noMax["max_available"]; has {
		t.Errorf("没有更大窗口不该报 max_available: %v", noMax)
	}
}

// TestSetModelNameInBody 只换 model，config_name 不动；空串是 no-op。
func TestSetModelNameInBody(t *testing.T) {
	in := []byte(`{"config_name":"glm-5.2","model":"glm-5.2","stream":true}`)
	var got map[string]any
	if err := json.Unmarshal(setModelNameInBody(in, "glm-5.2__max"), &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "glm-5.2__max" || got["config_name"] != "glm-5.2" {
		t.Errorf("setModelNameInBody 改错字段: %v", got)
	}
	if string(setModelNameInBody(in, "  ")) != string(in) {
		t.Error("空变体名应是 no-op")
	}
}

// TestMaxTwinModelNameTriggersMax /v1/models 里的公开 Max 名（<模型>-max）与
// max_context:true 等价：同样换 model（→ __max 变体）与 function（→ Max 面）。
func TestMaxTwinModelNameTriggersMax(t *testing.T) {
	installCatalog(t, maxModel())
	got := chatWith(t, Config{}, `{"model":"cn:glm-5.2-max",
		"messages":[{"role":"user","content":"hi"}]}`)

	if got["model"] != "glm-5.2__max" || got["function"] != "chat_v3" {
		t.Errorf("选 -max 条目没开 Max: model=%v function=%v", got["model"], got["function"])
	}
	if got["config_name"] != "glm-5.2" {
		t.Errorf("config_name 应是裸名（-max 不是上游名字）: %v", got["config_name"])
	}
}

// TestModelListPublishesMaxTwin Max 档要单独列一条 <模型>-max，窗口报 max 档；
// 没有更大窗口的模型不出现这种条目——客户端选了也不增长，列出来就是骗人。
func TestModelListPublishesMaxTwin(t *testing.T) {
	noMax := upstream.ModelInfo{ID: "glm-5.2-nomax", Name: "GLM-5.2", Function: "solo_work_lite",
		ContextWindow: 116000}
	installCatalog(t, maxModel(), noMax)
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: &upstream.Client{}})

	byID := map[string]map[string]any{}
	for _, m := range h.Models() {
		id, _ := m["id"].(string)
		byID[id] = m
	}
	if got := byID["cn:glm-5.2"]["context_length"]; got != int64(116000) {
		t.Errorf("原条目窗口被改了: %v", got)
	}
	twin, ok := byID["cn:glm-5.2-max"]
	if !ok {
		t.Fatalf("Max 档没单独列出（/v1/models 共 %d 条，没有 cn:glm-5.2-max）", len(byID))
	}
	if twin["context_length"] != int64(1000000) || twin["max"] != true {
		t.Errorf("Max 条目不对：context_length=%v max=%v", twin["context_length"], twin["max"])
	}
	if _, ok := byID["cn:glm-5.2-nomax-max"]; ok {
		t.Error("没有更大窗口的模型不该有 -max 条目")
	}
}

// TestModelNameEndingInMaxNotStripped 真名就以 -max 结尾的模型（Qwen3.8-Max）不能被
// 当成 Max 档后缀剥掉：剥完是 qwen3.8，mapModel 直接 400 unknown model。
func TestModelNameEndingInMaxNotStripped(t *testing.T) {
	qwen := upstream.ModelInfo{ID: "qwen3.8-max", Name: "Qwen3.8-Max", Function: "solo_work_lite",
		ContextWindow: 200000, ContextMax: 1000000, MaxModelName: "qwen3.8-max__max"}
	installCatalog(t, maxModel(), qwen)

	// 裸名照原样发出，config_name 也是它自己。
	got := chatWith(t, Config{}, `{"model":"cn:qwen3.8-max","messages":[{"role":"user","content":"hi"}]}`)
	if got["model"] != "qwen3.8-max" || got["config_name"] != "qwen3.8-max" {
		t.Errorf("真名被剥了: model=%v config_name=%v", got["model"], got["config_name"])
	}

	// 真正的 Max 档（同名再挂一个后缀）仍然要剥一层并开 Max。
	got = chatWith(t, Config{}, `{"model":"cn:qwen3.8-max-max","messages":[{"role":"user","content":"hi"}]}`)
	if got["model"] != "qwen3.8-max__max" || got["config_name"] != "qwen3.8-max" {
		t.Errorf("Max 档没开对: model=%v config_name=%v", got["model"], got["config_name"])
	}
}
