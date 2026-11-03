package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// concatToolCalls 按 OpenAI 客户端的口径累计流式 tool_calls：同 index 的字符串*拼接*。
// intl 的 gpt-5.2/5.4 会把首帧的 id+name 在末尾原样重发一遍，原样透传就会拼成
// "get_weatherget_weather"，Agent 侧再也匹配不上工具表。
func concatToolCalls(t *testing.T, body string) map[int]*struct{ id, name, args string } {
	t.Helper()
	out := map[int]*struct{ id, name, args string }{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("chunk 不是合法 JSON: %v → %s", err, data)
		}
		for _, ch := range chunk.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				c := out[tc.Index]
				if c == nil {
					c = &struct{ id, name, args string }{}
					out[tc.Index] = c
				}
				c.id += tc.ID
				c.name += tc.Function.Name
				c.args += tc.Function.Arguments
			}
		}
	}
	return out
}

// 国际版实测帧序（intl:gpt-5.2）：首帧给 id+name+空 arguments，中间只给 arguments 分片，
// 末尾**把首帧原样再来一次**。客户端按拼接累计，所以末尾那帧必须被吃掉。
func TestStreamToolCallNameNotDuplicated(t *testing.T) {
	const raw = `event:output
data:{"tool_calls":[{"function":{"arguments":"","name":"get_weather"},"id":"call_abc","index":0,"type":"function"}]}

event:output
data:{"tool_calls":[{"function":{"arguments":"{\"","name":""},"id":"","index":0,"type":""}]}

event:output
data:{"tool_calls":[{"function":{"arguments":"city","name":""},"id":"","index":0,"type":""}]}

event:output
data:{"tool_calls":[{"function":{"arguments":"\":\"Paris\"}","name":""},"id":"","index":0,"type":""}]}

event:output
data:{"tool_calls":[{"function":{"arguments":"","name":"get_weather"},"id":"call_abc","index":0,"type":"function"}]}

event:done
data:{"finish_reason":"stop"}

`
	rr := httptest.NewRecorder()
	if _, err := StreamWithError(rr, strings.NewReader(raw), nil, nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	body := rr.Body.String()
	calls := concatToolCalls(t, body)
	c, ok := calls[0]
	if !ok {
		t.Fatalf("没有下发 tool_calls: %q", body)
	}
	if c.name != "get_weather" {
		t.Fatalf("工具名被拼成 %q（重复下发没吃掉）", c.name)
	}
	if c.id != "call_abc" {
		t.Fatalf("id 被拼成 %q", c.id)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(c.args), &args); err != nil {
		t.Fatalf("arguments 拼错: %v → %q", err, c.args)
	}
	if args["city"] != "Paris" {
		t.Fatalf("arguments=%v", args)
	}
	// finish_reason 必须改成 tool_calls，否则 Agent 以为回合正常结束、不执行工具。
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("finish_reason 没修正: %q", body)
	}
	// 空 name/id/type 一律不该出现在下行帧里（严格客户端会把 name 赋成空串）。
	if strings.Contains(body, `"name":""`) || strings.Contains(body, `"id":""`) || strings.Contains(body, `"type":""`) {
		t.Errorf("仍在下发空字段: %q", body)
	}
}

// 累积式重发（后一帧是前一帧的超集）只补差值，拆片下发照旧拼接。
func TestStreamToolCallNameCumulativeResend(t *testing.T) {
	const raw = `event:output
data:{"tool_calls":[{"function":{"arguments":"{","name":"get_"},"id":"call_1","index":0}]}

event:output
data:{"tool_calls":[{"function":{"arguments":"","name":"get_weather"},"id":"call_1","index":0}]}

`
	rr := httptest.NewRecorder()
	if _, err := StreamWithError(rr, strings.NewReader(raw), nil, nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	c := concatToolCalls(t, rr.Body.String())[0]
	if c == nil || c.name != "get_weather" {
		t.Fatalf("累积式重发应按差值补: %q", c.name)
	}
}

// 流内 error 的 data 必须是 OpenAI 的**对象**信封：Agent 侧 SDK 只认 data 里的 error 键，
// 从前发的是裸字符串（data: "solo error code=4011 ..."），客户端一个 chunk 都解析不出，
// 于是「上游明确报错」在客户端表现成「模型返回空」。
func TestStreamErrorFrameIsOpenAIObject(t *testing.T) {
	const raw = "event:output\ndata:{\"response\":\"AAA\"}\n\n" +
		"event:error\ndata:{\"code\":4011,\"message\":\"exceeded the rate limit\"}\n\n"
	rr := httptest.NewRecorder()
	if _, err := StreamWithError(rr, strings.NewReader(raw), nil, nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	found := false
	for _, line := range strings.Split(rr.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" || strings.HasPrefix(data, "{\"choices\"") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("错误帧不是 JSON 对象: %q", data)
		}
		if e, ok := ev["error"].(map[string]any); ok {
			if !strings.Contains(e["message"].(string), "4011") {
				t.Errorf("错误帧 message=%v", e["message"])
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("没有下发 OpenAI 形状的错误帧: %q", rr.Body.String())
	}
}

// 并行工具调用时，上游会在同一帧里塞一条**只有 index 的空占位**（其余字段全是空串）。
// 清理空字段后它就成了 {"index":1}：OpenAI 官方 API 从不这么发，严格客户端（ZCode
// 用 zod 校验）看到 delta.tool_calls[i].function 缺失就判整帧非法、整轮失败
// —— issue #21 的 original 报错。占位条目不含任何信息，直接丢掉。
func TestStreamToolCallNoEmptyPlaceholderEntry(t *testing.T) {
	const raw = `event:output
data:{"tool_calls":[{"function":{"arguments":"}"},"index":1,"type":"function"},{"function":{"arguments":"","name":""},"id":"","index":1,"type":""},{"function":{"arguments":"","name":"delete_file"},"id":"call_2","index":2,"type":"function"}]}

event:done
data:{"finish_reason":"stop"}

`
	rr := httptest.NewRecorder()
	if _, err := StreamWithError(rr, strings.NewReader(raw), nil, nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	for _, line := range strings.Split(rr.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []map[string]any `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				if _, ok := tc["function"]; !ok {
					t.Fatalf("下发了没有 function 的 tool_calls 条目（严格客户端会判帧非法）: %s", data)
				}
			}
		}
	}
	if c := concatToolCalls(t, rr.Body.String())[2]; c == nil || c.name != "delete_file" {
		t.Fatalf("正常的并行条目被误删: %+v", c)
	}
}
