// responses.go Codex Responses 入站适配：POST /v1/responses ↔ 现有 chatCompletions。
// ponytail: 不存 previous_response_id / store；Codex 每轮带全量 input。
// 事件语义对齐 relaykit（new-api 拆出的协议转换模块，AGPL 仅作行为参考，未拷代码）：
// 推理走 reasoning_summary_* 事件族、截断如实报 response.incomplete、每事件带 sequence_number。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	chat, meta, err := responsesToChat(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(chat, &peek)
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(chat))
	r2.ContentLength = int64(len(chat))
	rw := &responsesWriter{ResponseWriter: w, stream: peek.Stream, meta: meta}
	defer rw.finish()
	h.chatCompletions(rw, r2)
}

func responsesToChat(src []byte) ([]byte, *responsesMeta, error) {
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return nil, nil, err
	}
	out := map[string]any{}
	meta := &responsesMeta{}
	for _, k := range []string{
		"model", "stream", "tool_choice", "max_tokens", "max_output_tokens",
		"max_completion_tokens", "temperature", "top_p", "user", "n", "stop",
		"metadata", "stream_options", "parallel_tool_calls",
		// ponytail: Codex 客户端按会话发 prompt_cache_key；丢掉就只能靠内容派生会话键。
		"prompt_cache_key",
	} {
		if v, ok := obj[k]; ok {
			out[k] = v
		}
	}
	if r, ok := obj["reasoning"].(map[string]any); ok {
		if e, ok := r["effort"]; ok {
			out["reasoning_effort"] = e
		}
	}
	if v, ok := obj["reasoning_effort"]; ok {
		out["reasoning_effort"] = v
	}
	if tools, ok := obj["tools"].([]any); ok {
		chatTools, custom, nsMap, alias := convertTools(tools)
		out["tools"] = chatTools
		meta.customTools, meta.nsMap, meta.alias = custom, nsMap, alias
	}
	// tool_choice 也要与出站工具名对齐：重名改名过的工具，客户端仍按原名点名。
	if v, ok := out["tool_choice"]; ok {
		out["tool_choice"] = meta.flatToolChoice(v)
	}
	msgs := []any{}
	if inst, ok := obj["instructions"].(string); ok && inst != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": inst})
	}
	switch in := obj["input"].(type) {
	case string:
		if in != "" {
			msgs = append(msgs, map[string]any{"role": "user", "content": in})
		}
	case []any:
		msgs = append(msgs, convertInputItems(in, meta)...)
	}
	out["messages"] = msgs
	b, err := json.Marshal(out)
	if err != nil {
		return nil, nil, err
	}
	return b, meta, nil
}

// responsesMeta 请求阶段提取、回程需要的工具语义（PR #109 的两条 Codex 协议细节）：
//   - customTools：客户端声明为 custom(freeform) 的工具名（如 Codex 的 apply_patch）。
//     上游没有 freeform 概念，只能降级成单 input 参数的 function 送上去，回程再还原
//     成 custom_tool_call —— 否则模型把载荷当普通文本吐出来，客户端永远收不到工具调用。
//   - nsMap：扁平工具名 -> namespace（新版 Codex App 用 namespace 声明 MCP/插件工具组）。
//     客户端是按 (name, namespace) 二元组派发执行器的，回程必须补回 namespace。
type responsesMeta struct {
	customTools map[string]bool
	nsMap       map[string]string
	// alias 出站扁平名 -> 客户端原名（只对重名改名过的有值）。
	// namespace 组内（或与顶层）重名时子工具被改成稳定后缀名（read_file_2），回程必须
	// 还原成客户端声明的原名，否则客户端下一轮按自己声明的名字回传工具结果就匹配不上；
	// 出站方向（历史 function_call / tool_choice）则要反着映射成扁平名。
	alias map[string]string
}

func (m *responsesMeta) isCustom(name string) bool { return m != nil && m.customTools[name] }

// flatName 反向映射：客户端原名 -> 出站扁平名（没改名过就原样返回）。
// 重名改名过的工具，原名不唯一（顶层 read_file 与 codex_app.read_file 都叫 read_file），
// 用 namespace 消歧；没有 ns 时按名字排序取第一个，保证同输入同结果。
func (m *responsesMeta) flatName(orig, ns string) string {
	if m == nil || len(m.alias) == 0 {
		return orig
	}
	best := ""
	for flat, o := range m.alias {
		if o != orig {
			continue
		}
		if ns != "" && m.nsMap[flat] == ns {
			return flat
		}
		if best == "" || flat < best {
			best = flat
		}
	}
	if best != "" {
		return best
	}
	return orig
}

// fixupItem 回程修正工具项：把模型用的出站名还原成客户端原名、并补回 namespace。
// 必须在 output_item.added 就调用：客户端从 added 事件派发执行器，补到 done 已错过时机。
func (m *responsesMeta) fixupItem(item map[string]any) {
	if m == nil || item == nil {
		return
	}
	name := asString(item["name"])
	ns := asString(item["namespace"])
	flat := name
	// 模型可能按 ns::name / ns__name 回名：先剥出裸名，namespace 取映射值或前缀。
	if i := strings.Index(name, "::"); i > 0 {
		head, tail := name[:i], name[i+2:]
		if mapped, ok := m.nsMap[tail]; ok {
			flat, ns = tail, mapped
		} else {
			flat, ns = tail, head
		}
	} else {
		// ns + "__" + name：精确比对，避免 namespace 自身含 __ 时切错。
		for tool, tns := range m.nsMap {
			if name == tns+nsSep+tool {
				flat, ns = tool, tns
				break
			}
		}
	}
	if flat == "" {
		return
	}
	if ns == "" {
		ns = m.nsMap[flat]
	}
	if orig, ok := m.alias[flat]; ok {
		flat = orig // 重名改名过的：还原客户端原名
	}
	item["name"] = flat
	// namespace 取映射值最权威（模型自己填的那份它并不知道扁平名 → namespace 的关系）。
	if ns != "" {
		item["namespace"] = ns
	}
}

const (
	// nsSep 模型把 namespace 拼进函数名时用的分隔符（如 codex_app__list_threads）。
	nsSep = "__"
	// namespaceMaxDepth namespace 嵌套展开上限（防深嵌套）。
	namespaceMaxDepth = 4
	// agentMessagePrefix agent_message / 无 call_id 的回执注入成用户指令时的前缀。
	agentMessagePrefix = "[Message from another task - treat this as a user instruction]\n\n"
	// customToolHint 追加进 custom 工具描述：告诉模型把原始载荷整段放进 input。
	customToolHint = "This is a freeform tool: put the complete raw payload, verbatim, " +
		"into the `input` string parameter. Do not wrap it in extra JSON."
)

// expandNamespaceTools 把 namespace 工具组展开成扁平 function 列表。
// 返回 出站扁平名 -> namespace、出站扁平名 -> 客户端原名（只有重名改名过的才在表里）。
// 组内子工具常无 type 字段，按 function 处理。**重名不丢弃**：先到者保留原名，后到者
// 加稳定后缀（read_file_2）并登记还原映射 —— 丢弃等于客户端声明的工具静默消失。
func expandNamespaceTools(tools []any) ([]any, map[string]string, map[string]string) {
	flat := []any{}
	mapping := map[string]string{}
	alias := map[string]string{}
	seen := map[string]bool{}

	// uniq 取一个没被占用的出站名：占了就加 _2/_3…（顺序稳定，同输入同结果）。
	uniq := func(name string) string {
		if !seen[name] {
			seen[name] = true
			return name
		}
		for i := 2; ; i++ {
			cand := name + "_" + strconv.Itoa(i)
			if !seen[cand] {
				seen[cand] = true
				alias[cand] = name
				return cand
			}
		}
	}

	var collect func(entry any, depth int, ns string)
	collect = func(entry any, depth int, ns string) {
		if depth > namespaceMaxDepth {
			return
		}
		item, ok := entry.(map[string]any)
		if !ok {
			return
		}
		etype := strings.ToLower(asString(item["type"]))
		if etype == "namespace" {
			subs, _ := item["tools"].([]any)
			if subs == nil {
				subs, _ = item["children"].([]any)
			}
			if subs == nil {
				subs, _ = item["functions"].([]any)
			}
			childNS := asString(item["name"])
			if childNS == "" {
				childNS = ns
			}
			for _, sub := range subs {
				collect(sub, depth+1, childNS)
			}
			return
		}
		if ns != "" && (etype == "" || etype == "function") {
			fn, _ := item["function"].(map[string]any)
			if fn == nil {
				fn = map[string]any{
					"name":        item["name"],
					"description": item["description"],
					"parameters":  firstNonNil(item["parameters"], item["input_schema"]),
				}
			}
			name := strings.TrimSpace(asString(fn["name"]))
			if name == "" {
				return
			}
			name = uniq(name)
			mapping[name] = ns
			params := fn["parameters"]
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			flatFn := map[string]any{
				"type":        "function",
				"name":        name,
				"description": asString(fn["description"]),
				"parameters":  params,
			}
			if s, ok := fn["strict"]; ok {
				flatFn["strict"] = s
			}
			flat = append(flat, flatFn)
			return
		}
		name := asString(item["name"])
		if name == "" {
			if fn, ok := item["function"].(map[string]any); ok {
				name = asString(fn["name"])
			}
		}
		if name != "" {
			out := uniq(name)
			if ns != "" {
				mapping[out] = ns
			}
			if out != name {
				setToolName(item, out) // 被重名挤出原名：改写出站名，回程按 alias 还原
			}
		}
		flat = append(flat, item)
	}

	for _, entry := range tools {
		collect(entry, 0, "")
	}
	return flat, mapping, alias
}

// setToolName 改写出站工具名：兼容 {"type":"function","function":{...}} 与裸 function 两种形态。
func setToolName(item map[string]any, name string) {
	if fn, ok := item["function"].(map[string]any); ok {
		fn["name"] = name
		return
	}
	item["name"] = name
}

// downgradeCustomTool 把 custom(freeform) 工具降级成单 input 参数的 Chat function。
// 只返回 function 体（name/description/parameters），由调用方按本仓约定包一层
// {"type":"function","function":...}。
func downgradeCustomTool(tool map[string]any) map[string]any {
	extra := ""
	if format, ok := tool["format"].(map[string]any); ok {
		if def := asString(format["definition"]); def != "" {
			extra = "\n\nGrammar:\n" + def
		}
	}
	return map[string]any{
		"name":        asString(tool["name"]),
		"description": strings.TrimSpace(asString(tool["description"]) + "\n\n" + customToolHint + extra),
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"input": map[string]any{
					"type":        "string",
					"description": "Complete raw payload for this tool, verbatim.",
				},
			},
			"required": []any{"input"},
		},
	}
}

// unwrapCustomInput 严格还原 freeform 载荷，返回 (载荷, 是否可信)。
//
// 只接受两种形态：`{"input": "<非空字符串>"}`（单键）与裸 JSON 字符串。其余
// （非法 JSON、多键、input 非字符串、空串）一律 ok=false —— custom 的载荷会被
// 客户端直接执行（apply_patch 就是打补丁），宁可让它看到失败，也不能把形态不明或
// 半截的内容当成可执行调用发出去。
func unwrapCustomInput(args string) (string, bool) {
	s := strings.TrimSpace(args)
	if s == "" {
		return "", false
	}
	var parsed any
	if json.Unmarshal([]byte(s), &parsed) != nil {
		return "", false
	}
	switch v := parsed.(type) {
	case string:
		if v == "" {
			return "", false
		}
		return v, true
	case map[string]any:
		if len(v) != 1 {
			return "", false
		}
		in, ok := v["input"].(string)
		if !ok || in == "" {
			return "", false
		}
		return in, true
	}
	return "", false
}

// marshalCustomInput 把回传的 freeform 输入打包成 Chat 工具参数（与 downgrade 对称）。
func marshalCustomInput(v any) string {
	s, ok := v.(string)
	if !ok {
		if v == nil {
			s = ""
		} else if b, err := json.Marshal(v); err == nil {
			s = string(b)
		}
	}
	b, err := json.Marshal(map[string]any{"input": s})
	if err != nil {
		return "{}"
	}
	return string(b)
}

func firstNonNil(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

// convertTools 把 Responses 工具定义转成上游 Chat 形态，并带出回程需要的语义
// （custom 工具名集合、出站名 -> namespace、出站名 -> 客户端原名）。
func convertTools(tools []any) ([]any, map[string]bool, map[string]string, map[string]string) {
	flat, nsMap, alias := expandNamespaceTools(tools)
	out := make([]any, 0, len(flat))
	custom := map[string]bool{}
	for _, t := range flat {
		m, ok := t.(map[string]any)
		if !ok {
			out = append(out, t)
			continue
		}
		if strings.EqualFold(asString(m["type"]), "custom") {
			if n := asString(m["name"]); n != "" {
				custom[n] = true
			}
			// 与下方普通 function 同款嵌套形状（上游只认 {"type","function"} 包裹）。
			out = append(out, map[string]any{"type": "function", "function": downgradeCustomTool(m)})
			continue
		}
		if _, ok := m["function"].(map[string]any); ok {
			out = append(out, m)
			continue
		}
		typ, _ := m["type"].(string)
		if typ == "" || typ == "function" {
			fn := map[string]any{}
			for _, k := range []string{"name", "description", "parameters", "strict"} {
				if v, ok := m[k]; ok {
					fn[k] = v
				}
			}
			out = append(out, map[string]any{"type": "function", "function": fn})
			continue
		}
		out = append(out, m)
	}
	return out, custom, nsMap, alias
}

// flatToolChoice tool_choice 点名了重名改名过的工具时，映射成出站扁平名。
func (m *responsesMeta) flatToolChoice(v any) any {
	if m == nil || len(m.alias) == 0 {
		return v
	}
	switch t := v.(type) {
	case string:
		return m.flatName(t, "")
	case map[string]any:
		ns := asString(t["namespace"])
		if n := asString(t["name"]); n != "" {
			t["name"] = m.flatName(n, ns)
		}
		if fn, ok := t["function"].(map[string]any); ok {
			if n := asString(fn["name"]); n != "" {
				fn["name"] = m.flatName(n, ns)
			}
		}
		return t
	}
	return v
}

func convertInputItems(items []any, meta *responsesMeta) []any {
	var out []any
	var pending []any
	flush := func() {
		if len(pending) == 0 {
			return
		}
		out = append(out, map[string]any{"role": "assistant", "content": "", "tool_calls": pending})
		pending = nil
	}
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		switch typ {
		case "reasoning", "item_reference":
			flush()
			continue
		case "function_call", "custom_tool_call":
			pending = append(pending, toToolCall(m, meta))
			continue
		case "function_call_output", "custom_tool_call_output":
			flush()
			if id := asString(m["call_id"]); id != "" {
				out = append(out, map[string]any{
					"role":         "tool",
					"tool_call_id": id,
					"content":      asContentString(m["output"]),
				})
				continue
			}
			// 无 call_id 的孤儿回执（来自其他任务）：同 agent_message 注入成用户指令，
			// 不伪装成工具结果——空 tool_call_id 到上游只会撞 11148 配对失败。
			// 带图时 convertContent 返回结构化 parts，必须整段保留：只取 text part 会把图
			// 悄悄丢掉（移植上游 #145，那里该分支按字符串处理，客户端每次重试都成裸 502）。
			c := convertContent(m["output"])
			if parts, ok := c.([]any); ok {
				out = append(out, map[string]any{
					"role": "user",
					"content": append([]any{map[string]any{
						"type": "text", "text": agentMessagePrefix,
					}}, parts...),
				})
				continue
			}
			if txt, _ := c.(string); strings.TrimSpace(txt) != "" {
				out = append(out, map[string]any{"role": "user", "content": agentMessagePrefix + txt})
			}
			continue
		case "agent_message":
			// 来自其他任务（子代理）的消息：按用户指令注入，别伪装成工具结果。
			flush()
			if txt := asContentString(m["content"]); strings.TrimSpace(txt) != "" {
				out = append(out, map[string]any{"role": "user", "content": agentMessagePrefix + txt})
			}
			continue
		}
		flush()
		role, _ := m["role"].(string)
		if role == "" {
			role = "user"
		}
		out = append(out, map[string]any{"role": role, "content": convertContent(m["content"])})
	}
	flush()
	return out
}

func toToolCall(m map[string]any, meta *responsesMeta) map[string]any {
	callID := asString(m["call_id"])
	if callID == "" {
		callID = asString(m["id"])
	}
	args := asString(m["arguments"])
	if asString(m["type"]) == "custom_tool_call" {
		// freeform 输入是整段字符串：打包成与降级工具对称的 {"input": ...}。
		args = marshalCustomInput(m["input"])
	}
	if args == "" {
		args = "{}"
	}
	// 历史里客户端用自己声明的原名；重名改名过的要映射成我们声明的出站名，否则对不上。
	// 客户端回传的 item 若带 namespace，用它消歧同名工具。
	return map[string]any{
		"id":   callID,
		"type": "function",
		"function": map[string]any{
			"name":      meta.flatName(asString(m["name"]), asString(m["namespace"])),
			"arguments": args,
		},
	}
}

func convertContent(c any) any {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var texts []string
		var parts []any
		hasNonText := false
		for _, p := range v {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "input_text", "output_text", "text", "":
				t := asString(m["text"])
				texts = append(texts, t)
				parts = append(parts, map[string]any{"type": "text", "text": t})
			case "input_image":
				hasNonText = true
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": imageURLFrom(m)},
				})
			}
		}
		if !hasNonText {
			return strings.Join(texts, "")
		}
		return parts
	default:
		return asContentString(c)
	}
}

func imageURLFrom(m map[string]any) string {
	switch u := m["image_url"].(type) {
	case string:
		return u
	case map[string]any:
		return asString(u["url"])
	}
	return asString(m["url"])
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func asContentString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, p := range t {
			if m, ok := p.(map[string]any); ok {
				b.WriteString(asString(m["text"]))
			}
		}
		return b.String()
	default:
		return asString(v)
	}
}

func chatCompletionToResponse(obj map[string]any, meta *responsesMeta) map[string]any {
	id, _ := obj["id"].(string)
	model, _ := obj["model"].(string)
	var created int64
	switch v := obj["created"].(type) {
	case float64:
		created = int64(v)
	case int64:
		created = v
	case int:
		created = int64(v)
	}
	var msg map[string]any
	fr := ""
	if chs, ok := obj["choices"].([]any); ok && len(chs) > 0 {
		if c, ok := chs[0].(map[string]any); ok {
			msg, _ = c["message"].(map[string]any)
			fr, _ = c["finish_reason"].(string)
		}
	}
	status, details := responsesStatusFromFinish(fr)
	output := []any{}
	if msg != nil {
		if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
			output = append(output, map[string]any{
				"id":      "rs_" + id,
				"type":    "reasoning",
				"status":  status,
				"summary": []any{map[string]any{"type": "summary_text", "text": rc}},
			})
		}
		content, _ := msg["content"].(string)
		tcs, _ := msg["tool_calls"].([]any)
		if content != "" || len(tcs) == 0 {
			output = append(output, map[string]any{
				"id":      "msg_" + id,
				"type":    "message",
				"status":  status,
				"role":    "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": content}},
			})
		}
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			fn, _ := tc["function"].(map[string]any)
			name, args := "", ""
			if fn != nil {
				name = asString(fn["name"])
				args = asString(fn["arguments"])
			}
			callID := asString(tc["id"])
			var item map[string]any
			if meta.isCustom(name) {
				// custom(freeform) 回程还原：载荷是整段字符串，不是 JSON 参数。
				// 形态非法（截断的半截 JSON / 多键 / 非字符串）时按 incomplete 下发：
				// 客户端不会执行未完成的调用，也不能拿半截补丁去打补丁。
				input, ok := unwrapCustomInput(args)
				itemStatus := status
				if !ok {
					log.Printf("WARN: [responses] custom 载荷形态非法，按不可执行下发 name=%s", name)
					input, itemStatus = "", "incomplete"
				}
				item = map[string]any{
					"id":      callID,
					"type":    "custom_tool_call",
					"call_id": callID,
					"name":    name,
					"input":   input,
					"status":  itemStatus,
				}
			} else {
				item = map[string]any{
					"id":        callID,
					"type":      "function_call",
					"call_id":   callID,
					"name":      name,
					"arguments": args,
					"status":    status,
				}
			}
			meta.fixupItem(item)
			output = append(output, item)
		}
	}
	out := map[string]any{
		"id":         respID(id),
		"object":     "response",
		"created_at": created,
		"status":     status,
		"model":      model,
		"output":     output,
	}
	if details != nil {
		out["incomplete_details"] = details
	}
	if u, ok := obj["usage"].(map[string]any); ok && u != nil {
		out["usage"] = convertUsage(u)
	}
	return out
}

func convertUsage(u map[string]any) map[string]any {
	out := map[string]any{}
	if v, ok := u["prompt_tokens"]; ok {
		out["input_tokens"] = v
	}
	if v, ok := u["completion_tokens"]; ok {
		out["output_tokens"] = v
	}
	if v, ok := u["total_tokens"]; ok {
		out["total_tokens"] = v
	}
	// ponytail: Responses 客户端的缓存读数在 input_tokens_details.cached_tokens
	// （Codex 据此显示 cached）。SOLO 上游的真实命中数在 cache_read_input_tokens
	// （实测 2026-09-28：cache_read=45 / prompt=45）；WorkBuddy 系上游给的是
	// prompt_tokens_details.cached_tokens，两个方言都认。没有该字段就不写，
	// 不拿 0 冒充「没命中」。
	if v, ok := u["cache_read_input_tokens"]; ok && v != nil {
		out["input_tokens_details"] = map[string]any{"cached_tokens": v}
	} else if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if c, ok := d["cached_tokens"]; ok {
			out["input_tokens_details"] = map[string]any{"cached_tokens": c}
		}
	}
	return out
}

func respID(chatID string) string {
	if chatID == "" {
		chatID = "wb2api"
	}
	if strings.HasPrefix(chatID, "resp_") {
		return chatID
	}
	return "resp_" + chatID
}

type responsesWriter struct {
	http.ResponseWriter
	stream  bool
	status  int
	hdrSent bool
	rest    []byte
	body    []byte
	x       streamState
	meta    *responsesMeta
}

type streamState struct {
	created, failed, completed bool
	// sawFinish/sawDone 上游正常收尾的两种标志：finish_reason 与 [DONE]。
	// 两者都没有就结束 = 异常 EOF / 断流，此时工具载荷不可信（见 closeOpenItems）。
	sawFinish, sawDone    bool
	id, model             string
	createdAt             int64
	outN                  int
	seq                   int // SSE 事件序号（sequence_number，当前 Responses wire 契约要求）
	status                string
	incompleteDetails     map[string]any
	reasoningOpen         bool
	reasoningIdx          int
	reasoningID           string
	reasoningText         strings.Builder
	messageOpen, textOpen bool
	messageIdx            int
	msgID                 string
	messageText           strings.Builder
	tools                 map[int]*toolAcc
	toolOrder             []int
	usage                 map[string]any
	output                []any
}

type toolAcc struct {
	idx, outIdx    int
	id, name       string
	args           strings.Builder
	opened, closed bool
	custom         bool // 客户端声明为 custom(freeform)：回程发 custom_tool_call 类 item
}

// toolDeltaEvent 按工具类型选流式增量事件名（custom 是 input 语义而非 arguments）。
func (w *responsesWriter) toolDeltaEvent(acc *toolAcc) string {
	if acc.custom {
		return "response.custom_tool_call_input.delta"
	}
	return "response.function_call_arguments.delta"
}

func (w *responsesWriter) Header() http.Header { return w.ResponseWriter.Header() }

func (w *responsesWriter) WriteHeader(code int) { w.status = code }

func (w *responsesWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *responsesWriter) Write(p []byte) (int, error) {
	if w.stream && strings.Contains(w.Header().Get("Content-Type"), "event-stream") {
		w.rest = append(w.rest, p...)
		for {
			i := bytes.Index(w.rest, []byte("\n\n"))
			if i < 0 {
				break
			}
			frame := string(bytes.TrimSpace(w.rest[:i]))
			w.rest = w.rest[i+2:]
			if err := w.handleFrame(frame); err != nil {
				return len(p), err
			}
		}
		return len(p), nil
	}
	w.body = append(w.body, p...)
	return len(p), nil
}

func (w *responsesWriter) finish() {
	if w.stream {
		if strings.Contains(w.Header().Get("Content-Type"), "event-stream") || w.hdrSent {
			if w.x.created && !w.x.completed && !w.x.failed {
				_ = w.closeOpenItems()
				_ = w.emitCompleted()
			}
			return
		}
		_ = w.emitJSONErrorAsFailed()
		return
	}
	w.flushJSON()
}

func (w *responsesWriter) flushJSON() {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	if len(w.body) == 0 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	var obj map[string]any
	if json.Unmarshal(w.body, &obj) != nil {
		w.ResponseWriter.WriteHeader(status)
		_, _ = w.ResponseWriter.Write(w.body)
		return
	}
	if _, hasErr := obj["error"]; hasErr {
		w.ResponseWriter.WriteHeader(status)
		_, _ = w.ResponseWriter.Write(w.body)
		return
	}
	raw, err := json.Marshal(chatCompletionToResponse(obj, w.meta))
	if err != nil {
		w.ResponseWriter.WriteHeader(status)
		_, _ = w.ResponseWriter.Write(w.body)
		return
	}
	w.ResponseWriter.WriteHeader(status)
	_, _ = w.ResponseWriter.Write(raw)
}

func (w *responsesWriter) emitJSONErrorAsFailed() error {
	var obj map[string]any
	_ = json.Unmarshal(w.body, &obj)
	return w.emitFailed(obj["error"])
}

func (w *responsesWriter) handleFrame(frame string) error {
	// SSE 注释帧（保活，`: keepalive`）：原样透传。吞掉它等于把网关的空闲保活一起吃掉，
	// 长思考期下游重新变静默，白加。客户端按 SSE 规范忽略注释，转发没有副作用。
	if strings.HasPrefix(frame, ":") {
		_, err := io.WriteString(w.ResponseWriter, frame+"\n\n")
		w.Flush()
		return err
	}
	if w.x.failed || w.x.completed {
		return nil
	}
	if strings.HasPrefix(frame, "data: [DONE]") {
		w.x.sawDone = true
		_ = w.closeOpenItems()
		return w.emitCompleted()
	}
	payload, ok := strings.CutPrefix(frame, "data: ")
	if !ok {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return nil
	}
	if errObj, ok := obj["error"]; ok {
		return w.emitFailed(errObj)
	}
	return w.handleChunk(obj)
}

func (w *responsesWriter) handleChunk(obj map[string]any) error {
	if !w.x.created {
		id, _ := obj["id"].(string)
		model, _ := obj["model"].(string)
		w.x.id = id
		w.x.model = model
		w.x.created = true
		if v, ok := obj["created"].(float64); ok && v > 0 {
			w.x.createdAt = int64(v)
		} else {
			w.x.createdAt = time.Now().Unix()
		}
		if err := w.emit("response.created", map[string]any{
			"response": map[string]any{
				"id":         respID(id),
				"object":     "response",
				"status":     "in_progress",
				"created_at": w.x.createdAt,
				"model":      model,
				"output":     []any{},
			},
		}); err != nil {
			return err
		}
	}
	if u, ok := obj["usage"].(map[string]any); ok && u != nil {
		w.x.usage = u
	}
	chs, _ := obj["choices"].([]any)
	if len(chs) == 0 {
		return nil
	}
	c, _ := chs[0].(map[string]any)
	if c == nil {
		return nil
	}
	if delta, _ := c["delta"].(map[string]any); delta != nil {
		if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
			if err := w.ensureReasoning(); err != nil {
				return err
			}
			w.x.reasoningText.WriteString(rc)
			if err := w.emit("response.reasoning_summary_text.delta", map[string]any{
				"item_id":       w.x.reasoningID,
				"output_index":  w.x.reasoningIdx,
				"summary_index": 0,
				"delta":         rc,
			}); err != nil {
				return err
			}
		}
		if txt, ok := delta["content"].(string); ok && txt != "" {
			if err := w.ensureMessageText(); err != nil {
				return err
			}
			w.x.messageText.WriteString(txt)
			if err := w.emit("response.output_text.delta", map[string]any{
				"item_id":       w.x.msgID,
				"output_index":  w.x.messageIdx,
				"content_index": 0,
				"delta":         txt,
			}); err != nil {
				return err
			}
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			if err := w.handleToolDeltas(tcs); err != nil {
				return err
			}
		}
	}
	if fr, ok := c["finish_reason"].(string); ok && fr != "" {
		w.x.sawFinish = true
		w.applyFinishReason(fr)
		return w.closeOpenItems()
	}
	return nil
}

// applyFinishReason 把上游截断如实映射成 incomplete（relaykit 同款语义）：
// max_output_tokens / content_filter 都不允许伪装成 completed，否则客户端
// （Codex）会把被截断的输出当作完整结果。
func (w *responsesWriter) applyFinishReason(fr string) {
	status, details := responsesStatusFromFinish(fr)
	if status != "" {
		w.x.status = status
		w.x.incompleteDetails = details
	}
}

func responsesStatusFromFinish(fr string) (string, map[string]any) {
	switch strings.TrimSpace(fr) {
	case "length", "max_tokens":
		return "incomplete", map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		return "incomplete", map[string]any{"reason": "content_filter"}
	default:
		return "completed", nil
	}
}

// itemStatus 关闭 item 时用的状态：截断时 item 也要标 incomplete。
func (w *responsesWriter) itemStatus() string {
	if w.x.status == "incomplete" {
		return "incomplete"
	}
	return "completed"
}

func (w *responsesWriter) nextIdx() int {
	i := w.x.outN
	w.x.outN++
	return i
}

func (w *responsesWriter) ensureReasoning() error {
	if w.x.reasoningOpen {
		return nil
	}
	w.x.reasoningIdx = w.nextIdx()
	w.x.reasoningID = "rs_" + w.x.id
	w.x.reasoningOpen = true
	if err := w.emit("response.output_item.added", map[string]any{
		"output_index": w.x.reasoningIdx,
		"item": map[string]any{
			"id":      w.x.reasoningID,
			"type":    "reasoning",
			"summary": []any{},
		},
	}); err != nil {
		return err
	}
	return w.emit("response.reasoning_summary_part.added", map[string]any{
		"item_id":       w.x.reasoningID,
		"output_index":  w.x.reasoningIdx,
		"summary_index": 0,
		"part":          map[string]any{"type": "summary_text"},
	})
}

func (w *responsesWriter) ensureMessageText() error {
	if err := w.closeReasoning(); err != nil {
		return err
	}
	if w.x.textOpen {
		return nil
	}
	if !w.x.messageOpen {
		w.x.messageIdx = w.nextIdx()
		w.x.msgID = "msg_" + w.x.id
		w.x.messageOpen = true
		if err := w.emit("response.output_item.added", map[string]any{
			"output_index": w.x.messageIdx,
			"item": map[string]any{
				"id":      w.x.msgID,
				"type":    "message",
				"status":  "in_progress",
				"role":    "assistant",
				"content": []any{},
			},
		}); err != nil {
			return err
		}
	}
	w.x.textOpen = true
	return w.emit("response.content_part.added", map[string]any{
		"item_id":       w.x.msgID,
		"output_index":  w.x.messageIdx,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": ""},
	})
}

func (w *responsesWriter) handleToolDeltas(tcs []any) error {
	if err := w.closeReasoning(); err != nil {
		return err
	}
	if err := w.closeMessage(); err != nil {
		return err
	}
	if w.x.tools == nil {
		w.x.tools = map[int]*toolAcc{}
	}
	for _, tci := range tcs {
		tc, _ := tci.(map[string]any)
		if tc == nil {
			continue
		}
		idx := 0
		if v, ok := tc["index"].(float64); ok {
			idx = int(v)
		}
		acc, ok := w.x.tools[idx]
		if !ok {
			acc = &toolAcc{idx: idx}
			w.x.tools[idx] = acc
			w.x.toolOrder = append(w.x.toolOrder, idx)
		}
		if id := asString(tc["id"]); id != "" {
			acc.id = id
		}
		fn, _ := tc["function"].(map[string]any)
		args := ""
		if fn != nil {
			if n := asString(fn["name"]); n != "" {
				acc.name = n
				acc.custom = w.meta.isCustom(n)
			}
			args = asString(fn["arguments"])
		}
		if args != "" {
			acc.args.WriteString(args)
		}
		// ponytail: 无名调用不是合法 Responses item——先攒着，等名字到了再宣告，
		// 宣告时把已攒参数作为整段 delta 一次性发出（relaykit 同款）。
		if !acc.opened {
			if acc.name == "" {
				continue
			}
			if err := w.openTool(acc); err != nil {
				return err
			}
			if acc.args.Len() > 0 {
				if err := w.emit(w.toolDeltaEvent(acc), map[string]any{
					"item_id":      acc.id,
					"output_index": acc.outIdx,
					"delta":        acc.args.String(),
				}); err != nil {
					return err
				}
			}
			continue
		}
		if args != "" {
			if err := w.emit(w.toolDeltaEvent(acc), map[string]any{
				"item_id":      acc.id,
				"output_index": acc.outIdx,
				"delta":        args,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *responsesWriter) openTool(acc *toolAcc) error {
	if acc.opened {
		return nil
	}
	acc.outIdx = w.nextIdx()
	if acc.id == "" {
		acc.id = "call_" + asString(acc.idx)
	}
	acc.opened = true
	item := map[string]any{
		"id":      acc.id,
		"status":  "in_progress",
		"call_id": acc.id,
		"name":    acc.name,
	}
	if acc.custom {
		item["type"], item["input"] = "custom_tool_call", ""
	} else {
		item["type"], item["arguments"] = "function_call", ""
	}
	// namespace 必须在 added 就带上：客户端从 added 事件派发执行器，补到 done 已太晚。
	w.meta.fixupItem(item)
	return w.emit("response.output_item.added", map[string]any{
		"output_index": acc.outIdx,
		"item":         item,
	})
}

func (w *responsesWriter) closeOpenItems() error {
	if err := w.closeReasoning(); err != nil {
		return err
	}
	if err := w.closeMessage(); err != nil {
		return err
	}
	for _, idx := range w.x.toolOrder {
		acc := w.x.tools[idx]
		if acc == nil || acc.closed || acc.name == "" {
			continue // 无名调用不是合法 item，丢弃
		}
		if !acc.opened {
			if err := w.openTool(acc); err != nil {
				return err
			}
			if acc.args.Len() > 0 {
				if err := w.emit(w.toolDeltaEvent(acc), map[string]any{
					"item_id":      acc.id,
					"output_index": acc.outIdx,
					"delta":        acc.args.String(),
				}); err != nil {
					return err
				}
			}
		}
		var item map[string]any
		if acc.custom {
			input, ok := unwrapCustomInput(acc.args.String())
			status := w.itemStatus()
			// 残缺的 custom 调用不作为可执行调用发出：delta 已经流出去的收不回来，
			// 用 item 的 status=incomplete 表达 —— 客户端不会执行未完成的调用。
			if !ok || !(w.x.sawFinish || w.x.sawDone) {
				log.Printf("WARN: [responses] custom 调用残缺（载荷可信=%v 正常收尾=%v），按不可执行下发 name=%s",
					ok, w.x.sawFinish || w.x.sawDone, acc.name)
				if !ok {
					input = ""
				}
				status = "incomplete"
			}
			if err := w.emit("response.custom_tool_call_input.done", map[string]any{
				"item_id":      acc.id,
				"output_index": acc.outIdx,
				"input":        input,
			}); err != nil {
				return err
			}
			item = map[string]any{
				"id": acc.id, "type": "custom_tool_call", "status": status,
				"call_id": acc.id, "name": acc.name, "input": input,
			}
		} else {
			if err := w.emit("response.function_call_arguments.done", map[string]any{
				"item_id":      acc.id,
				"output_index": acc.outIdx,
				"arguments":    acc.args.String(),
			}); err != nil {
				return err
			}
			item = map[string]any{
				"id":        acc.id,
				"type":      "function_call",
				"status":    w.itemStatus(),
				"call_id":   acc.id,
				"name":      acc.name,
				"arguments": acc.args.String(),
			}
		}
		w.meta.fixupItem(item)
		if err := w.emit("response.output_item.done", map[string]any{
			"output_index": acc.outIdx,
			"item":         item,
		}); err != nil {
			return err
		}
		w.x.output = append(w.x.output, item)
		acc.closed = true
	}
	return nil
}

func (w *responsesWriter) closeReasoning() error {
	if !w.x.reasoningOpen {
		return nil
	}
	text := w.x.reasoningText.String()
	if err := w.emit("response.reasoning_summary_text.done", map[string]any{
		"item_id":       w.x.reasoningID,
		"output_index":  w.x.reasoningIdx,
		"summary_index": 0,
		"text":          text,
	}); err != nil {
		return err
	}
	if err := w.emit("response.reasoning_summary_part.done", map[string]any{
		"item_id":       w.x.reasoningID,
		"output_index":  w.x.reasoningIdx,
		"summary_index": 0,
		"part":          map[string]any{"type": "summary_text", "text": text},
	}); err != nil {
		return err
	}
	// ponytail: Codex 消费 summary 事件族；聊天上游的 reasoning_content 无
	// 「摘要 vs 原文」之分，统一作为 summary 下发（relaykit 同款）。
	item := map[string]any{
		"id":      w.x.reasoningID,
		"type":    "reasoning",
		"status":  w.itemStatus(),
		"summary": []any{map[string]any{"type": "summary_text", "text": text}},
	}
	if err := w.emit("response.output_item.done", map[string]any{
		"output_index": w.x.reasoningIdx,
		"item":         item,
	}); err != nil {
		return err
	}
	w.x.output = append(w.x.output, item)
	w.x.reasoningOpen = false
	return nil
}

func (w *responsesWriter) closeMessage() error {
	if !w.x.messageOpen {
		return nil
	}
	if w.x.textOpen {
		if err := w.emit("response.output_text.done", map[string]any{
			"item_id":       w.x.msgID,
			"output_index":  w.x.messageIdx,
			"content_index": 0,
			"text":          w.x.messageText.String(),
		}); err != nil {
			return err
		}
		if err := w.emit("response.content_part.done", map[string]any{
			"item_id":       w.x.msgID,
			"output_index":  w.x.messageIdx,
			"content_index": 0,
		}); err != nil {
			return err
		}
		w.x.textOpen = false
	}
	item := map[string]any{
		"id":     w.x.msgID,
		"type":   "message",
		"status": w.itemStatus(),
		"role":   "assistant",
		"content": []any{
			map[string]any{"type": "output_text", "text": w.x.messageText.String()},
		},
	}
	if err := w.emit("response.output_item.done", map[string]any{
		"output_index": w.x.messageIdx,
		"item":         item,
	}); err != nil {
		return err
	}
	w.x.output = append(w.x.output, item)
	w.x.messageOpen = false
	return nil
}

func (w *responsesWriter) emitCompleted() error {
	if w.x.completed || w.x.failed {
		return nil
	}
	w.x.completed = true
	typ, status := "response.completed", "completed"
	if w.x.status == "incomplete" {
		typ, status = "response.incomplete", "incomplete"
	}
	resp := map[string]any{
		"id":         respID(w.x.id),
		"object":     "response",
		"status":     status,
		"created_at": w.x.createdAt,
		"model":      w.x.model,
		"output":     w.x.output,
	}
	if w.x.incompleteDetails != nil {
		resp["incomplete_details"] = w.x.incompleteDetails
	}
	if w.x.usage != nil {
		resp["usage"] = convertUsage(w.x.usage)
	}
	return w.emit(typ, map[string]any{"response": resp})
}

func (w *responsesWriter) emitFailed(errObj any) error {
	if w.x.failed {
		return nil
	}
	w.x.failed = true
	if errObj == nil {
		errObj = map[string]any{"message": "upstream error", "type": "api_error"}
	}
	return w.emit("response.failed", map[string]any{
		"response": map[string]any{
			"id":     respID(w.x.id),
			"object": "response",
			"status": "failed",
			"error":  errObj,
		},
	})
}

func (w *responsesWriter) emit(typ string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["type"] = typ
	// ponytail: 当前 Responses wire 契约要求每个事件带递增 sequence_number（Codex 会读）。
	payload["sequence_number"] = w.x.seq
	w.x.seq++
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if !w.hdrSent {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		// ponytail: 不显式 WriteHeader——下面的 io.WriteString 会隐式提交 200。
		// 流式路径的 Flush（StreamHint 逐帧 flush）可能已先提交，再显式调用就是
		// net/http 的 "superfluous response.WriteHeader call" 噪音。
		w.hdrSent = true
		w.status = http.StatusOK
	}
	if _, err := io.WriteString(w.ResponseWriter, "event: "+typ+"\ndata: "+string(raw)+"\n\n"); err != nil {
		return err
	}
	w.Flush()
	return nil
}
