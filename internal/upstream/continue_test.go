package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"traework2api/internal/auth"
)

// SOLO 截断段：completion_tokens=32000（cap 特征）且 done 报 stop（上游对截断的伪装）。
const soloCutSeg = "event:metadata\ndata:{\"model\":\"glm-5.2\",\"session_id\":\"s1\"}\n\n" +
	"event:output\ndata:{\"response\":\"你好\",\"reasoning_content\":\"想\",\"tool_calls\":null}\n\n" +
	"event:token_usage\ndata:{\"prompt_tokens\":10,\"completion_tokens\":32000,\"total_tokens\":32010}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

// 续写段：自然收尾（completion 远小于 cap）。
const soloNextSeg = "event:output\ndata:{\"response\":\"继续\",\"tool_calls\":null}\n\n" +
	"event:token_usage\ndata:{\"prompt_tokens\":20,\"completion_tokens\":7,\"total_tokens\":27}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

// 正常段：未截断。
const soloNormalSeg = "event:output\ndata:{\"response\":\"你好\",\"tool_calls\":null}\n\n" +
	"event:token_usage\ndata:{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

// fakeCutUpstream 第 N 次调用（N=续写次数，段 1 手工喂入）返回脚本化 SOLO SSE，
// 并记录每次实际发出的请求体。
func fakeCutUpstream(t *testing.T, script func(call int) (int, string)) (*Client, *[]string) {
	t.Helper()
	reqs := &[]string{}
	call := 0
	c := testClient(rtFunc(func(r *http.Request) (*http.Response, error) {
		call++
		raw, _ := io.ReadAll(r.Body)
		*reqs = append(*reqs, string(raw))
		status, body := script(call)
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}))
	return c, reqs
}

func newTestContinueReader(t *testing.T, c *Client, body string, rc io.ReadCloser) *ContinueReader {
	t.Helper()
	a := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}
	return NewContinueReader(c, a, []byte(body), rc)
}

// TestContinueReaderCutContinues 截断（completion=32000 且 done=stop）→ 同模型续写；
// 客户端可见流：两段正文都在、只留一个 done、usage 为跨段累计。
func TestContinueReaderCutContinues(t *testing.T) {
	c, reqs := fakeCutUpstream(t, func(call int) (int, string) { return 200, soloNextSeg })
	r := newTestContinueReader(t, c, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(soloCutSeg)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if len(*reqs) != 1 {
		t.Fatalf("续写请求数=%d want 1", len(*reqs))
	}
	if !strings.Contains(s, "你好") || !strings.Contains(s, "继续") {
		t.Fatalf("两段正文都应在场:\n%s", s)
	}
	if n := strings.Count(s, "event:done"); n != 1 {
		t.Fatalf("done 事件数=%d want 1（首段 done 应被吞）:\n%s", n, s)
	}
	// usage 改写：最后一条 token_usage 为跨段累计（prompt 10+20 / completion 32000+7）。
	last := lastUsageLine(t, s)
	if last["completion_tokens"] != float64(32007) || last["prompt_tokens"] != float64(30) {
		t.Fatalf("合并 usage=%v want completion=32007 prompt=30", last)
	}
	// 续写请求（PrepareBody 后的 SOLO 方言）：同模型 + assistant 已输出内容 + 续写指令 + 原始消息。
	req := (*reqs)[0]
	for _, want := range []string{`"glm-5.2"`, "你好", "从中断处继续输出剩余内容", `"text":"hi"`} {
		if !strings.Contains(req, want) {
			t.Fatalf("续写请求缺少 %q:\n%s", want, req)
		}
	}
	if !strings.Contains(req, `"role":"assistant"`) {
		t.Fatalf("续写请求缺 assistant 消息:\n%s", req)
	}
}

// TestContinueReaderNormalNoContinue 未截断（completion < cap）→ 不续写，事件原样通过。
func TestContinueReaderNormalNoContinue(t *testing.T) {
	c, reqs := fakeCutUpstream(t, func(call int) (int, string) { return 200, soloNextSeg })
	r := newTestContinueReader(t, c, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(soloNormalSeg)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if len(*reqs) != 0 {
		t.Fatalf("续写请求数=%d want 0（未截断）", len(*reqs))
	}
	if !strings.Contains(s, `"response":"你好"`) || strings.Count(s, "event:done") != 1 {
		t.Fatalf("事件应原样通过:\n%s", s)
	}
}

// TestContinueReaderToolCutNoContinue 工具调用分片在场 → 不续写，done 原样通过。
func TestContinueReaderToolCutNoContinue(t *testing.T) {
	seg := "event:output\ndata:{\"response\":\"\",\"tool_calls\":[{\"index\":0,\"function_call\":{\"name\":\"Bash\",\"arguments\":\"{\\\"cmd\\\":\"}}]}\n\n" +
		"event:token_usage\ndata:{\"prompt_tokens\":10,\"completion_tokens\":32000,\"total_tokens\":32010}\n\n" +
		"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"
	c, reqs := fakeCutUpstream(t, func(call int) (int, string) { return 200, soloNextSeg })
	r := newTestContinueReader(t, c, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(seg)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("续写请求数=%d want 0（工具截断不续）", len(*reqs))
	}
	if strings.Count(string(out), "event:done") != 1 {
		t.Fatalf("done 应原样通过:\n%s", out)
	}
}

// TestContinueReaderExplicitLimitNoContinue 客户端显式输出限额 → 不续写。
func TestContinueReaderExplicitLimitNoContinue(t *testing.T) {
	c, reqs := fakeCutUpstream(t, func(call int) (int, string) { return 200, soloNextSeg })
	r := newTestContinueReader(t, c, `{"model":"glm-5.2","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(soloCutSeg)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("续写请求数=%d want 0（显式限额不续）", len(*reqs))
	}
	if strings.Count(string(out), "event:done") != 1 {
		t.Fatalf("done 应原样通过:\n%s", out)
	}
}

// TestContinueReaderSeg2FailureDegrades 续写请求失败 → 降级：done 回放、不挂死、
// 首段 usage（已改写为累计帧）仍在场。
func TestContinueReaderSeg2FailureDegrades(t *testing.T) {
	c, reqs := fakeCutUpstream(t, func(call int) (int, string) {
		return 500, `{"code":4008,"msg":"quota exceeded"}`
	})
	r := newTestContinueReader(t, c, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(soloCutSeg)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if len(*reqs) != 1 {
		t.Fatalf("续写请求数=%d want 1", len(*reqs))
	}
	if strings.Count(s, "event:done") != 1 {
		t.Fatalf("降级后 done 应回放:\n%s", s)
	}
	if last := lastUsageLine(t, s); last["completion_tokens"] != float64(32000) {
		t.Fatalf("首段 usage 应改写在场: %v", last)
	}
}

// TestContinueReaderCapAtMaxSegments 连续截断封顶：最多 1 次原生 + (maxContinueSegments-1) 次续写。
func TestContinueReaderCapAtMaxSegments(t *testing.T) {
	c, reqs := fakeCutUpstream(t, func(call int) (int, string) { return 200, soloCutSeg })
	r := newTestContinueReader(t, c, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(soloCutSeg)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != maxContinueSegments-1 {
		t.Fatalf("续写请求数=%d want %d（封顶）", len(*reqs), maxContinueSegments-1)
	}
	if strings.Count(string(out), "event:done") != 1 {
		t.Fatalf("封顶后应有且仅有一个 done:\n%s", out)
	}
}

// TestContinueReaderSeamReasoningDropped 续写段自带的 reasoning 对客户端剥离（仅累计）：
// 否则 responses 侧会开第二个 reasoning item，codex 报 OutputTextDelta without active item。
func TestContinueReaderSeamReasoningDropped(t *testing.T) {
	seg2 := "event:output\ndata:{\"response\":\"\",\"reasoning_content\":\"内部思考\",\"tool_calls\":null}\n\n" +
		"event:output\ndata:{\"response\":\"继续\",\"reasoning_content\":\"还想\",\"tool_calls\":null}\n\n" +
		"event:token_usage\ndata:{\"prompt_tokens\":20,\"completion_tokens\":7,\"total_tokens\":27}\n\n" +
		"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"
	c, _ := fakeCutUpstream(t, func(call int) (int, string) { return 200, seg2 })
	r := newTestContinueReader(t, c, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(soloCutSeg)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"reasoning_content":"想"`) {
		t.Fatalf("段1 思考应在场:\n%s", s)
	}
	if strings.Contains(s, "内部思考") || strings.Contains(s, "还想") {
		t.Fatalf("续写段思考不应外泄:\n%s", s)
	}
	if !strings.Contains(s, `"response":"继续"`) {
		t.Fatalf("续写段正文应在场:\n%s", s)
	}
}

// TestContinueReaderCapTolerance 实际到达值会略超 cap（kimi 实测 32003），>= 判定须容忍。
func TestContinueReaderCapTolerance(t *testing.T) {
	seg := strings.Replace(soloCutSeg, `"completion_tokens":32000`, `"completion_tokens":32003`, 1)
	c, reqs := fakeCutUpstream(t, func(call int) (int, string) { return 200, soloNextSeg })
	r := newTestContinueReader(t, c, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(seg)))
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("续写请求数=%d want 1（32003 也应判截断）", len(*reqs))
	}
}

// TestContinueReaderPerModelCap 输出硬上限按模型区分：glm-5.3 实测 16000（done 仍报
// stop），16000 必须判截断并续写；同值落在默认 cap（32000）的模型上则不判截断。
// 变体名（glm-5.3__max）与裸 config_name 两条键都要能查到。
func TestContinueReaderPerModelCap(t *testing.T) {
	seg53 := strings.Replace(soloCutSeg, `"completion_tokens":32000`, `"completion_tokens":16000`, 1)
	cases := []struct {
		name       string
		body       string
		wantReqs   int
		wantReason string
	}{
		{"裸名 glm-5.3", `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`, 1, "16000=cap 应续写"},
		{"变体名 glm-5.3__max", `{"model":"glm-5.3__max","messages":[{"role":"user","content":"hi"}]}`, 1, "剥后缀后=cap 应续写"},
		{"config_name 兜底", `{"config_name":"glm-5.3","model":"","messages":[]}`, 1, "裸 ID 键应命中"},
		{"默认 cap 模型 16000 不续", `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`, 0, "16000 < 32000 非截断"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, reqs := fakeCutUpstream(t, func(call int) (int, string) { return 200, soloNextSeg })
			r := newTestContinueReader(t, c, tc.body, io.NopCloser(strings.NewReader(seg53)))
			if _, err := io.ReadAll(r); err != nil {
				t.Fatalf("read: %v", err)
			}
			if len(*reqs) != tc.wantReqs {
				t.Fatalf("续写请求数=%d want %d（%s）", len(*reqs), tc.wantReqs, tc.wantReason)
			}
		})
	}
}

// TestContinueReaderExplicitLimitAboveCapContinues 显式限额 ≥ cap 是「上限而非目标」：
// max_completion_tokens=128000 的客户端（如 ZCode 按 128k 请求）不该因这个字段被关掉
// 续写——它在 16000/32000 处被截断并不是它想要的。
func TestContinueReaderExplicitLimitAboveCapContinues(t *testing.T) {
	c, reqs := fakeCutUpstream(t, func(call int) (int, string) { return 200, soloNextSeg })
	r := newTestContinueReader(t, c,
		`{"model":"glm-5.2","max_completion_tokens":128000,"messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(soloCutSeg)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("续写请求数=%d want 1（限额 ≥ cap 应续写）", len(*reqs))
	}
	if !strings.Contains(string(out), "继续") {
		t.Fatalf("续写段正文应在场:\n%s", out)
	}
}

// TestContinueReaderCumulativeLimitStops 限额 ≥ cap 时续写照常，但跨段累计到限额即止：
// max_tokens=64000 → 首段 32000（<64000，续）→ 段 2 再 32000（累计 64000 ≥ 64000，止）。
func TestContinueReaderCumulativeLimitStops(t *testing.T) {
	c, reqs := fakeCutUpstream(t, func(call int) (int, string) { return 200, soloCutSeg })
	r := newTestContinueReader(t, c,
		`{"model":"glm-5.2","max_tokens":64000,"messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(soloCutSeg)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if len(*reqs) != 1 {
		t.Fatalf("续写请求数=%d want 1（累计 64000 已到限额）", len(*reqs))
	}
	if strings.Count(s, "event:done") != 1 {
		t.Fatalf("到限额止后应有且仅有一个 done:\n%s", s)
	}
	if last := lastUsageLine(t, s); last["completion_tokens"] != float64(64000) {
		t.Fatalf("累计 usage=%v want completion=64000", last)
	}
}

// TestContinueReaderSegBudgetHardCap 段数随限额扩容但有硬顶：max_tokens=256000 /
// cap 32000 → ceil=8 → 段数硬顶 8（1 原生 + 7 续写），到第 8 段累计恰 256000 也到限额。
func TestContinueReaderSegBudgetHardCap(t *testing.T) {
	c, reqs := fakeCutUpstream(t, func(call int) (int, string) { return 200, soloCutSeg })
	r := newTestContinueReader(t, c,
		`{"model":"glm-5.2","max_tokens":256000,"messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(soloCutSeg)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != maxContinueSegmentsHard-1 {
		t.Fatalf("续写请求数=%d want %d（扩容硬顶）", len(*reqs), maxContinueSegmentsHard-1)
	}
	if strings.Count(string(out), "event:done") != 1 {
		t.Fatalf("封顶后应有且仅有一个 done:\n%s", out)
	}
}

// TestContinueReaderExplicitLimitBelowCapStillRespected 限额 < cap 的旧行为保持：
// max_tokens=4096 的客户端确实只想要这么多，截断在它眼里就是终点，不续。
func TestContinueReaderExplicitLimitBelowCapStillRespected(t *testing.T) {
	c, reqs := fakeCutUpstream(t, func(call int) (int, string) { return 200, soloNextSeg })
	r := newTestContinueReader(t, c,
		`{"model":"glm-5.2","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(soloCutSeg)))
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("续写请求数=%d want 0（限额 < cap 尊重意图）", len(*reqs))
	}
}

// lastUsageLine 取输出里最后一条 token_usage 的载荷。
func lastUsageLine(t *testing.T, s string) map[string]any {
	t.Helper()
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.HasPrefix(lines[i], "data:") {
			continue
		}
		var obj map[string]any
		payload := strings.TrimPrefix(lines[i], "data:")
		if json.Unmarshal([]byte(payload), &obj) != nil {
			continue
		}
		if _, ok := obj["completion_tokens"]; ok {
			return obj
		}
	}
	t.Fatalf("输出里没有 token_usage 帧:\n%s", s)
	return nil
}

// cutReader 先吐一段数据、再报一个非 EOF 的读错误（模拟上游中途断连）。
type cutReader struct {
	data string
	sent bool
	err  error
}

func (c *cutReader) Read(p []byte) (int, error) {
	if !c.sent {
		c.sent = true
		return copy(p, c.data), nil
	}
	return 0, c.err
}

func (c *cutReader) Close() error { return nil }

// 上游中途断连必须把读错误交给上层：吞掉就会被当成「正常收尾」——补一个 [DONE]、
// token_usage 已到就按成功记账，客户端拿到一条看似完整实则截断的回复。
func TestContinueReaderSurfacesTransportError(t *testing.T) {
	c, _ := fakeCutUpstream(t, func(int) (int, string) { return 200, "" })
	// 带 max_tokens → 显式限额不续写，测试不会去打假上游。
	body := `{"model":"glm-5.2","max_tokens":100,"messages":[]}`
	seg := "event:output\ndata:{\"response\":\"半句话\"}\n\n"
	r := newTestContinueReader(t, c, body, &cutReader{data: seg, err: io.ErrUnexpectedEOF})

	out, err := io.ReadAll(r)
	if err == nil || err == io.EOF {
		t.Fatalf("中途断连应把读错误抛给上层，实际 err=%v", err)
	}
	if !strings.Contains(string(out), "半句话") {
		t.Errorf("已写出的字节要先交完再报错，实际 %q", out)
	}
}
