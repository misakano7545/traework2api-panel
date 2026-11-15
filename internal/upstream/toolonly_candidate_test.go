// toolonly_candidate_test.go 「说完了但其实该动手」的观测判据：只判形态、不改行为。
package upstream

import (
	"io"
	"strings"
	"testing"
)

// toolOnlyBody 最后一条是工具结果（agent 刚看完输出、本该继续动手）。
const toolOnlyBody = `{"model":"glm-5.2","tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}],` +
	`"messages":[{"role":"user","content":"查一下配置"},` +
	`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"Read","arguments":"{}"}}]},` +
	`{"role":"tool","tool_call_id":"c1","content":"port=7864"}]}`

// toolOnlyBodyNotLast 最后一条是 user（模型在回答提问，短答是正常的）。
const toolOnlyBodyNotLast = `{"model":"glm-5.2","tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}],` +
	`"messages":[{"role":"user","content":"查一下配置"},` +
	`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"Read","arguments":"{}"}}]},` +
	`{"role":"tool","tool_call_id":"c1","content":"port=7864"},` +
	`{"role":"user","content":"端口是多少？"}]}`

func TestToolOnlyCandidateShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		text string
		want bool
	}{
		{"句号短答 + 最后是工具结果", toolOnlyBody, "端口是 7864。", true},
		{"最后一条不是工具结果", toolOnlyBodyNotLast, "端口是 7864。", false},
		{"尾部悬空（归 narration）", toolOnlyBody, "我看一下配置文件：", false},
		{"在等用户拍板", toolOnlyBody, "要我现在改吗？", false},
		{"正文太短不算（空）", toolOnlyBody, "", false},
		{"正文超过长度闸门", toolOnlyBody, strings400(), false},
		{"闲聊请求（没有工具轮）", chatBody, "栈是后进先出。", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := toolOnlyCandidate([]byte(c.body), c.text); got != c.want {
				t.Fatalf("toolOnlyCandidate=%v want %v（text=%q）", got, c.want, c.text)
			}
		})
	}
}

// strings400 恰好达到长度闸门的一段正文。
func strings400() string {
	b := make([]byte, toolOnlyMaxBytes)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

// TestToolOnlyDoesNotChangeBehavior 观测层不得改写客户端可见的流：命中形态时仍然只有
// 一个 done、不发第二个上游请求。
func TestToolOnlyDoesNotChangeBehavior(t *testing.T) {
	seg := `event:output` + "\n" + `data:{"response":"端口是 7864。","tool_calls":null}` + "\n\n" +
		`event:token_usage` + "\n" + `data:{"prompt_tokens":900,"completion_tokens":8,"total_tokens":908}` + "\n\n" +
		`event:done` + "\n" + `data:{"finish_reason":"stop"}` + "\n\n"
	c, reqs := fakeCutUpstream(t, func(int) (int, string) { return 200, soloNextSeg })
	r := newTestContinueReader(t, c, toolOnlyBody, io.NopCloser(strings.NewReader(seg)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if len(*reqs) != 0 {
		t.Fatalf("观测层不该发起救援请求，实发 %d 次", len(*reqs))
	}
	if n := strings.Count(s, "event:done"); n != 1 {
		t.Fatalf("done 事件数=%d want 1", n)
	}
	if !strings.Contains(s, "端口是 7864。") {
		t.Fatalf("正文被改动：%q", s)
	}
}
