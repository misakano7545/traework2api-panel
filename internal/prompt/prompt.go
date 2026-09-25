// Package prompt 网关自有系统提示词：内置默认 + 文件覆盖 + 降级中性提示词。
//
// 背景：客户端（各类 CLI/编辑器插件）会在 system 里注入固定模板句，上游内容审核按逐字
// 精确匹配误杀合法流量。方案：出站前用网关自己的 system 替换客户端的 system/developer，
// 从源头消灭这一路指纹。用户/assistant 消息不动（那是用户内容，不做清洗）。
package prompt

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
)

//go:embed defaultprompt.md
var defaultPrompt string

// Degraded 降级提示词：误报处理用，刻意极简中性。
const Degraded = "You are a helpful assistant. Respond in the user's language, follow the user's instructions, and be direct and concise."

// Load 按 mode 与 file 加载系统提示词文本。
//   - file 非空 → 读文件（不存在/读失败返回 error，调用方 fail fast）；
//   - file 空 → 内置 defaultPrompt。
//
// mode 在这里只做透传记录：Load 只负责"拿到一段提示词文本"，路由语义由调用方决定。
func Load(mode, file string) (string, error) {
	if file == "" {
		return defaultPrompt, nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("prompt file %s: %w", file, err)
	}
	return string(raw), nil
}

// Rewrite 把请求体里的系统提示词换成网关自有的一条（custom 模式）：
//   - 删除 messages 中所有 role 为 system/developer 的消息；
//   - 在 messages 头部插入一条 {"role":"system","content":systemPrompt}；
//   - 其余字段与 user/assistant/tool 消息逐字不动。
//
// 解析失败 → 原样返回（绝不失败）：这是出站关键路径，坏请求交给上游按原语义处理。
func Rewrite(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		obj["messages"] = []any{map[string]any{"role": "system", "content": systemPrompt}}
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
		return body
	}
	kept := make([]any, 0, len(msgs)+1)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		role, _ := mm["role"].(string)
		if role == "system" || role == "developer" {
			continue
		}
		kept = append(kept, m)
	}
	obj["messages"] = append([]any{map[string]any{"role": "system", "content": systemPrompt}}, kept...)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// Append 在"开头连续 system/developer 块"之后插入网关自有 system（append 模式）：
// 客户端项目规范/工具约定与网关提示词并用，既有消息逐字不动。
//
// 判定显式同时匹配 system 与 developer：此时消息还没归一角色，漏判 developer 会把网关
// 提示词插到它前面，顺序语义就变了。
func Append(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		obj["messages"] = []any{map[string]any{"role": "system", "content": systemPrompt}}
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
		return body
	}
	insertAt := 0
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			break
		}
		role, _ := mm["role"].(string)
		if role != "system" && role != "developer" {
			break
		}
		insertAt++
	}
	gw := map[string]any{"role": "system", "content": systemPrompt}
	out := make([]any, 0, len(msgs)+1)
	out = append(out, msgs[:insertAt]...)
	out = append(out, gw)
	out = append(out, msgs[insertAt:]...)
	obj["messages"] = out
	raw, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return raw
}
