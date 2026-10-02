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
