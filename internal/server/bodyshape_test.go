package server

import (
	"strings"
	"testing"
)

// 上游说「param is invalid」时只有形状能定位问题字段：键名、角色序列、
// 消息里的多余字段都必须在那一行日志里，否则只能靠猜。
func TestBodyShapeListsOddFields(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash__max","config_name":"deepseek-v4.1-flash",
		"function":"chat_v3","stream":true,
		"messages":[
			{"role":"system","content":"ZZSECRET"},
			{"role":"assistant","content":null,"tool_calls":[],"reasoning_content":""},
			{"role":"tool","tool_call_id":"c1","content":"ZZSECRET2"}
		]}`)
	got := bodyShape(body)
	for _, want := range []string{"keys=", "config_name", "function", "msgs=3",
		"assistant:1", "tool:1", "msg.reasoning_content:1", "内容块", "tools=[]"} {
		if !strings.Contains(got, want) {
			t.Errorf("形状里缺 %q：%s", want, got)
		}
	}
	if strings.Contains(got, "ZZSECRET") {
		t.Errorf("正文进了日志：%s", got)
	}
	if s := bodyShape([]byte("not json")); !strings.Contains(s, "不可解析") {
		t.Errorf("坏 body 要明说，实际 %q", s)
	}
}
