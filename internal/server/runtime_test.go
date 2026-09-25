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

func TestApplyRuntimeChangesDefaultModel(t *testing.T) {
	var model string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		var m struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(b, &m)
		model = m.Model
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(soloSSE)),
		}, nil
	})
	up := &upstream.Client{
		HTTP:       &http.Client{Transport: rt},
		StreamHTTP: &http.Client{Transport: rt},
		AgentHost:  "https://fake.example",
		ClientID:   upstream.ClientID,
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	h.ApplyRuntime(Runtime{DefaultModel: "kimi-k2.6"})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if model != "kimi-k2.6" {
		t.Fatalf("model=%q", model)
	}
}

// 面板里改密钥要立刻生效：旧密钥 401、新密钥 200，不用重启。
func TestApplyRuntimeSwapsAPIKey(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1"}), APIKey: "old"})
	call := func(k string) int {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		if k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := call("old"); code != 200 {
		t.Fatalf("旧密钥应通过: %d", code)
	}
	h.ApplyRuntime(Runtime{APIKey: "new"})
	if code := call("old"); code != 401 {
		t.Fatalf("旧密钥应失效: %d", code)
	}
	if code := call("new"); code != 200 {
		t.Fatalf("新密钥应通过: %d", code)
	}
	// 清空 = 关鉴权（面板里把密钥删掉就是这个语义）。
	h.ApplyRuntime(Runtime{APIKey: ""})
	if code := call(""); code != 200 {
		t.Fatalf("空密钥应不鉴权: %d", code)
	}
}
