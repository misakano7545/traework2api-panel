package prompt

import (
	"encoding/json"
	"strings"
	"testing"
)

// custom 模式：客户端 system/developer 被替换成网关自己的那一条，其余消息逐字不动。
func TestRewriteReplacesSystem(t *testing.T) {
	body := []byte(`{"model":"m","temperature":0.5,"messages":[
		{"role":"system","content":"客户端模板"},
		{"role":"developer","content":"项目规范"},
		{"role":"user","content":"你好"}]}`)
	out := Rewrite(body, "网关提示词")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("应只剩 1 条 system + 1 条 user，got %d", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "网关提示词" {
		t.Fatalf("首条=%v", first)
	}
	if obj["temperature"] != 0.5 {
		t.Fatalf("其它字段被改: %v", obj["temperature"])
	}
	if !strings.Contains(string(out), "你好") {
		t.Fatal("user 消息丢了")
	}
	// 坏 JSON / 空提示词 → 原样返回（出站关键路径不允许失败）。
	if got := Rewrite([]byte("{not json"), "x"); string(got) != "{not json" {
		t.Fatalf("坏 JSON 应原样返回: %s", got)
	}
	if got := Rewrite(body, ""); string(got) != string(body) {
		t.Fatal("空提示词应原样返回")
	}
}

// append 模式：插在开头连续 system/developer 块之后，既有消息逐字保留。
func TestAppendKeepsExisting(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"system","content":"A"},
		{"role":"system","content":"B"},
		{"role":"user","content":"问"}]}`)
	out := Append(body, "网关")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("应插入 1 条，got %d", len(msgs))
	}
	got := []any{msgs[0].(map[string]any)["content"], msgs[1].(map[string]any)["content"],
		msgs[2].(map[string]any)["content"], msgs[3].(map[string]any)["content"]}
	want := []any{"A", "B", "网关", "问"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("顺序/内容错: %v，期望 %v", got, want)
		}
	}
}

// 内置默认提示词不能是空的（Load 空 file 的路径）。
func TestLoadBuiltin(t *testing.T) {
	text, err := Load("custom", "")
	if err != nil || strings.TrimSpace(text) == "" {
		t.Fatalf("内置提示词为空: %v", err)
	}
	if _, err := Load("custom", "/nonexistent/prompt.md"); err == nil {
		t.Fatal("文件不存在应报错（fail fast，不静默回落）")
	}
}
