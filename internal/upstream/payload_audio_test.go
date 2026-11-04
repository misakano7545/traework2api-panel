package upstream

import (
	"encoding/json"
	"testing"
)

// 上游只认 text / image_url：input_audio 实测回 4001（2026-11-04 astrbot 语音消息），
// 而 content 数组被剔空同样回 4001。三条都要守住：留下 text、留下 image_url、
// 剔掉 audio、全剔空时给占位而不是空数组。
func TestPrepareBodyDropsAudioPart(t *testing.T) {
	src := []byte(`{"model":"deepseek-v4.1-flash__max","messages":[
		{"role":"user","content":[
			{"type":"text","text":"这段语音说了什么"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}},
			{"type":"input_audio","input_audio":{"data":"QUJD","format":"wav"}}]},
		{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"QUJD","format":"wav"}}]}]}`)
	body := PrepareBody(src)
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	first := msgs[0].(map[string]any)["content"].([]any)
	if len(first) != 2 {
		t.Fatalf("第一条消息应剩 text+image_url 两块，实际 %d: %v", len(first), first)
	}
	if first[0].(map[string]any)["type"] != "text" || first[1].(map[string]any)["type"] != "image_url" {
		t.Errorf("留下的块类型不对: %v", first)
	}
	only := msgs[1].(map[string]any)["content"].([]any)
	if len(only) != 1 || only[0].(map[string]any)["text"] != nonTextPlaceholder {
		t.Errorf("只剩语音块的消息应换成占位文本，实际 %v（空数组上游回 4001）", only)
	}
}
