package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
	"traework2api/internal/upstream"
)

func TestMessagesToChat(t *testing.T) {
	src := []byte(`{
		"model":"glm-5.2",
		"max_tokens":128,
		"system":[{"type":"text","text":"sys-a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"sys-b"}],
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"look"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aaa"}}
			]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"secret","signature":"sig"},
				{"type":"text","text":"ok"},
				{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"ls"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":"a.txt"},
				{"type":"text","text":"next"}
			]}
		],
		"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"Bash","disable_parallel_tool_use":true},
		"stop_sequences":["END"],
		"top_k":5,
		"metadata":{"user_id":"u"},
		"thinking":{"type":"adaptive","display":"summarized"},
		"output_config":{"effort":"high","format":{"type":"json_schema","schema":{"type":"object"}}},
		"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}
		}`)
	got, err := messagesToChat(src)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["top_k"]; ok {
		t.Fatal("top_k must be dropped")
	}
	if obj["max_tokens"] != float64(128) || obj["user"] != "u" {
		t.Fatalf("scalars=%v", obj)
	}
	stops, _ := obj["stop"].([]any)
	if len(stops) != 1 || stops[0] != "END" {
		t.Fatalf("stop=%v", obj["stop"])
	}
	if obj["parallel_tool_calls"] != false {
		t.Fatalf("parallel=%v", obj["parallel_tool_calls"])
	}
	choice := obj["tool_choice"].(map[string]any)
	fn := choice["function"].(map[string]any)
	if fn["name"] != "Bash" {
		t.Fatalf("tool_choice=%v", choice)
	}
	if tools := obj["tools"].([]any); len(tools) != 1 {
		t.Fatalf("tools=%v", tools)
	}
	if obj["reasoning_effort"] != "high" {
		t.Fatalf("effort=%v", obj["reasoning_effort"])
	}
	if _, ok := obj["thinking"]; ok {
		t.Fatal("thinking must not be forwarded")
	}
	if _, ok := obj["output_config"]; ok {
		t.Fatal("output_config must not be forwarded")
	}
	if _, ok := obj["context_management"]; ok {
		t.Fatal("context_management must not be forwarded")
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("len=%d %#v", len(msgs), msgs)
	}
	if msgs[0].(map[string]any)["content"] != "sys-a\nsys-b" {
		t.Fatalf("system=%v", msgs[0])
	}
	user := msgs[1].(map[string]any)
	parts := user["content"].([]any)
	img := parts[1].(map[string]any)["image_url"].(map[string]any)["url"]
	if img != "data:image/png;base64,aaa" {
		t.Fatalf("image=%v", img)
	}
	asst := msgs[2].(map[string]any)
	if strings.Contains(mustJSON(asst), "secret") || strings.Contains(mustJSON(asst), "signature") {
		t.Fatalf("thinking leaked: %v", asst)
	}
	tc := asst["tool_calls"].([]any)[0].(map[string]any)
	if tc["id"] != "toolu_1" || tc["function"].(map[string]any)["arguments"] != `{"cmd":"ls"}` {
		t.Fatalf("tool_call=%v", tc)
	}
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "toolu_1" || tool["content"] != "a.txt" {
		t.Fatalf("tool=%v", tool)
	}
	if msgs[4].(map[string]any)["content"] != "next" {
		t.Fatalf("next=%v", msgs[4])
	}
}

func TestMessagesClientShapes(t *testing.T) {
	// Claude Code 实测顶层字段 + SDK 内置工具 + 文档块。模型名原样。
	src := []byte(`{
		"model":"claude-opus-4-6",
		"max_tokens":32000,
		"stream":true,
		"thinking":{"type":"adaptive"},
		"output_config":{"effort":"medium"},
		"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},
		"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral"}}],
		"tools":[
			{"name":"Bash","description":"run","input_schema":{"type":"object"},"eager_input_streaming":true},
			{"type":"bash_20250124","name":"bash"},
			{"type":"web_search_20250305","name":"web_search"}
		],
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"read "},
				{"type":"document","title":"note","source":{"type":"text","media_type":"text/plain","data":"hello"}},
				{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBE"}}
			]},
			{"role":"assistant","content":[
				{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"q":"x"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"srv_1","is_error":true,"content":[{"type":"text","text":"nope"}]}
			]}
		]
	}`)
	got, err := messagesToChat(src)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["model"] != "claude-opus-4-6" || obj["reasoning_effort"] != "medium" || obj["stream"] != true {
		t.Fatalf("scalars=%v", obj)
	}
	if _, ok := obj["context_management"]; ok {
		t.Fatal("context_management forwarded")
	}
	tools := obj["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "Bash" {
		t.Fatalf("tools=%v", tools)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("len=%d %#v", len(msgs), msgs)
	}
	if !strings.Contains(msgs[1].(map[string]any)["content"].(string), "bash") || !strings.Contains(msgs[1].(map[string]any)["content"].(string), "web_search") {
		t.Fatalf("note=%v", msgs[1])
	}
	user := msgs[2].(map[string]any)["content"].([]any)
	if user[1].(map[string]any)["text"] != "note\nhello" {
		t.Fatalf("doc=%v", user[1])
	}
	if user[2].(map[string]any)["text"] != "[document application/pdf omitted]" {
		t.Fatalf("pdf=%v", user[2])
	}
	asst := msgs[3].(map[string]any)
	if asst["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"] != "web_search" {
		t.Fatalf("server tool=%v", asst)
	}
	if msgs[4].(map[string]any)["content"] != "error: nope" {
		t.Fatalf("result=%v", msgs[4])
	}
}

func TestEffortFromBudget(t *testing.T) {
	got, err := messagesToChat([]byte(`{"model":"m","thinking":{"type":"enabled","budget_tokens":16000},"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["reasoning_effort"] != "high" {
		t.Fatalf("effort=%v", obj["reasoning_effort"])
	}
	got, err = messagesToChat([]byte(`{"model":"m","thinking":{"type":"disabled"},"output_config":{"effort":"low"},"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["reasoning_effort"] != "low" {
		t.Fatalf("effort wins=%v", obj["reasoning_effort"])
	}
}

func TestChatCompletionToMessage(t *testing.T) {
	got := chatCompletionToMessage(map[string]any{
		"id":    "chatcmpl-1",
		"model": "glm-5.2",
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role":              "assistant",
				"content":           "hi",
				"reasoning_content": "think",
				"tool_calls": []any{map[string]any{
					"id":       "call_a",
					"type":     "function",
					"function": map[string]any{"name": "Bash", "arguments": `{"cmd":"ls"}`},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 2},
	})
	if got["id"] != "msg_chatcmpl-1" || got["type"] != "message" || got["stop_reason"] != "tool_use" {
		t.Fatalf("envelope=%v", got)
	}
	content := got["content"].([]any)
	// reasoning_content 无 signature，作为 thinking 块下发展示（客户端回传时丢弃）。
	th := content[0].(map[string]any)
	if th["type"] != "thinking" || th["thinking"] != "think" {
		t.Fatalf("thinking=%v", th)
	}
	if content[1].(map[string]any)["text"] != "hi" {
		t.Fatalf("text=%v", content[1])
	}
	tu := content[2].(map[string]any)
	in := tu["input"].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_a" || in["cmd"] != "ls" {
		t.Fatalf("tool_use=%v", tu)
	}
	u := got["usage"].(map[string]any)
	if u["input_tokens"] != 1 || u["output_tokens"] != 2 {
		t.Fatalf("usage=%v", u)
	}
}

// TestAnthropicStopMapping 截断/内容过滤不得静默映射成 end_turn（Claude 侧语义）。
func TestAnthropicStopMapping(t *testing.T) {
	for fr, want := range map[string]string{
		"stop":           "end_turn",
		"length":         "max_tokens",
		"max_tokens":     "max_tokens",
		"tool_calls":     "tool_use",
		"function_call":  "tool_use",
		"content_filter": "refusal",
	} {
		if got := anthropicStop(fr); got != want {
			t.Fatalf("anthropicStop(%s)=%s want %s", fr, got, want)
		}
	}
}

func TestMessagesStreamEventOrder(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &messagesWriter{ResponseWriter: rec, stream: true}
	rw.Header().Set("Content-Type", "text/event-stream")
	frames := []string{
		`data: {"id":"chatcmpl-1","model":"glm-5.2","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"Bash","arguments":"{\"x\":"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, f := range frames {
		if _, err := rw.Write([]byte(f)); err != nil {
			t.Fatal(err)
		}
	}
	rw.finish()
	body := rec.Body.String()
	types := sseTypes(body)
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	for _, n := range want {
		if !containsStr(types, n) {
			t.Fatalf("missing %s in %v\n%s", n, types, body)
		}
	}
	if types[0] != "message_start" || types[len(types)-1] != "message_stop" {
		t.Fatalf("order=%v", types)
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Fatalf("chat [DONE] 泄漏: %s", body)
	}
	// reasoning_content 以 thinking 块下发（Claude Code 展示），且先于文本块。
	if !strings.Contains(body, `"type":"thinking_delta"`) || !strings.Contains(body, `"thinking":"think"`) {
		t.Fatalf("thinking 块 missing: %s", body)
	}
	if ti, xi := strings.Index(body, "thinking_delta"), strings.Index(body, `"type":"text_delta"`); ti < 0 || xi < 0 || ti > xi {
		t.Fatalf("thinking 应先于文本: ti=%d xi=%d", ti, xi)
	}
	if n := strings.Count(body, `"type":"content_block_stop"`); n < 2 {
		t.Fatalf("thinking/文本块都要关闭: stops=%d body=%s", n, body)
	}
	if !strings.Contains(body, `"text":"hi"`) || !strings.Contains(body, `"partial_json":"{\"x\":"`) || !strings.Contains(body, `"partial_json":"1}"`) {
		t.Fatalf("deltas missing: %s", body)
	}
	if !strings.Contains(body, `"stop_reason":"tool_use"`) {
		t.Fatalf("stop missing: %s", body)
	}
}

func TestMessagesEndpoint(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/messages"} {
		up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, soloSSE, true })
		p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
		h := NewHandler(Config{Pool: p, Upstream: up})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":false}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s code=%d body=%s", path, rec.Code, rec.Body.Bytes())
		}
		var obj map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
			t.Fatal(err)
		}
		if obj["type"] != "message" || obj["stop_reason"] != "end_turn" {
			t.Fatalf("%s got=%v", path, obj)
		}
		content := obj["content"].([]any)
		// soloSSE 夹具带 reasoning_content（"想一下"）：移植后以 thinking 块下发，
		// 文本块顺延到第 2 个。
		th, _ := content[0].(map[string]any)
		if th["type"] != "thinking" || th["thinking"] != "想一下" {
			t.Fatalf("%s thinking=%v", path, th)
		}
		text := content[1].(map[string]any)["text"]
		if text != "你好" {
			t.Fatalf("%s text=%v", path, text)
		}
	}
}

func TestMessagesEndpointStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, soloSSE, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.Bytes())
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "event-stream") {
		t.Fatalf("ct=%s", rec.Header().Get("Content-Type"))
	}
	types := sseTypes(rec.Body.String())
	if len(types) == 0 || types[0] != "message_start" || types[len(types)-1] != "message_stop" {
		t.Fatalf("types=%v body=%s", types, rec.Body.String())
	}
	if !containsStr(types, "content_block_delta") {
		t.Fatalf("no text delta in %v", types)
	}
}

func TestMessagesEndpointStreamErrorBeforeSSE(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: &upstream.Client{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200 SSE", rec.Code)
	}
	types := sseTypes(rec.Body.String())
	if !containsStr(types, "error") {
		t.Fatalf("types=%v body=%s", types, rec.Body.String())
	}
	if containsStr(types, "message_stop") {
		t.Fatalf("failed stream must not stop cleanly: %v", types)
	}
}

func TestMessagesAuthXAPIKey(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, soloSSE, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "secret"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key code=%d", rec.Code)
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["type"] != "error" || obj["error"].(map[string]any)["type"] != "authentication_error" {
		t.Fatalf("auth body=%v", obj)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "secret")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("x-api-key code=%d body=%s", rec.Code, rec.Body.Bytes())
	}
}

func TestMessagesInvalidJSON(t *testing.T) {
	_, err := messagesToChat([]byte(`{`))
	if err == nil {
		t.Fatal("want error")
	}
	h := NewHandler(Config{Pool: pool.New(""), Upstream: &upstream.Client{}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", bytes.NewReader([]byte(`{`))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["type"] != "error" {
		t.Fatalf("body=%v", obj)
	}
}

func TestMessagesEndpointNonStreamError(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: &upstream.Client{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/messages", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.Bytes())
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("body=%s err=%v", rec.Body.Bytes(), err)
	}
	errObj, _ := obj["error"].(map[string]any)
	if obj["type"] != "error" || errObj["type"] == "" || errObj["message"] == "" {
		t.Fatalf("code=%d body=%v", rec.Code, obj)
	}
}

// tool_result 里的图片：Claude Code 读截图、MCP 的截图类工具都这么回。
// 图不能留在 tool 消息里（Chat 的 tool 角色只接受文本），要等这批工具结果全部落地
// 之后另起一条 user 消息（占位文本在首个 part）。
// 口径与 workbuddy2api-panel 同款（那边先落地）。
func TestMessagesToolResultImageMoved(t *testing.T) {
	msgsFor := func(content string) []any {
		t.Helper()
		got, err := messagesToChat([]byte(`{"model":"m","messages":[{"role":"user","content":[` + content + `]}]}`))
		if err != nil {
			t.Fatal(err)
		}
		var obj map[string]any
		if err := json.Unmarshal(got, &obj); err != nil {
			t.Fatal(err)
		}
		msgs, _ := obj["messages"].([]any)
		return msgs
	}
	roles := func(m []any) string {
		var r []string
		for _, x := range m {
			r = append(r, asString(x.(map[string]any)["role"]))
		}
		return strings.Join(r, ",")
	}

	t.Run("text_and_image_then_client_text", func(t *testing.T) {
		m := msgsFor(`{"type":"tool_result","tool_use_id":"toolu_1","content":[
			{"type":"text","text":"shot"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aaa"}}]},
			{"type":"text","text":"next"}`)
		// 图的落点在「这批工具结果之后」的下一个消息边界（message 级 flush）：
		// 同一块里客户端自己还有正文时，图排在它之后 —— 与 WB 同款，硬约束只是
		// 「不能夹在 tool 消息之间」，这条形状满足。
		if got := roles(m); got != "tool,user,user" {
			t.Fatalf("角色序列=%s %#v", got, m)
		}
		if tool := m[0].(map[string]any); tool["tool_call_id"] != "toolu_1" || tool["content"] != "shot" {
			t.Fatalf("正文该留在 tool 消息里：%v", tool)
		}
		if m[1].(map[string]any)["content"] != "next" {
			t.Fatalf("客户端正文不该被合并：%v", m[1])
		}
		parts, ok := m[2].(map[string]any)["content"].([]any)
		if !ok || len(parts) != 2 || asString(parts[0].(map[string]any)["text"]) != toolImagePlaceholder {
			t.Fatalf("合成 user 消息=%#v", m[2])
		}
		img := parts[1].(map[string]any)
		if img["type"] != "image_url" {
			t.Fatalf("image part=%v", img)
		}
		if asString(img["image_url"].(map[string]any)["url"]) != "data:image/png;base64,aaa" {
			t.Fatalf("data URI 拼错：%v", img)
		}
	})

	// 只有图片时正文是空串（图另起一条 user 消息，占位文本就在那里指路）。
	t.Run("image_only_keeps_empty_text", func(t *testing.T) {
		m := msgsFor(`{"type":"tool_result","tool_use_id":"toolu_1","content":[
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aaa"}}]}`)
		if got := roles(m); got != "tool,user" {
			t.Fatalf("角色序列=%s %#v", got, m)
		}
		if m[0].(map[string]any)["content"] != "" {
			t.Fatalf("tool content=%v", m[0])
		}
	})

	t.Run("is_error_still_prefixed", func(t *testing.T) {
		m := msgsFor(`{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":[
			{"type":"text","text":"boom"}]}`)
		if tool := m[0].(map[string]any); tool["content"] != "error: boom" {
			t.Fatalf("is_error 前缀丢了：%v", tool)
		}
	})
}
