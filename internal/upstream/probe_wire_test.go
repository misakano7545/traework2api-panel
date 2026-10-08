// probe_wire_test.go 线形探针：**只改请求体形状**，问上游三件事（2026-10-08 定缺用）。
//
//  1. `developer` 角色：Codex 系客户端会用；上游只认 system/user/assistant/tool 吗？
//  2. 孤儿 tool_call（assistant 带 tool_calls、后面没有对应 tool 结果）：上游 400 吗？
//     —— 若是，客户端写坏的会话历史会让之后每轮都 400，需要出站前清理。
//  3. `max_completion_tokens` 别名：上游认不认（认则无需翻译成 max_tokens）。
//     判定靠「上限是否真的生效」：给一个必然超长的题，8 的上限被尊重 → completion≤10。
//
// 手动跑：TW2A_PROBE_WIRE=1 go test ./internal/upstream -run TestProbeLiveWireShape -v
// 吃额度：4 次小请求（prompt≈hi，max≈8）。
package upstream

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"traework2api/internal/auth"
)

func TestProbeLiveWireShape(t *testing.T) {
	if os.Getenv("TW2A_PROBE_WIRE") == "" {
		t.Skip("置 TW2A_PROBE_WIRE=1 才跑（真实凭据 + 网络，吃额度）")
	}
	model := os.Getenv("TW2A_PROBE_MODEL")
	if model == "" {
		model = "glm-5.2"
	}
	canary := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"max_tokens":8}`, model)
	c, a := probeChat(t, canary)

	// 探测请求：只读 SSE 前 400 字节就掐（够看开头，不整条烧完输出），顺带留 usage 行。
	send := func(label, body string) {
		rc, status, respBody, err := c.ChatStream(a, []byte(body))
		if err != nil {
			fmt.Printf("----- %s\n  发出: %s\n  → 传输层错误: %v\n", label, body, err)
			return
		}
		if rc == nil {
			fmt.Printf("----- %s\n  发出: %s\n  → %d 拒绝，上游原话: %.300s\n", label, body, status, respBody)
			return
		}
		b, _ := io.ReadAll(io.LimitReader(rc, 400))
		rc.Close()
		out := string(b)
		fmt.Printf("----- %s\n  发出: %s\n  → %d 通过，SSE 前 400 字节: %s\n", label, body, status, out)
		if i := strings.Index(out, "token_usage"); i >= 0 {
			fmt.Printf("  usage 附近: %.200s\n", out[i:])
		}
	}

	fmt.Printf("\n===== 线形探针 model=%s uid=%s\n", model, a.UID)

	send("P1 developer 角色",
		fmt.Sprintf(`{"model":%q,"messages":[{"role":"developer","content":"be terse"},{"role":"user","content":"hi"}],"max_tokens":8}`, model))

	send("P2 孤儿 tool_call（无 tool 结果）",
		fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"},`+
			`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},`+
			`{"role":"user","content":"ok"}],"max_tokens":8}`, model))

	send("P3a max_completion_tokens=8（别名是否生效）",
		fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"count from 1 to 50, one number per line"}],"max_completion_tokens":8}`, model))

	send("P3b 对照 max_tokens=8",
		fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"count from 1 to 50, one number per line"}],"max_tokens":8}`, model))

	// 上限是否生效只能看实际产出量（前 400 字节里没有 usage）。
	sendFull(t, c, a, "P3a-full max_completion_tokens=8",
		fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"count from 1 to 50, one number per line"}],"max_completion_tokens":8}`, model))
	sendFull(t, c, a, "P3b-full max_tokens=8",
		fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"count from 1 to 50, one number per line"}],"max_tokens":8}`, model))
}

// sendFull 整条读完后打印末帧 token_usage 的 completion_tokens —— P3 的判据在这，
// 前 400 字节看不到（上限是否生效只能靠实际产出量）。
func sendFull(t *testing.T, c *Client, a *auth.Auth, label, body string) {
	t.Helper()
	rc, status, respBody, err := c.ChatStream(a, []byte(body))
	if err != nil {
		t.Logf("%s: 传输层错误 %v", label, err)
		return
	}
	if rc == nil {
		t.Logf("%s: %d 拒绝 %.300s", label, status, respBody)
		return
	}
	full, _ := io.ReadAll(rc)
	rc.Close()
	s := string(full)
	last := strings.LastIndex(s, `"completion_tokens":`)
	if last < 0 {
		t.Logf("%s: %d 通过但没看到 usage（%d 字节）", label, status, len(s))
		return
	}
	end := last + 200
	if end > len(s) {
		end = len(s)
	}
	t.Logf("%s: %d 通过，usage 片段: %s", label, status, s[last:end])
}
