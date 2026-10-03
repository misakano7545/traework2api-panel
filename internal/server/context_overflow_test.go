package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"traework2api/internal/auth"
	"traework2api/internal/upstream"
)

// 上下文超长（4026）是**调用方**的问题，不是账号的：不许给号上软冷却。
// 老口径按 "exceeded" 子串把它归成 ErrSoftRate → CooldownSoft 60s 起、指数到 2h；
// 客户端（见到 429/错误就重试的那些）一重试，轮转过的每个号都跟着躺下。
func TestClientContextOverflowDoesNotCoolAccount(t *testing.T) {
	var calls []string
	up := fakeWithPaths(&calls, func(path, _ string) (int, string, bool) {
		if path != upstream.EpChat {
			return 200, `{"user_info":{"config_info_list":[]}}`, false
		}
		// 实测原文（2026-10-03，dev 窗口 232768 的模型发 ~250K token）。
		return 200, "event:error\ndata:{\"code\":4026,\"message\":\"We're sorry, your context length has exceeded the maximum limit.\",\"extra\":null}\n\n" +
			"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n", true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"too long"}]}`)))

	// 两个号都吃了同一条超长请求，但**都不该被冷却**。
	for _, st := range p.List() {
		if st.Cooling {
			t.Fatalf("%s 被客户端超长请求打进冷却：reason=%q", st.UID, st.Reason)
		}
		if st.ErrCount != 0 {
			t.Fatalf("%s 被误记熔断计数 %d", st.UID, st.ErrCount)
		}
	}
	// 客户端要能读到上游原话（错误帧照旧透传），而不是一句笼统的 429。
	if body := rec.Body.String(); !strings.Contains(body, "4026") {
		t.Fatalf("错误帧应透传上游码与原话：%q", body)
	}
	if rec.Code >= 500 {
		t.Fatalf("不该回 %d（那是「所有账号不可用」的形状，会让客户端重试）", rec.Code)
	}
}
