package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"traework2api/internal/auth"
	"traework2api/internal/upstream"
)

// paidGateUpstream 假上游：pay_status 回指定身份，聊天路径回一条正常流。
// 计数用来看「有没有真的去打上游」——订阅专享模型必须一次都不打。
type paidGateUpstream struct {
	identity  int  // user_pay_identity 的值
	noPay     bool // true = pay_status 不返回身份字段（模拟上游改结构）
	planLimit bool // true = 聊天路径回 1005（模型级权益拒绝）
	chats     int
	pays      int
}

func (f *paidGateUpstream) client() *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			_ = body
			if strings.Contains(r.URL.Path, "pay_status") {
				f.pays++
				if f.noPay {
					return jsonResp(`{"detail":{}}`), nil
				}
				return jsonResp(`{"user_pay_identity":` + itoa(f.identity) + `,"user_pay_identity_str":"x"}`), nil
			}
			f.chats++
			sse := soloSSE
			if f.planLimit {
				sse = "event: error\ndata: {\"code\":1005,\"msg\":\"plan limit\"}\n\n"
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sse)),
			}, nil
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
}

func (f *paidGateUpstream) total() int { return f.chats + f.pays }

func jsonResp(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	out := ""
	for i > 0 {
		out = string(rune('0'+i%10)) + out
		i /= 10
	}
	return out
}

func paidCatalog(t *testing.T) {
	t.Helper()
	installCatalog(t,
		upstream.ModelInfo{ID: "gpt-6.1-sol", Name: "GPT-6.1-Sol", Function: "solo_work_lite", ContextWindow: 200000, PaidOnly: true},
		upstream.ModelInfo{ID: "gpt-5.2", Name: "GPT-5.2", Function: "solo_work_lite", ContextWindow: 200000},
	)
}

func chatReq(model string) *http.Request {
	return httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
}

// Free 号请求订阅专享模型：403 + 一次上游都不打（不烧积分、不触发 1005、不罚号）。
func TestPaidOnlyModelRejectedOnFreeAccount(t *testing.T) {
	paidCatalog(t)
	up := &paidGateUpstream{identity: 0}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up.client(),
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq("gpt-6.1-sol"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d body=%s，期望 403", rec.Code, rec.Body)
	}
	var e map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("resp not json: %v", err)
	}
	errObj, _ := e["error"].(map[string]any)
	if errObj["code"] != "model_requires_subscription" {
		t.Errorf("code=%v，期望 model_requires_subscription", errObj["code"])
	}
	// 只允许那次 pay_status 查询，聊天请求一次都不能发出去。
	if up.chats != 0 {
		t.Errorf("聊天请求打了 %d 次上游，Free 号调订阅专享模型不该烧请求", up.chats)
	}
}

// 免费模型照常走，不受影响。
func TestFreeModelUnaffectedByGate(t *testing.T) {
	paidCatalog(t)
	up := &paidGateUpstream{identity: 0}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up.client(),
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq("gpt-5.2"))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s，免费模型应照常 200", rec.Code, rec.Body)
	}
	if up.chats != 1 {
		t.Errorf("聊天请求打了 %d 次，期望 1", up.chats)
	}
}

// 订阅档账号请求订阅专享模型：放行（计划是按号给的，有订阅号就该让它试）。
func TestPaidOnlyModelAllowedOnPaidAccount(t *testing.T) {
	paidCatalog(t)
	up := &paidGateUpstream{identity: 1}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up.client(),
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq("gpt-6.1-sol"))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s，订阅档账号应放行", rec.Code, rec.Body)
	}
}

// pay_status 不返回身份时按「不知道」放行：不能因为上游改了结构就把付费用户全挡掉。
func TestPaidOnlyModelAllowedWhenIdentityUnknown(t *testing.T) {
	paidCatalog(t)
	up := &paidGateUpstream{noPay: true}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up.client(),
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq("gpt-6.1-sol"))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s，身份未知时应放行（不猜）", rec.Code, rec.Body)
	}
}

// 兜底：身份查不到、请求真被上游 1005 拒了，也不能把整号冷却 12 小时——
// 那会连带同号上几十个免费模型一起死（线上 17 天没成功过一次就是这么来的）。
func TestPlanLimitOnPaidOnlyModelDoesNotCoolAccount(t *testing.T) {
	paidCatalog(t)
	pool := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "event:error\ndata:{\"code\":1005,\"msg\":\"plan limit\"}\n\n", true
	})
	h := NewHandler(Config{Pool: pool, Upstream: up})
	req := chatReq("gpt-6.1-sol")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	st, ok := pool.Status("u1")
	if !ok {
		t.Fatal("账号不在池里")
	}
	if st.Cooling {
		t.Fatalf("订阅专享模型被 1005 拒后把整个号冷却了（reason=%q until=%v）——"+
			"同号上的免费模型会一起 503", st.Reason, st.Until)
	}
}

// 对照：普通模型拿 1005 也不罚号（模型级拒绝），但要学成订阅专享：
// 下一轮入口直接 403，不再打上游。
func TestPlanLimitOnFreeModelLearnsPaidOnly(t *testing.T) {
	paidCatalog(t)
	pool := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := &paidGateUpstream{identity: 0, planLimit: true}
	h := NewHandler(Config{Pool: pool, Upstream: up.client()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq("gpt-5.2"))
	st, _ := pool.Status("u1")
	if st.Cooling {
		t.Fatalf("1005 冷却了账号（%q）：1005 是模型级的，冷整号会把同号上几十个免费模型一起打死", st.Reason)
	}
	if !h.modelPaidOnly(auth.RealmCN, "gpt-5.2") {
		t.Fatal("1005 之后该模型应记为订阅专享，否则下一轮又白打一次上游")
	}

	before := up.chats
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq("gpt-5.2"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("第二轮 code=%d，期望 403（学成订阅专享后入口拦下）", rec.Code)
	}
	if up.chats != before {
		t.Errorf("第二轮又打了 %d 次上游", up.chats-before)
	}
}

// 同名模型两个地区档位不同：intl:glm-5.2 要订阅、cn:glm-5.2 免费（客户端实测：国际版列表里
// GLM-5.2 带锁）。合并模型表时同名只留先到的那个（cn），付费身份必须按地区单独记，
// 否则客户端显式写 intl:glm-5.2 会被放过 → 1005 → 又把整个国际号冷 12 小时。
func TestPaidOnlyIsPerRealm(t *testing.T) {
	installCatalog(t, upstream.ModelInfo{ID: "glm-5.2", ContextWindow: 200000})
	// 国际版那份在同名去重里被丢掉了，这里补上 fetchDynamicModels 记在去重之前的那一条。
	dynamicModelsCache.Lock()
	dynamicModelsCache.paidOnlyByModel[auth.RealmIntl+"/glm-5.2"] = true
	dynamicModelsCache.Unlock()

	up := &paidGateUpstream{identity: 0}
	pool := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "intl1", AccessToken: "at2", ExpiresAt: 9999999999, Domain: "www.trae.ai"},
	)
	h := NewHandler(Config{Pool: pool, Upstream: up.client()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq("intl:glm-5.2"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("intl:glm-5.2 code=%d body=%s，国际版订阅专享应 403", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq("cn:glm-5.2"))
	if rec.Code != 200 {
		t.Fatalf("cn:glm-5.2 code=%d body=%s，国内免费不该被国际版的档位连坐", rec.Code, rec.Body)
	}
}

// 国际版槽位旁路模型（gpt-6-sol / gpt-6-luna）在目录里也被标成订阅专享，但必须放行：
// identity_list 描述的是「直调模型名」那条路，槽位里挂的是 openai/gpt-5.6-sol，
// Free 号实测能出正文。按目录一刀切会把国际版唯一能用的模型砍掉（升级回归）。
func TestPaidOnlySlotRoutedIntlModelNotGated(t *testing.T) {
	installCatalog(t, upstream.ModelInfo{ID: "gpt-6-sol", ContextWindow: 200000, PaidOnly: true})
	dynamicModelsCache.Lock()
	dynamicModelsCache.realmByModel["gpt-6-sol"] = auth.RealmIntl
	dynamicModelsCache.paidOnlyByModel[auth.RealmIntl+"/gpt-6-sol"] = true
	dynamicModelsCache.Unlock()

	up := &paidGateUpstream{identity: 0}
	pool := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "intl1", AccessToken: "at2", ExpiresAt: 9999999999, Domain: "www.trae.ai"},
	)
	h := NewHandler(Config{Pool: pool, Upstream: up.client()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq("intl:gpt-6-sol"))
	if rec.Code != 200 {
		t.Fatalf("intl:gpt-6-sol code=%d body=%s，槽位旁路模型不该被 403 挡", rec.Code, rec.Body)
	}
	if up.chats != 1 {
		t.Errorf("聊天请求打了 %d 次，期望 1（应真的走上游）", up.chats)
	}
}
