package server

// 模型表拉取的两个回归点（2026-10-31 现场故障固化）：
//
//  1. realmReps 把「禁用/暂停」的号也当候选：它们凭据多半已失效，排在地区首位时
//     整个地区的模型表静默消失。
//  2. fetchDynamicModels 每个地区只试一个号，失败就 continue：一个失效号让国际版
//     46 个模型全部从 /v1/models 消失，而日志里一个字都没有。
//
// 现场形态：11 个 intl 号里 UID 最小的那个 token 失效，面板国际模型全灭。

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/upstream"
)

// resetModelCache 清空动态模型缓存（fetchDynamicModels 命中缓存就不拉取了）。
func resetModelCache(t *testing.T) {
	t.Helper()
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

// modelProbeBody 单 function 视图的模型表响应：一条可对话模型。
func modelProbeBody(configName, displayName string) string {
	return `{"config_info_list":[{"config_name":"` + configName + `","usage":"chat_completion",` +
		`"display_config":{"display_name":"` + displayName + `"},"context_window_tokens":{"dev":200000}}]}`
}

// modelsUpstream 按 Authorization 里的 access token 决定回什么：
// "bad" 回 401（模拟 session_dead），"cn"/"intl" 回各自的模型表。
// 批量视图端点一律 500，让 FetchModels 走「静默回退」分支（不影响结论）。
func modelsUpstream(t *testing.T) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authz := r.Header.Get("Authorization")
			if strings.HasSuffix(r.URL.Path, upstream.EpModelsBatch) {
				return &http.Response{
					StatusCode: 500,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":500}`)),
				}, nil
			}
			switch {
			case strings.HasSuffix(authz, "bad"):
				return &http.Response{
					StatusCode: 401,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":1001,"message":"not authenticated"}`)),
				}, nil
			case strings.HasSuffix(authz, "intl"):
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(modelProbeBody("gpt-5.2", "GPT-5.2"))),
				}, nil
			default:
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(modelProbeBody("glm-5.2", "GLM-5.2"))),
				}, nil
			}
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
}

// TestRealmRepsSkipsDisabledAndPaused：禁用/暂停的号不得进候选。
// 修复前它们会排进候选，凭据失效时把整个地区拖垮。
func TestRealmRepsSkipsDisabledAndPaused(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "u-disabled", AccessToken: "at-bad", ExpiresAt: 9999999999, Domain: "www.trae.ai"},
		&auth.Auth{UID: "u-paused", AccessToken: "at-bad", ExpiresAt: 9999999999, Domain: "www.trae.ai"},
		&auth.Auth{UID: "u-good", AccessToken: "at-intl", ExpiresAt: 9999999999, Domain: "www.trae.ai"},
	)
	p.Disable("u-disabled", "session_dead")
	p.Pause("u-paused", "手动暂停")

	h := NewHandler(Config{Pool: p, Upstream: modelsUpstream(t)})
	got := map[string]bool{}
	for _, a := range h.realmReps() {
		got[a.UID] = true
	}

	t.Logf("realmReps 候选 = %v", got)
	if got["u-disabled"] {
		t.Fatalf("禁用的号仍在候选里（凭据失效会拖垮整个地区）: %v", got)
	}
	if got["u-paused"] {
		t.Fatalf("暂停的号仍在候选里: %v", got)
	}
	if !got["u-good"] {
		t.Fatalf("健康号被漏掉: %v", got)
	}
}

// TestFetchDynamicModelsRetriesNextAccountInRealm：同地区首个号失败要换号重试，
// 不能整区放弃。现场：intl 代表号 token 失效 → 46 个国际模型全部消失。
func TestFetchDynamicModelsRetriesNextAccountInRealm(t *testing.T) {
	resetModelCache(t)

	// UID 升序 = 候选顺序：i-bad 排在 i-good 之前，正是现场形态。
	p := testPoolWith(
		&auth.Auth{UID: "c1-cn", AccessToken: "at-cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "i1-bad", AccessToken: "at-bad", ExpiresAt: 9999999999, Domain: "www.trae.ai"},
		&auth.Auth{UID: "i2-good", AccessToken: "at-intl", ExpiresAt: 9999999999, Domain: "www.trae.ai"},
	)
	h := NewHandler(Config{Pool: p, Upstream: modelsUpstream(t)})

	models := h.fetchDynamicModels()
	ids := map[string]string{}
	for _, m := range models {
		ids[m.ID] = ""
	}
	t.Logf("拉到 %d 个模型: %v", len(models), ids)

	if _, ok := ids["gpt-5.2"]; !ok {
		t.Fatalf("国际版模型缺失：首个号失败后没有换号重试，整区被放弃（拉到 %v）", ids)
	}
	if _, ok := ids["glm-5.2"]; !ok {
		t.Fatalf("国内版模型缺失（拉到 %v）", ids)
	}
}

// TestFetchDynamicModelsRealmPrefix：重试拿到的 intl 模型必须带 intl 归属，
// 否则选号会拿国内号去接国际模型（上游必失败）。
func TestFetchDynamicModelsRealmPrefix(t *testing.T) {
	resetModelCache(t)

	p := testPoolWith(
		&auth.Auth{UID: "i1-bad", AccessToken: "at-bad", ExpiresAt: 9999999999, Domain: "www.trae.ai"},
		&auth.Auth{UID: "i2-good", AccessToken: "at-intl", ExpiresAt: 9999999999, Domain: "www.trae.ai"},
	)
	h := NewHandler(Config{Pool: p, Upstream: modelsUpstream(t)})
	h.fetchDynamicModels()

	if realm := h.modelRealm("gpt-5.2"); realm != auth.RealmIntl {
		t.Fatalf("gpt-5.2 归属 = %q，重试拿到的国际模型应记为 %q", realm, auth.RealmIntl)
	}
}

// TestSameModelKeptInBothRealms：同名模型在两个地区各留一条。
//
// 现场：deepseek-v4.1-flash / minimax-m2.7 / kimi-k2.5 在两个地区的上游表里都存在，
// 但按裸名去重只留先到的（国内 UID 更小，所以永远先到）⇒ intl 版这三个从
// /v1/models 彻底消失，客户端能选、代理没有。
func TestSameModelKeptInBothRealms(t *testing.T) {
	resetModelCache(t)

	// 两个地区都提供同名模型：按 UID 升序 cn 先到（正是现场的失败形态）。
	p := testPoolWith(
		&auth.Auth{UID: "c1-cn", AccessToken: "at-cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "i1-intl", AccessToken: "at-intl", ExpiresAt: 9999999999, Domain: "www.trae.ai"},
	)
	up := sameModelUpstream(t)
	h := NewHandler(Config{Pool: p, Upstream: up})

	models := h.fetchDynamicModels()
	realms := map[string]string{}
	for _, m := range models {
		realms[m.Realm+":"+m.ID] = ""
	}
	t.Logf("条目 = %v", realms)

	if _, ok := realms[auth.RealmCN+":shared-model"]; !ok {
		t.Fatalf("国内版同名模型缺失: %v", realms)
	}
	if _, ok := realms[auth.RealmIntl+":shared-model"]; !ok {
		t.Fatalf("国际版同名模型被裸名去重吃掉（客户端能选、代理没有）: %v", realms)
	}
}

// sameModelUpstream 两个地区各回一条**同名**模型。
func sameModelUpstream(t *testing.T) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, upstream.EpModelsBatch) {
				return &http.Response{
					StatusCode: 500,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":500}`)),
				}, nil
			}
			// 两个地区都返回同名模型 shared-model，仅展示名不同。
			body := modelProbeBody("shared-model", "Shared Model")
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
}
