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

func TestResponsesToChat(t *testing.T) {
	src := []byte(`{
		"model":"gpt-5.3-codex",
		"instructions":"You are Codex",
		"input":[
			{"role":"user","content":[{"type":"input_text","text":"run ls"}]},
			{"type":"function_call","call_id":"call_1","name":"Bash","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"a.txt"},
			{"type":"reasoning","summary":[]}
		],
		"tools":[{"type":"function","name":"Bash","parameters":{"type":"object"}}],
		"reasoning":{"effort":"medium"},
		"max_output_tokens":128,
		"store":true,
		"previous_response_id":"resp_old"
	}`)
	got, _, err := responsesToChat(src)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["store"]; ok {
		t.Fatal("store must be dropped")
	}
	if _, ok := obj["previous_response_id"]; ok {
		t.Fatal("previous_response_id must be dropped")
	}
	if _, ok := obj["input"]; ok {
		t.Fatal("input must be translated away")
	}
	if obj["model"] != "gpt-5.3-codex" {
		t.Fatalf("model=%v", obj["model"])
	}
	if obj["reasoning_effort"] != "medium" {
		t.Fatalf("reasoning_effort=%v", obj["reasoning_effort"])
	}
	if obj["max_output_tokens"] != float64(128) {
		t.Fatalf("max_output_tokens=%v", obj["max_output_tokens"])
	}
	tools, _ := obj["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", tools)
	}
	tm := tools[0].(map[string]any)
	fn := tm["function"].(map[string]any)
	if fn["name"] != "Bash" {
		t.Fatalf("tool name=%v", fn["name"])
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len=%d %#v", len(msgs), msgs)
	}
	m0 := msgs[0].(map[string]any)
	if m0["role"] != "system" || m0["content"] != "You are Codex" {
		t.Fatalf("system=%v", m0)
	}
	m1 := msgs[1].(map[string]any)
	if m1["role"] != "user" || m1["content"] != "run ls" {
		t.Fatalf("user=%v", m1)
	}
	m2 := msgs[2].(map[string]any)
	if m2["role"] != "assistant" {
		t.Fatalf("assistant=%v", m2)
	}
	tcs := m2["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	if tc["id"] != "call_1" {
		t.Fatalf("call id=%v", tc["id"])
	}
	tfn := tc["function"].(map[string]any)
	if tfn["name"] != "Bash" || tfn["arguments"] != `{"cmd":"ls"}` {
		t.Fatalf("function=%v", tfn)
	}
	m3 := msgs[3].(map[string]any)
	if m3["role"] != "tool" || m3["tool_call_id"] != "call_1" || m3["content"] != "a.txt" {
		t.Fatalf("tool msg=%v", m3)
	}
}

func TestResponsesToChatStringInput(t *testing.T) {
	got, _, err := responsesToChat([]byte(`{"model":"m","input":"hi","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	_ = json.Unmarshal(got, &obj)
	msgs := obj["messages"].([]any)
	m := msgs[0].(map[string]any)
	if m["role"] != "user" || m["content"] != "hi" {
		t.Fatalf("got=%v", m)
	}
	if obj["stream"] != true {
		t.Fatalf("stream=%v", obj["stream"])
	}
}

func TestChatCompletionToResponse(t *testing.T) {
	chat := map[string]any{
		"id":      "chatcmpl-1",
		"object":  "chat.completion",
		"created": float64(1),
		"model":   "m",
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":              "assistant",
				"content":           "hi",
				"reasoning_content": "think",
				"tool_calls": []any{map[string]any{
					"id":       "call_a",
					"type":     "function",
					"function": map[string]any{"name": "Bash", "arguments": "{}"},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3},
	}
	got := chatCompletionToResponse(chat, nil)
	if got["id"] != "resp_chatcmpl-1" || got["object"] != "response" || got["status"] != "completed" {
		t.Fatalf("envelope=%v", got)
	}
	out := got["output"].([]any)
	if len(out) != 3 {
		t.Fatalf("output len=%d %#v", len(out), out)
	}
	r0 := out[0].(map[string]any)
	if r0["type"] != "reasoning" {
		t.Fatalf("item0=%v", out[0])
	}
	sum, _ := r0["summary"].([]any)
	if len(sum) != 1 || sum[0].(map[string]any)["text"] != "think" || sum[0].(map[string]any)["type"] != "summary_text" {
		t.Fatalf("reasoning summary=%v", r0)
	}
	if out[1].(map[string]any)["type"] != "message" {
		t.Fatalf("item1=%v", out[1])
	}
	if out[2].(map[string]any)["type"] != "function_call" {
		t.Fatalf("item2=%v", out[2])
	}
	fc := out[2].(map[string]any)
	if fc["call_id"] != "call_a" || fc["name"] != "Bash" {
		t.Fatalf("function_call=%v", fc)
	}
	u := got["usage"].(map[string]any)
	if u["input_tokens"] != 1 || u["output_tokens"] != 2 {
		t.Fatalf("usage=%v", u)
	}
}

func TestResponsesStreamEventOrder(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responsesWriter{ResponseWriter: rec, stream: true}
	rw.Header().Set("Content-Type", "text/event-stream")
	frames := []string{
		`data: {"id":"chatcmpl-1","model":"gpt-5.3-codex","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"Bash","arguments":"{\"x\":"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, f := range frames {
		if _, err := rw.Write([]byte(f)); err != nil {
			t.Fatal(err)
		}
	}
	rw.finish()
	types := sseTypes(rec.Body.String())
	if len(types) == 0 || types[0] != "response.created" {
		t.Fatalf("first=%v", types)
	}
	if types[len(types)-1] != "response.completed" {
		t.Fatalf("last=%v", types)
	}
	need := []string{
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_text.delta",
		"response.function_call_arguments.delta",
		"response.completed",
	}
	for _, n := range need {
		if !containsStr(types, n) {
			t.Fatalf("missing %s in %v", n, types)
		}
	}
	// 每个事件都带递增的 sequence_number（当前 Responses wire 契约，Codex 会读）。
	seqs := sseSequenceNumbers(rec.Body.String())
	if len(seqs) != len(types) {
		t.Fatalf("sequence_number 覆盖不全: seqs=%d events=%d %v", len(seqs), len(types), types)
	}
	for i, n := range seqs {
		if int(n) != i {
			t.Fatalf("sequence_number[%d]=%v want %d: %v", i, n, i, seqs)
		}
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"delta":"think"`) || !strings.Contains(body, `"delta":"hi"`) {
		t.Fatalf("deltas missing: %s", body)
	}
	if !strings.Contains(body, `"delta":"{\"x\":"`) || !strings.Contains(body, `"delta":"1}"`) {
		t.Fatalf("tool args missing: %s", body)
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Fatal("chat [DONE] must not leak")
	}
}

func TestResponsesEndpointNonStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, soloSSE, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":false}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.Bytes())
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["object"] != "response" || obj["status"] != "completed" {
		t.Fatalf("got=%v", obj)
	}
	out := obj["output"].([]any)
	found := false
	for _, it := range out {
		m := it.(map[string]any)
		if m["type"] == "message" {
			found = true
			content := m["content"].([]any)
			text := content[0].(map[string]any)["text"]
			if text != "你好" {
				t.Fatalf("text=%v", text)
			}
		}
	}
	if !found {
		t.Fatalf("no message in %v", out)
	}
}

func TestResponsesEndpointStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, soloSSE, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.Bytes())
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "event-stream") {
		t.Fatalf("ct=%s", rec.Header().Get("Content-Type"))
	}
	types := sseTypes(rec.Body.String())
	if len(types) == 0 || types[0] != "response.created" || types[len(types)-1] != "response.completed" {
		t.Fatalf("types=%v body=%s", types, rec.Body.String())
	}
	if !containsStr(types, "response.output_text.delta") {
		t.Fatalf("no text delta in %v", types)
	}
}

func TestResponsesEndpointStreamErrorBeforeSSE(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: &upstream.Client{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200 SSE", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "event-stream") {
		t.Fatalf("ct=%s body=%s", rec.Header().Get("Content-Type"), rec.Body.String())
	}
	types := sseTypes(rec.Body.String())
	if !containsStr(types, "response.failed") {
		t.Fatalf("types=%v body=%s", types, rec.Body.String())
	}
	if containsStr(types, "response.completed") {
		t.Fatalf("failed stream must not complete: %v", types)
	}
}

func sseTypes(body string) []string {
	var types []string
	for _, block := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var m map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) != nil {
				continue
			}
			if t, ok := m["type"].(string); ok {
				types = append(types, t)
			}
		}
	}
	return types
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// sseSequenceNumbers 收集每个事件的 sequence_number。
func sseSequenceNumbers(body string) []float64 {
	var seqs []float64
	for _, block := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var m map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) != nil {
				continue
			}
			if _, ok := m["type"]; !ok {
				continue
			}
			if n, ok := m["sequence_number"].(float64); ok {
				seqs = append(seqs, n)
			}
		}
	}
	return seqs
}

// TestResponsesStreamIncompleteOnLength 上游截断（finish_reason=length）不得伪装成
// response.completed——Codex 据此判断输出完整性，事件与 item 状态都要标 incomplete。
func TestResponsesStreamIncompleteOnLength(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responsesWriter{ResponseWriter: rec, stream: true}
	rw.Header().Set("Content-Type", "text/event-stream")
	frames := []string{
		`data: {"id":"chatcmpl-1","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"部分输出"}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, f := range frames {
		if _, err := rw.Write([]byte(f)); err != nil {
			t.Fatal(err)
		}
	}
	rw.finish()
	types := sseTypes(rec.Body.String())
	if len(types) == 0 || types[len(types)-1] != "response.incomplete" {
		t.Fatalf("last=%v", types)
	}
	if containsStr(types, "response.completed") {
		t.Fatalf("截断流不得报 completed: %v", types)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"status":"incomplete"`) {
		t.Fatalf("status missing: %s", body)
	}
	if !strings.Contains(body, `"incomplete_details":{"reason":"max_output_tokens"}`) {
		t.Fatalf("incomplete_details missing: %s", body)
	}
	// 应答对象 + message item（含 output 数组里的副本）至少两处标 incomplete。
	if n := strings.Count(body, `"status":"incomplete"`); n < 2 {
		t.Fatalf("item status incomplete 次数=%d body=%s", n, body)
	}
}

// TestResponsesStreamHoldsNamelessTool 无名 function_call 不是合法 Responses item：
// 名字到达前不宣告，宣告时把已攒参数一次性发出（relaykit 同款）。
func TestResponsesStreamHoldsNamelessTool(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responsesWriter{ResponseWriter: rec, stream: true}
	rw.Header().Set("Content-Type", "text/event-stream")
	frames := []string{
		`data: {"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","function":{"name":"Bash","arguments":"1}"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, f := range frames {
		if _, err := rw.Write([]byte(f)); err != nil {
			t.Fatal(err)
		}
	}
	rw.finish()
	body := rec.Body.String()
	if !strings.Contains(body, `"delta":"{\"a\":1}"`) {
		t.Fatalf("整段参数 delta missing: %s", body)
	}
	if !strings.Contains(body, `"arguments":"{\"a\":1}"`) || !strings.Contains(body, `"name":"Bash"`) {
		t.Fatalf("item done missing: %s", body)
	}
	if n := strings.Count(body, `"type":"function_call"`); n != 3 {
		t.Fatalf("function_call item 次数=%d want 3（added/done/最终 output）body=%s", n, body)
	}
}

// TestResponsesStreamDropsNamelessTool 全程无名的 tool_calls 视为无效：不宣告、不进 output。
func TestResponsesStreamDropsNamelessTool(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responsesWriter{ResponseWriter: rec, stream: true}
	rw.Header().Set("Content-Type", "text/event-stream")
	frames := []string{
		`data: {"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, f := range frames {
		if _, err := rw.Write([]byte(f)); err != nil {
			t.Fatal(err)
		}
	}
	rw.finish()
	body := rec.Body.String()
	if strings.Contains(body, "function_call") {
		t.Fatalf("无名调用泄漏: %s", body)
	}
	types := sseTypes(body)
	if types[len(types)-1] != "response.completed" {
		t.Fatalf("types=%v", types)
	}
}

func TestResponsesToChatInvalidJSON(t *testing.T) {
	_, _, err := responsesToChat([]byte(`{`))
	if err == nil {
		t.Fatal("want error")
	}
	h := NewHandler(Config{Pool: pool.New(""), Upstream: &upstream.Client{}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", bytes.NewReader([]byte(`{`))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

// TestUsageCacheFields 上游的缓存命中数必须透出到两种格式的 usage（此前一律 0，
// 客户端和面板都看不见缓存是否生效）。
func TestUsageCacheFields(t *testing.T) {
	up := map[string]any{
		"prompt_tokens":               float64(10000),
		"completion_tokens":           float64(5),
		"cache_read_input_tokens":     float64(8960),
		"cache_creation_input_tokens": float64(100),
	}
	au := anthropicUsage(up)
	if au["cache_read_input_tokens"] != float64(8960) {
		t.Fatalf("anthropic cache_read_input_tokens=%v want 8960", au["cache_read_input_tokens"])
	}
	if au["cache_creation_input_tokens"] != float64(100) {
		t.Fatalf("anthropic cache_creation_input_tokens=%v want 100", au["cache_creation_input_tokens"])
	}
	// Claude 口径：input_tokens **不含**缓存读/写（单独字段计）。旧断言钉的是
	// 「SOLO 直报、原样透传」（当时缺证据：prompt 是否含缓存读未实测）——2026-10-08
	// 由 responses.go 的实测记录定案：`cache_read=45 / prompt=45`，即 prompt_tokens
	// 含缓存读，故必须扣减，否则 Claude Code 的上下文占用被虚抬。
	// 10000 - (8960 + 100) = 940。
	if au["input_tokens"] != float64(940) {
		t.Fatalf("anthropic input_tokens=%v want 940（扣掉缓存读/写）", au["input_tokens"])
	}
	// 只报缓存读、没报 prompt 全量的极端 body：扣成负数要夹到 0，不许出现负上下文。
	if got := anthropicUsage(map[string]any{"cache_read_input_tokens": float64(50)})["input_tokens"]; got != float64(0) {
		t.Fatalf("扣减后 input_tokens=%v want 0（不为负）", got)
	}
	// 没有缓存字段的普通请求一字不动。
	if got := anthropicUsage(map[string]any{"prompt_tokens": float64(7)})["input_tokens"]; got != float64(7) {
		t.Fatalf("无缓存字段时 input_tokens=%v want 7（不动）", got)
	}
	if got := anthropicUsage(nil)["cache_read_input_tokens"]; got != nil {
		t.Fatalf("nil usage 不该凭空造缓存字段, got=%v", got)
	}
	d, _ := convertUsage(up)["input_tokens_details"].(map[string]any)
	if d == nil || d["cached_tokens"] != float64(8960) {
		t.Fatalf("responses input_tokens_details=%v want cached_tokens 8960", d)
	}

	chat, _, err := responsesToChat([]byte(`{"model":"m","prompt_cache_key":"sess-1","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(chat, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["prompt_cache_key"] != "sess-1" {
		t.Fatalf("prompt_cache_key 未透传: %v", obj["prompt_cache_key"])
	}
}

// commitTrackingWriter 模拟 net/http 的响应提交语义：Write/Flush 在未提交时
// 隐式 WriteHeader(200)；提交后的重复 WriteHeader 计数（= 线上那条
// "superfluous response.WriteHeader call" 警告的来源）。
type commitTrackingWriter struct {
	hdr         http.Header
	body        bytes.Buffer
	status      int
	committed   bool
	superfluous int
}

func newCommitTrackingWriter() *commitTrackingWriter {
	return &commitTrackingWriter{hdr: http.Header{}}
}

func (c *commitTrackingWriter) Header() http.Header { return c.hdr }
func (c *commitTrackingWriter) WriteHeader(code int) {
	if c.committed {
		c.superfluous++
		return
	}
	c.committed, c.status = true, code
}
func (c *commitTrackingWriter) Write(p []byte) (int, error) {
	if !c.committed {
		c.WriteHeader(http.StatusOK)
	}
	return c.body.Write(p)
}
func (c *commitTrackingWriter) Flush() {
	if !c.committed {
		c.WriteHeader(http.StatusOK)
	}
}

// TestResponsesEmitAfterFlushNoSuperfluousWriteHeader 流式路径先 Flush（上游逐帧
// flush 已隐式提交 200）再 emit：不得出现第二次 WriteHeader（net/http 会打
// superfluous 警告，线上实测噪音）。
func TestResponsesEmitAfterFlushNoSuperfluousWriteHeader(t *testing.T) {
	fw := newCommitTrackingWriter()
	w := &responsesWriter{ResponseWriter: fw, stream: true}
	w.Flush() // 模拟 StreamHint 在无输出帧后的 flush：提交 200
	if err := w.emit("response.created", map[string]any{"response": map[string]any{"id": "resp_1"}}); err != nil {
		t.Fatal(err)
	}
	if fw.superfluous != 0 {
		t.Fatalf("emit 二次 WriteHeader 次数=%d want 0", fw.superfluous)
	}
	if fw.status != http.StatusOK {
		t.Fatalf("status=%d want 200", fw.status)
	}
	if !strings.Contains(fw.body.String(), "event: response.created") {
		t.Fatalf("body=%q", fw.body.String())
	}
}

func TestResponsesOrphanFunctionCallOutput(t *testing.T) {
	msgsFor := func(input string) []any {
		t.Helper()
		got, _, err := responsesToChat([]byte(`{"model":"m","input":` + input + `}`))
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

	// 带图：孤儿回执必须整段保留 parts，前缀是独立 text part（图不能丢）。
	t.Run("image_parts_kept", func(t *testing.T) {
		m := msgsFor(`[{"type":"function_call_output","output":[
			{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}]}]`)
		if len(m) != 1 {
			t.Fatalf("len=%d %#v", len(m), m)
		}
		msg := m[0].(map[string]any)
		if msg["role"] != "user" {
			t.Fatalf("role=%v", msg["role"])
		}
		parts, ok := msg["content"].([]any)
		if !ok || len(parts) != 2 {
			t.Fatalf("content=%#v", msg["content"])
		}
		if p0 := parts[0].(map[string]any); !strings.HasPrefix(asString(p0["text"]), "[Message from another task") {
			t.Fatalf("prefix part=%v", p0)
		}
		p1 := parts[1].(map[string]any)
		if p1["type"] != "image_url" {
			t.Fatalf("image part=%v", p1)
		}
	})

	t.Run("text_only_flattens", func(t *testing.T) {
		m := msgsFor(`[{"type":"function_call_output","output":[{"type":"output_text","text":"done"}]}]`)
		msg := m[0].(map[string]any)
		if msg["role"] != "user" || !strings.Contains(asString(msg["content"]), "done") {
			t.Fatalf("got=%v", msg)
		}
	})

	t.Run("dict_output_serialized", func(t *testing.T) {
		m := msgsFor(`[{"type":"function_call_output","output":{"type":"output_text","text":"done"}}]`)
		msg := m[0].(map[string]any)
		if msg["role"] != "user" || !strings.Contains(asString(msg["content"]), "done") {
			t.Fatalf("got=%v", msg)
		}
	})

	// 有 call_id：仍是标准工具结果，一行不许变。
	t.Run("with_call_id_unchanged", func(t *testing.T) {
		m := msgsFor(`[{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":[{"type":"output_text","text":"done"}]}]`)
		if len(m) != 2 {
			t.Fatalf("len=%d %#v", len(m), m)
		}
		if m[0].(map[string]any)["role"] != "assistant" {
			t.Fatalf("msgs[0]=%v", m[0])
		}
		msg := m[1].(map[string]any)
		if msg["role"] != "tool" || msg["tool_call_id"] != "call_1" || msg["content"] != "done" {
			t.Fatalf("tool msg=%v", msg)
		}
	})
}

func TestResponsesCustomToolAndNamespace(t *testing.T) {
	src := []byte(`{"model":"m","stream":true,"input":"patch it","tools":[` +
		`{"type":"custom","name":"apply_patch","description":"Apply a patch","format":{"type":"grammar","definition":"start: patch"}},` +
		`{"type":"namespace","name":"codex_app","tools":[{"name":"list_threads","description":"list","parameters":{"type":"object"}}]}` +
		`]}`)
	chat, meta, err := responsesToChat(src)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(chat, &body); err != nil {
		t.Fatal(err)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools=%v want 2（namespace 应展开、custom 应降级）", tools)
	}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if tool["type"] != "function" {
			t.Fatalf("tool type=%v want function（上游只认扁平 function）", tool["type"])
		}
		fn, _ := tool["function"].(map[string]any)
		if fn == nil {
			t.Fatalf("tool 未按 {\"type\",\"function\"} 包裹：%v", tool)
		}
		params, _ := fn["parameters"].(map[string]any)
		props, _ := params["properties"].(map[string]any)
		if fn["name"] == "apply_patch" && props["input"] == nil {
			t.Fatalf("custom 工具应降级为单 input 参数：%v", tool)
		}
	}
	if !meta.isCustom("apply_patch") || meta.isCustom("list_threads") {
		t.Fatalf("customTools=%v want 只有 apply_patch", meta.customTools)
	}
	if meta.nsMap["list_threads"] != "codex_app" {
		t.Fatalf("nsMap=%v want list_threads -> codex_app", meta.nsMap)
	}

	// 回程（Codex 走的流式路径）：custom 还原成 custom_tool_call/input 事件，namespace 补回。
	rec := httptest.NewRecorder()
	rw := &responsesWriter{ResponseWriter: rec, stream: true, meta: meta}
	rw.Header().Set("Content-Type", "text/event-stream")
	frames := []string{
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"apply_patch","arguments":"{\"input\":\"PATCH\"}"}}]}}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"list_threads","arguments":"{}"}}]}}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, f := range frames {
		if _, err := rw.Write([]byte(f)); err != nil {
			t.Fatal(err)
		}
	}
	rw.finish()
	out := rec.Body.String()
	for _, want := range []string{
		"response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.done",
		`"input":"PATCH"`, // 回程已把 {"input":...} 还原成 freeform 原文
		`"namespace":"codex_app"`,
		"response.function_call_arguments.done", // 非 custom 工具仍走 function_call 路径
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("回程缺少 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, `"type":"custom_tool_call"`) && !strings.Contains(out, `"name":"apply_patch"`) {
		t.Fatal("custom 项应带 name")
	}
}

func sseItems(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, block := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var m map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) != nil {
				continue
			}
			if m["type"] != "response.output_item.done" {
				continue
			}
			if it, ok := m["item"].(map[string]any); ok {
				out = append(out, it)
			}
		}
	}
	return out
}

// driveStream 把 frames 喂给 responsesWriter，返回回程 body（Codex 走的流式路径）。
func driveStream(t *testing.T, meta *responsesMeta, frames ...string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	rw := &responsesWriter{ResponseWriter: rec, stream: true, meta: meta}
	rw.Header().Set("Content-Type", "text/event-stream")
	for _, f := range frames {
		if _, err := rw.Write([]byte(f)); err != nil {
			t.Fatal(err)
		}
	}
	rw.finish()
	return rec.Body.String()
}

// TestResponsesToolContractDetails 三条工具契约细节（对照 ithtelab/workbuddy-manager 的
// docs/namespace-compat.md，都是别人实测踩过的坑）：
//  1. namespace 子工具与顶层重名时不能丢弃 —— 重名者按稳定后缀改名，回程还原客户端原名，
//     出站方向（历史 function_call 与 tool_choice）反向映射成扁平名；
//  2. custom 载荷只有 {"input":"<string>"}（或裸 JSON 字符串）算可信，其余不当可执行调用；
//  3. 残缺 custom 调用（形态非法 / 无 finish_reason 与 [DONE] 的异常收尾）按 incomplete 下发。
func TestResponsesToolContractDetails(t *testing.T) {
	// ① 严格解包：非法形态一律不可信。
	for _, c := range []struct {
		in   string
		want string
		ok   bool
	}{
		{`{"input":"PATCH"}`, "PATCH", true},
		{`"PATCH"`, "PATCH", true},
		{`{"input":"PATCH","extra":1}`, "", false}, // 多键
		{`{"input":123}`, "", false},               // 非字符串
		{`{"input":""}`, "", false},                // 空载荷
		{`{"input":"PATCH"`, "", false},            // 半截 JSON（length 截断的典型形态）
		{`*** Begin Patch`, "", false},             // 未包裹的裸文本
		{"", "", false},
	} {
		got, ok := unwrapCustomInput(c.in)
		if got != c.want || ok != c.ok {
			t.Fatalf("unwrap(%q)=(%q,%v) want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}

	// ② 重名不丢弃 + 双向映射。
	src := []byte(`{"model":"m","stream":true,"tool_choice":"read_file","input":[` +
		`{"type":"function_call","call_id":"c0","name":"read_file","namespace":"codex_app","arguments":"{}"}],` +
		`"tools":[` +
		`{"type":"function","name":"read_file","description":"top","parameters":{"type":"object"}},` +
		`{"type":"namespace","name":"codex_app","tools":[{"name":"read_file","description":"in ns","parameters":{"type":"object"}}]}` +
		`]}`)
	chat, meta, err := responsesToChat(src)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(chat, &body); err != nil {
		t.Fatal(err)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools=%d want 2（重名不得丢弃）", len(tools))
	}
	var names []string
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		names = append(names, asString(fn["name"]))
	}
	if strings.Join(names, ",") != "read_file,read_file_2" {
		t.Fatalf("出站工具名=%v want [read_file read_file_2]", names)
	}
	if meta.nsMap["read_file_2"] != "codex_app" || meta.alias["read_file_2"] != "read_file" {
		t.Fatalf("nsMap=%v alias=%v", meta.nsMap, meta.alias)
	}
	// 历史与 tool_choice 用客户端原名：出站必须映射成我们声明的扁平名。
	msgs, _ := body["messages"].([]any)
	histName := ""
	for _, mm := range msgs {
		m, _ := mm.(map[string]any)
		tcs, _ := m["tool_calls"].([]any)
		for _, tr := range tcs {
			tc, _ := tr.(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			histName = asString(fn["name"])
		}
	}
	if histName != "read_file_2" {
		t.Fatalf("历史 function_call 名=%q want read_file_2（出站名）", histName)
	}
	if tc := asString(body["tool_choice"]); tc != "read_file_2" {
		t.Fatalf("tool_choice=%q want read_file_2（出站名）", tc)
	}

	// 回程：模型用出站名回名 → 必须还原成客户端原名，并带上 namespace。
	streamOut := driveStream(t, meta,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"read_file_2","arguments":"{}"}}]}}]}`+"\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n",
		"data: [DONE]\n\n",
	)
	items := sseItems(t, streamOut)
	if len(items) != 1 {
		t.Fatalf("流式 items=%v", items)
	}
	if got := asString(items[0]["name"]); got != "read_file" {
		t.Fatalf("流式回程 name=%q want read_file（客户端原名）", got)
	}
	if got := asString(items[0]["namespace"]); got != "codex_app" {
		t.Fatalf("流式回程 namespace=%q want codex_app", got)
	}

	// 非流式出口必须与流式一致（他们那次的 bug 就是两条出口名字不一致）。
	nsObj := map[string]any{
		"id": "c2", "model": "m",
		"choices": []any{map[string]any{
			"index": 0.0, "finish_reason": "tool_calls",
			"message": map[string]any{"tool_calls": []any{map[string]any{
				"id": "call_b", "function": map[string]any{"name": "read_file_2", "arguments": "{}"},
			}}},
		}},
	}
	nsOut, _ := chatCompletionToResponse(nsObj, meta)["output"].([]any)
	if len(nsOut) != 1 {
		t.Fatalf("非流式 output=%v", nsOut)
	}
	nsItem, _ := nsOut[0].(map[string]any)
	if asString(nsItem["name"]) != "read_file" || asString(nsItem["namespace"]) != "codex_app" {
		t.Fatalf("非流式出口与流式不一致：%v", nsItem)
	}

	// ③ custom 残缺 / 形态非法不得当成可执行调用。
	meta2 := &responsesMeta{customTools: map[string]bool{"apply_patch": true}}
	customFrame := func(args string) string {
		b, _ := json.Marshal(map[string]any{"id": "c3", "choices": []any{map[string]any{
			"index": 0.0, "delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": 0.0, "id": "call_patch",
				"function": map[string]any{"name": "apply_patch", "arguments": args},
			}}},
		}}})
		return "data: " + string(b) + "\n\n"
	}
	finishFrame := `data: {"id":"c3","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"
	lengthFrame := `data: {"id":"c3","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n"
	onlyItem := func(streamBody string) map[string]any {
		t.Helper()
		its := sseItems(t, streamBody)
		if len(its) != 1 {
			t.Fatalf("items=%v body=%s", its, streamBody)
		}
		return its[0]
	}

	// 正常收尾 + 合法载荷 → 可执行。
	okItem := onlyItem(driveStream(t, meta2, customFrame(`{"input":"PATCH"}`), finishFrame, "data: [DONE]\n\n"))
	if asString(okItem["status"]) != "completed" || asString(okItem["input"]) != "PATCH" {
		t.Fatalf("正常 custom 应为可执行：%v", okItem)
	}
	// length 截断（半截 JSON）→ incomplete，且不把半截当载荷。
	truncItem := onlyItem(driveStream(t, meta2, customFrame(`{"input":"PATCH`), lengthFrame, "data: [DONE]\n\n"))
	if asString(truncItem["status"]) != "incomplete" || asString(truncItem["input"]) != "" {
		t.Fatalf("截断的 custom 应按不可执行下发：%v", truncItem)
	}
	// 异常收尾（既无 finish_reason 也无 [DONE]）：载荷可信但收尾不可信 → incomplete。
	cutItem := onlyItem(driveStream(t, meta2, customFrame(`{"input":"PATCH"}`)))
	if asString(cutItem["status"]) != "incomplete" || asString(cutItem["input"]) != "PATCH" {
		t.Fatalf("异常收尾的 custom 应按不可执行下发：%v", cutItem)
	}

	// 非流式：合法载荷还原原文；截断的半截 JSON 不当作载荷。
	nsCustom := func(args, fr string) map[string]any {
		return map[string]any{
			"id": "c4", "model": "m",
			"choices": []any{map[string]any{
				"index": 0.0, "finish_reason": fr,
				"message": map[string]any{"tool_calls": []any{map[string]any{
					"id": "call_patch", "function": map[string]any{"name": "apply_patch", "arguments": args},
				}}},
			}},
		}
	}
	okNS, _ := chatCompletionToResponse(nsCustom(`{"input":"PATCH"}`, "tool_calls"), meta2)["output"].([]any)
	okIt, _ := okNS[0].(map[string]any)
	if asString(okIt["input"]) != "PATCH" || asString(okIt["status"]) != "completed" {
		t.Fatalf("非流式合法 custom：%v", okIt)
	}
	truncNS, _ := chatCompletionToResponse(nsCustom(`{"input":"PATCH`, "length"), meta2)["output"].([]any)
	truncIt, _ := truncNS[0].(map[string]any)
	if asString(truncIt["status"]) != "incomplete" || asString(truncIt["input"]) != "" {
		t.Fatalf("非流式截断 custom 应按不可执行下发：%v", truncIt)
	}
}

// 工具结果里的图片：必须挪出 tool 消息（Chat 的 tool 角色带 image_url 会被 OpenAI
// 直接 400），并攒到本轮工具结果**全部**落地之后 —— 中间插消息会让后面的 tool_call_id
// 失去应答，并行工具调用必然踩中。Codex 的 view_image 就是这么回图的。
// 口径与 workbuddy2api-panel 同款（那边先落地）：占位文本放在合成 user 消息的首个 part。
func TestResponsesToolOutputImageMoved(t *testing.T) {
	msgsFor := func(input string) []any {
		t.Helper()
		got, _, err := responsesToChat([]byte(`{"model":"m","input":` + input + `}`))
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
	const img = `{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}`
	const call = `{"type":"function_call","call_id":"c1","name":"view_image","arguments":"{}"}`

	t.Run("text_and_image", func(t *testing.T) {
		m := msgsFor(`[` + call + `,{"type":"function_call_output","call_id":"c1","output":[
			{"type":"output_text","text":"shot"},` + img + `]}]`)
		if got := roles(m); got != "assistant,tool,user" {
			t.Fatalf("角色序列=%s %#v", got, m)
		}
		if tool := m[1].(map[string]any); tool["content"] != "shot" {
			t.Fatalf("正文该留在 tool 消息里：%v", tool)
		}
		parts, ok := m[2].(map[string]any)["content"].([]any)
		if !ok || len(parts) != 2 {
			t.Fatalf("合成 user 消息=%#v", m[2])
		}
		if asString(parts[0].(map[string]any)["text"]) != toolImagePlaceholder {
			t.Fatalf("首个 part 该是占位文本：%v", parts[0])
		}
		p := parts[1].(map[string]any)
		if p["type"] != "image_url" || asString(p["image_url"].(map[string]any)["url"]) == "" {
			t.Fatalf("image part=%v", p)
		}
	})

	// 上一轮的图必须先于下一轮 assistant 落地：两轮一起钉「没丢」与「没插早」。
	t.Run("two_rounds_role_sequence", func(t *testing.T) {
		m := msgsFor(`[` + call + `,{"type":"function_call_output","call_id":"c1","output":[` + img + `]},
			{"type":"function_call","call_id":"c2","name":"shell","arguments":"{}"},
			{"type":"function_call_output","call_id":"c2","output":"ok"}]`)
		if got := roles(m); got != "assistant,tool,user,assistant,tool" {
			t.Fatalf("角色序列=%s %#v", got, m)
		}
	})

	t.Run("parallel_outputs_stay_adjacent", func(t *testing.T) {
		m := msgsFor(`[` + call + `,
			{"type":"function_call","call_id":"c2","name":"shell","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":[` + img + `]},
			{"type":"function_call_output","call_id":"c2","output":"ok"}]`)
		if got := roles(m); got != "assistant,tool,tool,user" {
			t.Fatalf("两条 tool 必须紧挨着，图在其后：%s %#v", got, m)
		}
	})

	// 纯文本路径逐字不变：不该给所有工具结果都补一条 user 消息。
	t.Run("text_only_no_extra_message", func(t *testing.T) {
		m := msgsFor(`[` + call + `,{"type":"function_call_output","call_id":"c1","output":"done"}]`)
		if got := roles(m); got != "assistant,tool" {
			t.Fatalf("角色序列=%s %#v", got, m)
		}
		if m[1].(map[string]any)["content"] != "done" {
			t.Fatalf("正文=%v", m[1])
		}
	})
}
