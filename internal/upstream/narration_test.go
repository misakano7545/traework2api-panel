// narration_test.go 预告句收尾的救援：判据必须窄（工具请求 + 尾部悬空 + 不在等用户
// 拍板），且每请求只救一次。
package upstream

import (
	"io"
	"strings"
	"testing"
)

// narrBody 一条典型的 agent 请求：声明了工具、历史里有工具轮（assistant tool_calls +
// tool 结果），模型刚看完工具输出。
const narrBody = `{"model":"glm-5.2","tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}],` +
	`"messages":[{"role":"user","content":"看下 a.go"},` +
	`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"Read","arguments":"{}"}}]},` +
	`{"role":"tool","tool_call_id":"c1","content":"package main"}]}`

// chatBody 闲聊客户端：挂了工具但历史里没有工具轮（这类客户端以冒号收尾是正常文风）。
const chatBody = `{"model":"glm-5.2","tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}],` +
	`"messages":[{"role":"user","content":"给我讲讲栈"}]}`

// narrSeg 正文停在一句预告上（冒号收尾），done 报 stop。
const narrSeg = "event:output\ndata:{\"response\":\"我先读一下这个文件：\",\"tool_calls\":null}\n\n" +
	"event:token_usage\ndata:{\"prompt_tokens\":10,\"completion_tokens\":30,\"total_tokens\":40}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

// TestNarrationRescuesSuspendedTail 尾部悬空 + 工具请求 → 补一次救援续写，正文接续、
// 只留一个 done。
func TestNarrationRescuesSuspendedTail(t *testing.T) {
	c, reqs := fakeCutUpstream(t, func(int) (int, string) { return 200, soloNextSeg })
	r := newTestContinueReader(t, c, narrBody, io.NopCloser(strings.NewReader(narrSeg)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if len(*reqs) != 1 {
		t.Fatalf("救援请求数=%d want 1", len(*reqs))
	}
	if !strings.Contains((*reqs)[0], "停在一句预告") {
		t.Fatalf("续写指令应是预告句救援那条：%s", (*reqs)[0])
	}
	if !strings.Contains((*reqs)[0], `"role":"assistant"`) {
		t.Fatalf("救援请求要带已输出正文作 assistant 前缀：%s", (*reqs)[0])
	}
	if !strings.Contains(s, "我先读一下这个文件：") || !strings.Contains(s, "继续") {
		t.Fatalf("两段正文都应在场：\n%s", s)
	}
	if n := strings.Count(s, "event:done"); n != 1 {
		t.Fatalf("done 事件数=%d want 1（首段 done 应被吞）：\n%s", n, s)
	}
}

// TestNarrationSkipsNonToolSession 历史里没有工具轮 → 不救（角色扮演/闲聊的正常文风）。
func TestNarrationSkipsNonToolSession(t *testing.T) {
	c, reqs := fakeCutUpstream(t, func(int) (int, string) { return 200, soloNextSeg })
	r := newTestContinueReader(t, c, chatBody, io.NopCloser(strings.NewReader(narrSeg)))
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("非工具会话不该救援，实发 %d 次请求", len(*reqs))
	}
}

// TestNarrationSkipsUserYield 正文在等用户拍板（「请确认后我再发送：」）→ 不救：
// 补一句「继续」会拿到工具调用并被采纳，等于替用户按下发送键。
func TestNarrationSkipsUserYield(t *testing.T) {
	seg := "event:output\ndata:{\"response\":\"邮件已写好，请确认后我再发送：\",\"tool_calls\":null}\n\n" +
		"event:token_usage\ndata:{\"prompt_tokens\":10,\"completion_tokens\":30,\"total_tokens\":40}\n\n" +
		"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"
	c, reqs := fakeCutUpstream(t, func(int) (int, string) { return 200, soloNextSeg })
	r := newTestContinueReader(t, c, narrBody, io.NopCloser(strings.NewReader(seg)))
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("等用户确认的收尾不该救援，实发 %d 次请求", len(*reqs))
	}
}

// TestNarrationSkipsCompleteTail 正常句号收尾 → 不救。
func TestNarrationSkipsCompleteTail(t *testing.T) {
	seg := "event:output\ndata:{\"response\":\"文件看完了，结论如上。\",\"tool_calls\":null}\n\n" +
		"event:token_usage\ndata:{\"prompt_tokens\":10,\"completion_tokens\":30,\"total_tokens\":40}\n\n" +
		"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"
	c, reqs := fakeCutUpstream(t, func(int) (int, string) { return 200, soloNextSeg })
	r := newTestContinueReader(t, c, narrBody, io.NopCloser(strings.NewReader(seg)))
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("说完的收尾不该救援，实发 %d 次请求", len(*reqs))
	}
}

// TestNarrationOnlyOnce 救援段自己又停在预告句上 → 不再救（每请求一次）。
func TestNarrationOnlyOnce(t *testing.T) {
	c, reqs := fakeCutUpstream(t, func(int) (int, string) { return 200, narrSeg })
	r := newTestContinueReader(t, c, narrBody, io.NopCloser(strings.NewReader(narrSeg)))
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("救援请求数=%d want 1（不得套娃）", len(*reqs))
	}
}

// TestNarrationSkipsMarkupComplete 完整写出来的文本化工具调用 → 不救：客户端会自己
// 解析执行它，再续写等于让模型发第二次调用。
func TestNarrationSkipsMarkupComplete(t *testing.T) {
	seg := "event:output\ndata:{\"response\":\"</function_calls>\",\"tool_calls\":null}\n\n" +
		"event:token_usage\ndata:{\"prompt_tokens\":10,\"completion_tokens\":30,\"total_tokens\":40}\n\n" +
		"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"
	c, reqs := fakeCutUpstream(t, func(int) (int, string) { return 200, soloNextSeg })
	r := newTestContinueReader(t, c, narrBody, io.NopCloser(strings.NewReader(seg)))
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("完整标记块不该救援，实发 %d 次请求", len(*reqs))
	}
}
