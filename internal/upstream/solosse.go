// solosse.go SOLO 自定义 SSE 解析 → OpenAI SSE（流式转换 + 非流式聚合）。
//
// SOLO 事件序列（SPEC §4.6，实测）：
//
//	id:1
//	event:metadata
//	data:{"model":"","session_id":"...","prompt_completion_id":0,...}
//
//	id:2
//	event:timing_cost
//	data:{"name":"llm_raw_chat_v2",...}
//
//	event:output                          ← ×N，核心内容
//	data:{"response":"<content 增量>",
//	      "reasoning_content":"<思考链增量>",
//	      "tool_calls":<null 或工具调用>}
//
//	event:extra_info                       ← 含 reasoning_content 完整版
//	event:token_usage
//	data:{"prompt_tokens":21,"completion_tokens":142,"total_tokens":163,"reasoning_tokens":135}
//
//	event:done
//	data:{"finish_reason":"stop"}
package upstream

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrEmptyStream 上游 200 但整条流没产出任何模型事件（无 output、无 token_usage、无 error）。
// 这不是「模型回了空话」，而是上游空转了一次——调用方据此换号重试，而不是把空回复
// 当成成功写回客户端（照 autumnsentiment/Trae2api-cn 的「首个模型事件前空响应可重试一次」）。
var ErrEmptyStream = errors.New("upstream returned an empty stream")

// keepaliveInterval 下游保活间隔：上游长时间只思考不出字（或大模型首字极慢）时，
// 定期往下游写一个 SSE 注释帧，防中间盒（Cloudflare 空闲 ~100s）、客户端 idle 计时器把连接掐掉。
//
//	注释帧只对 chat 直通类客户端有效；codex 系按事件判活（stream_idle_timeout_ms），
//	那种情况要靠 reasoning 事件，注释帧救不了——这里也不是为它加的。
//
// 15s 是「远小于任何空闲阈值、又几乎不产生流量」的取值；0 = 关闭。变量而非常量只为测试能调小。
var keepaliveInterval = 15 * time.Second

// keepaliveLine SSE 注释帧（以 ':' 开头，客户端解析器一律忽略，不占事件通道）。
const keepaliveLine = ": keepalive\n\n"

// SOLOEvent 单条 SOLO SSE 事件（归一化）。
type SOLOEvent struct {
	Event        string          // metadata | timing_cost | output | extra_info | token_usage | done | error
	Response     string          // output: content 增量
	Reasoning    string          // output: 思考链增量
	ToolCalls    json.RawMessage // output: 工具调用（null 或对象/数组）
	Usage        map[string]any  // token_usage
	FinishReason string          // done
	ErrorCode    int64           // error
	ErrorMessage string          // error
}

// SOLOStreamError 上游 SSE 流内的业务错误（event:error）。非流式聚合时返回，
// 调用方可据此分类冷却账号并轮转。
type SOLOStreamError struct {
	Code int64
	Msg  string
}

func (e *SOLOStreamError) Error() string {
	return fmt.Sprintf("solo error code=%d msg=%s", e.Code, e.Msg)
}

// soloSpecificKind 只看**具体**判据（业务码 + 文案标记）；认不出返回 ok=false。
//
// 拆出来是给 HTTP 级 Classify 共用同一张表（README 一直写着「HTTP 级与 event:error
// 共用一张表」，代码里其实只有这张开关）。区间/兜底**不算**具体判据：HTTP 4xx 的 body
// 里出现一个认不出的业务码时，该按 HTTP 状态归 Client，不能落到流内那套「认不出就按
// 上游故障罚号」的兜底（把 11101 bad param 罚成 ErrServer 会白喂熔断）。
func soloSpecificKind(code int64, msg string) (ErrKind, bool) {
	lower := strings.ToLower(msg)
	switch {
	// 1005 plan 权益不足。文字判据只在**没有结构化业务码**时才用（code==0）：否则
	// 111005 这种「数字里含 1005」的未知码会被误判成 12h 硬冷却（Classify 现在会无条件查这张表）。
	case code == 1005 || (code == 0 && strings.Contains(lower, "plan")):
		return ErrPlanLimit, true
	// 模型配置为空 / 参数非法是**模型**问题，不是账号问题，罚号没有意义。
	// 实测：function 给错通道时上游回 4001 "the param is invalid"，
	// 旧口径按罚号计数，一次客户端参数错误就把好号推向熔断。
	case code == 4001 || strings.Contains(lower, "model config is empty"):
		return ErrNone, true
	// 4011 = 通道级速率/额度限制（实测 2026-09-28）：同一账号在 coder 通道被 4011 拒的同时，
	// Work 通道立刻照常出正文。罚整个账号会让另一条通道的模型跟着躺 60 秒，所以不罚号。
	case code == 4011:
		return ErrNone, true
	// 4026 = 上下文超长（实测：dev 窗口 232768 的模型发 ~250K token 回
	// {"code":4026,"message":"We're sorry, your context length has exceeded the maximum limit."}）。
	// 这是**调用方**的问题，不是账号的：老口径靠下面那条 "exceeded" 子串把它归成 ErrSoftRate，
	// 于是一个客户端发超长 prompt 就给好号上软冷却（60s 起、指数到 2h）；它一重试，
	// 轮转过的每个号都跟着躺下，几次就能把整池冻住。放在 4008/quota 之前，别被 "exceeded" 抢走。
	case code == 4026 || strings.Contains(lower, "context length"):
		return ErrNone, true
	// 4008/quota 走**软冷却**（README 有实测政策），不是 12h 计划冷却：它是可自愈的限流，
	// 不是权益不足。
	case code == 4008 || strings.Contains(lower, "quota") ||
		strings.Contains(lower, "exceeded") || strings.Contains(lower, "rate"):
		return ErrSoftRate, true
	case code == 401:
		return ErrSessionDead, true
	case code == 429:
		return ErrSoftRate, true
	case code == 404:
		return ErrNotFound, true
	}
	return ErrNone, false
}

// Kind 将 SSE 流内错误分类，口径照参考实现 trae-workbuddy-switch 的 classify_solo：
// 先看具体判据（业务码 + 文案，见 soloSpecificKind），认不出才按数字区间归类。
// 顺序有讲究 —— 把 1005 当 Client 会让一个额度耗尽的号在几十分钟后被反复重试；
// 把业务码（1005/4001/4023…）按 `>= 500` 之类的区间吞掉，则会把最需要单独识别的一类
// 全归成 Server。流内只有业务码可看，所以这里的兜底比 HTTP 级狠（照旧：认不出的码
// 按上游侧故障处理）。
func (e *SOLOStreamError) Kind() ErrKind {
	if k, ok := soloSpecificKind(e.Code, e.Msg); ok {
		return k
	}
	switch {
	// 只有真正的 HTTP 状态码区间才映射为 Client / Server（显式写区间上界）。
	case e.Code >= 400 && e.Code < 500:
		return ErrClient
	case e.Code >= 500 && e.Code < 600:
		return ErrServer
	case e.Code == 0:
		return ErrNone
	default:
		// 认不出的业务码按「上游侧故障」处理：与旧口径（一律罚号计数）一致，也与参考实现
		// 把 BusinessError 映射成 300s 冷却同向。只有明确与账号无关的码才不罚号。
		return ErrServer
	}
}

// ParseSOLOLine 解析一条事件（eventName 为 event 行值，dataLine 为 data 行值）。
func ParseSOLOLine(eventName, dataLine string) (*SOLOEvent, error) {
	ev := &SOLOEvent{Event: strings.TrimSpace(eventName)}
	if dataLine == "" {
		return ev, nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(dataLine), &raw); err != nil {
		return nil, err
	}
	switch ev.Event {
	case "output":
		if v, ok := raw["response"].(string); ok {
			ev.Response = v
		}
		if v, ok := raw["reasoning_content"].(string); ok {
			ev.Reasoning = v
		}
		if tc, ok := raw["tool_calls"]; ok {
			ev.ToolCalls, _ = json.Marshal(tc)
		}
	case "token_usage":
		ev.Usage = raw
		mirrorOpenAIUsage(raw)
	case "done":
		if v, ok := raw["finish_reason"].(string); ok {
			ev.FinishReason = v
		}
	case "error":
		if v, ok := raw["code"].(float64); ok {
			ev.ErrorCode = int64(v)
		}
		if v, ok := raw["message"].(string); ok {
			ev.ErrorMessage = v
		}
	}
	return ev, nil
}

// mirrorOpenAIUsage 给 SOLO 的 token_usage 补上 OpenAI 兼容层读的嵌套字段。
// 上游只报顶层 cache_read_input_tokens / reasoning_tokens，客户端读的是
// prompt_tokens_details.cached_tokens / completion_tokens_details.reasoning_tokens，
// 不补就一律显示缓存未命中（issue #3）。就地改写：raw 是每帧新建的 map。
// 上游报了 0 也照写——0 是「量过、没命中」，缺字段是「没量」。
func mirrorOpenAIUsage(u map[string]any) {
	if u == nil {
		return
	}
	if c, ok := u["cache_read_input_tokens"].(float64); ok {
		u["cached_tokens"] = c
		u["prompt_cache_hit_tokens"] = c
		u["prompt_tokens_details"] = map[string]any{"cached_tokens": c}
	}
	if r, ok := u["reasoning_tokens"].(float64); ok {
		u["completion_tokens_details"] = map[string]any{"reasoning_tokens": r}
	}
}

// sseState 维护一行 SSE 的 event/data 跨行累积。
type sseState struct {
	event string
	data  strings.Builder
}

// reset 清空状态（事件边界触发）。
func (s *sseState) reset() {
	s.event = ""
	s.data.Reset()
}

// scanLine 处理一行；返回该行触发的事件（事件边界时解析并返回）。
func scanLine(st *sseState, line string) *SOLOEvent {
	switch {
	case line == "":
		if st.event == "" {
			st.reset()
			return nil
		}
		ev, err := ParseSOLOLine(st.event, st.data.String())
		st.reset()
		if err != nil {
			return nil
		}
		return ev
	case strings.HasPrefix(line, "event:"):
		st.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	case strings.HasPrefix(line, "data:"):
		st.data.WriteString(strings.TrimPrefix(line, "data:"))
	case strings.HasPrefix(line, ":"):
		// 注释行忽略
	}
	return nil
}

// maxSSELine 单行上限。ReadString 遇到永不换行的坏帧会一直往内存里堆，
// 真实帧最大也就 tool_calls 参数级别（几百 KB），8 MB 足够。
const maxSSELine = 8 << 20

// sseLine 读一行（含换行符），语义同 bufio.ReadString('\n')，但单行超过 maxSSELine 时报错而不是继续堆内存。
func sseLine(br *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		chunk, err := br.ReadSlice('\n')
		if sb.Len()+len(chunk) > maxSSELine {
			return "", fmt.Errorf("SSE 单行超过 %d 字节，判流坏", maxSSELine)
		}
		sb.Write(chunk)
		if err == nil {
			return sb.String(), nil
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return sb.String(), err // io.EOF 等：连已读部分一起返回，调用方语义不变
	}
}

// Aggregate 读取完整 SOLO SSE，聚合 response + reasoning + tool_calls + usage，
// 产出单个 OpenAI chat.completion（非流式）。
func Aggregate(r io.Reader) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id           string
		content      strings.Builder
		reasoning    strings.Builder
		finishReason = "stop"
		usage        map[string]any
		toolCalls    = map[int]map[string]any{}
		toolOrder    []int
		upstreamErr  error
	)
	st := &sseState{}
	for {
		line, err := sseLine(br)
		if err != nil && err != io.EOF {
			return nil, err
		}
		if ev := scanLine(st, strings.TrimRight(line, "\r\n")); ev != nil {
			switch ev.Event {
			case "metadata":
				if id == "" && ev.Usage != nil {
					// metadata 无 id 可用，保留为空，末尾补 chatcmpl。
				}
			case "output":
				content.WriteString(ev.Response)
				reasoning.WriteString(ev.Reasoning)
				mergeToolCallJSON(toolCalls, &toolOrder, ev.ToolCalls)
			case "token_usage":
				usage = ev.Usage
			case "done":
				if ev.FinishReason != "" {
					finishReason = ev.FinishReason
				}
			case "error":
				upstreamErr = &SOLOStreamError{Code: ev.ErrorCode, Msg: ev.ErrorMessage}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if upstreamErr != nil {
		return nil, upstreamErr
	}
	// 空流：一个模型事件都没有（无 output/usage/error）→ 上游空转，交给调用方换号重试。
	if content.Len() == 0 && reasoning.Len() == 0 && len(toolOrder) == 0 && usage == nil {
		return nil, ErrEmptyStream
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	message := map[string]any{
		"role":    "assistant",
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sortInts(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
		// 同一处上游伪装，聚合路径也要改（见 streamOpts 的 done 分支）：
		// 非流式客户端拿到 stop 同样不会执行工具，只修流式等于没修一半。
		if finishReason == "" || finishReason == "stop" {
			finishReason = "tool_calls"
		}
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "",
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// mergeToolCallJSON 把 SOLO output.tool_calls（json.RawMessage，可能 null/对象/数组）
// 合并进 toolCalls（按 index）。
func mergeToolCallJSON(toolCalls map[int]map[string]any, toolOrder *[]int, raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		var one map[string]any
		if json.Unmarshal(raw, &one) != nil {
			return
		}
		arr = []map[string]any{one}
	}
	for _, call := range arr {
		if call == nil {
			continue
		}
		idx := 0
		if v, ok := call["index"].(float64); ok {
			idx = int(v)
		}
		merged, seen := toolCalls[idx]
		if !seen {
			merged = map[string]any{"index": idx}
			toolCalls[idx] = merged
			*toolOrder = append(*toolOrder, idx)
		}
		mergeToolCallDelta(merged, call)
	}
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖，function.arguments 拼接。
// 上游 SOLO 用 `function_call` 字段（实测），OpenAI 标准用 `function`；两者都兼容。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		df, _ = delta["function_call"].(map[string]any) // SOLO 专属字段名
	}
	if df == nil {
		return
	}
	// 清理 SOLO 专属字段,只保留标准 OpenAI function 结构(name/arguments)
	delete(df, "namespace")
	delete(df, "partial_arguments")
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// sortInts 升序排序（避免引 sort 包只为三行）。
func sortInts(a []int) {
	for i := 0; i < len(a)-1; i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

// StreamStat 下发统计：stat 非 nil 时由 Stream* 回填，供调用方记账。
// ToolCalls 在**帧写给客户端的那一刻**置 1——客户端中途断开也算它收到过。
type StreamStat struct {
	ToolCalls int
}

// Stream 流式转换：SOLO SSE → OpenAI SSE chunk，每 chunk flush，保证至少一个 [DONE]。
// 调用方必须先设置过 status 200；本函数自设 SSE headers。
func Stream(w http.ResponseWriter, r io.Reader) error {
	_, err := streamOpts(w, r, nil, nil, "")
	return err
}

// StreamWithError 同 Stream，额外在遇到上游 event:error 时回调 onErr（非 nil），
// 供调用方冷却账号/记录日志；错误信息同时注入 SSE 事件流。
// 返回末帧 token_usage（上游未给时为 nil），供调用方记用量账。
func StreamWithError(w http.ResponseWriter, r io.Reader, onErr func(*SOLOStreamError), stat *StreamStat) (map[string]any, error) {
	return streamOpts(w, r, onErr, stat, "")
}

// StreamWithErrorModel 同 StreamWithError，但每帧的 model 字段回填成 model。
//
// 上游 SOLO 的 SSE 帧里 model 恒为空串（见 writeChunk），照抄下去客户端拿到的是
// "model":"" —— OpenAI 规范要求回显客户端请求的模型名，部分 SDK（litellm 等）
// 会校验这个字段。回填的是客户端要的名字，不是内部 config_name。
func StreamWithErrorModel(w http.ResponseWriter, r io.Reader, onErr func(*SOLOStreamError), stat *StreamStat, model string) (map[string]any, error) {
	return streamOpts(w, r, onErr, stat, model)
}

// streamOpts Stream 的可选参数版本。
func streamOpts(w http.ResponseWriter, r io.Reader, onErr func(*SOLOStreamError), stat *StreamStat, model string) (map[string]any, error) {
	// 写出与保活分属两个 goroutine，所以所有写入（含头）走这一把锁串行化；
	// net/http 的 ResponseWriter 不是并发安全的。
	var (
		wmu         sync.Mutex
		lastWrite   = time.Now()
		committed   bool // 已往下游写过任何字节（含注释帧）——决定空流还能不能换号重试
		headersDone bool
		stopped     bool // 收尾已开始：保活线程不许再写（否则注释帧会插到调用方的错误体前面）
	)
	fl, _ := w.(http.Flusher)
	// 头不能在这里就地设：空流要走重试，此时一个字节都不该写给客户端，
	// 否则 Content-Type 已经是 event-stream，调用方再写 JSON 错误就自相矛盾了。
	ensureHeaders := func() {
		if headersDone {
			return
		}
		headersDone = true
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
	}
	// writeLocked 写一帧，调用方必须已持 wmu。
	writeLocked := func(s string) error {
		ensureHeaders()
		if _, err := io.WriteString(w, s); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		lastWrite = time.Now()
		committed = true
		return nil
	}
	writeRaw := func(s string) error {
		wmu.Lock()
		defer wmu.Unlock()
		return writeLocked(s)
	}

	br := bufio.NewReaderSize(r, 64*1024)
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	var pendingUsage map[string]any
	var lastUsage map[string]any // 已收到的最后一份 token_usage（pendingUsage 会被 writeChunk 消费掉）
	sawDone := false
	sawModel := false     // 见过 output 事件（正文/思考/工具任一）
	sawToolCalls := false // output 里见过工具调用：done 帧据此修正被上游伪装的 finish_reason
	errSent := false      // 已写过 error 帧：之后再来的 output/done 一律不再写
	st := &sseState{}
	tcSent := map[int]tcSeen{} // 已下发的 id/name：上游会把首帧原样重发（见 dedupeToolCallDelta）
	writeChunk := func(delta map[string]any, finish string) error {
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": delta,
				},
			},
		}
		choice := chunk["choices"].([]any)[0].(map[string]any)
		if finish != "" {
			choice["finish_reason"] = finish
		}
		if pendingUsage != nil {
			chunk["usage"] = pendingUsage
			pendingUsage = nil
		}
		raw, _ := json.Marshal(chunk)
		return writeRaw("data: " + string(raw) + "\n\n")
	}
	writeDONE := func() error {
		return writeRaw("data: [DONE]\n\n")
	}

	// 保活：静默满一个间隔就往下游发一个注释帧——**首字节之前也发**。
	// 取舍明写在注释里：发了注释帧 = 已提交响应，那条流就不能再走「空流换号重试」
	// （见 ErrEmptyStream）。而「上游只思考不吐字」恰恰是首字节前的那段静默，
	// 也正是最容易被中间盒掐掉的一段；真正空转的上游是**立刻**结束的，根本等不到一个间隔。
	// 所以冲突实际不会同时命中：快速空流留给重试，长静默留给保活。
	// 间隔在主线程读一次就定死：goroutine 里再读包级变量会与测试/热改并发（-race 可复现）。
	kaInterval := keepaliveInterval
	var kaStop chan struct{}
	if kaInterval > 0 {
		kaStop = make(chan struct{})
		// 退出前先置 stopped 再关线程：否则最后一次 tick 可能在 streamOpts 返回之后
		// 才落到 ResponseWriter 上（调用方接手写错误体时就成了脏帧）。
		// 取锁保证了「要么这次写完整结束、要么根本没开始」。
		defer func() {
			wmu.Lock()
			stopped = true
			wmu.Unlock()
			close(kaStop)
		}()
		go func() {
			t := time.NewTicker(kaInterval)
			defer t.Stop()
			for {
				select {
				case <-kaStop:
					return
				case <-t.C:
					// 整段持锁：与「空流判定 + 收尾」互斥，否则注释帧可能插在
					// 调用方即将写出的 JSON 错误体前面（响应已提交，错误体就成了脏数据）。
					wmu.Lock()
					if stopped {
						wmu.Unlock()
						return
					}
					if time.Since(lastWrite) >= kaInterval {
						// 写失败（客户端已断）不必上报：主循环下一次写/读会拿到同一个错误。
						_ = writeLocked(keepaliveLine)
					}
					wmu.Unlock()
				}
			}
		}()
	}

	for {
		line, err := sseLine(br)
		if err != nil && err != io.EOF {
			return lastUsage, err
		}
		if ev := scanLine(st, strings.TrimRight(line, "\r\n")); ev != nil {
			switch ev.Event {
			case "output":
				if errSent {
					break // 已写过 error 帧，后面的正文不再追加
				}
				delta := map[string]any{}
				if ev.Response != "" {
					delta["content"] = ev.Response
				}
				if ev.Reasoning != "" {
					delta["reasoning_content"] = ev.Reasoning
				}
				if len(ev.ToolCalls) > 0 && string(ev.ToolCalls) != "null" {
					var tc []map[string]any
					if err := json.Unmarshal(ev.ToolCalls, &tc); err == nil {
						// SOLO 上游 tool_call 条目用 `function_call` 字段 → 转成 OpenAI 的 `function`
						kept := tc[:0]
						for _, call := range tc {
							if fc, ok := call["function_call"].(map[string]any); ok {
								call["function"] = fc
								delete(call, "function_call")
							}
							// 清理 SOLO 专属字段,只保留标准 OpenAI function 结构(name/arguments)
							if fn, ok := call["function"].(map[string]any); ok {
								delete(fn, "namespace")
								delete(fn, "partial_arguments")
							}
							dedupeToolCallDelta(call, tcSent)
							// 清空字段后只剩 index 的占位条目不含任何信息，而严格客户端
							// （ZCode 用 zod）见 delta.tool_calls[i].function 缺失就判整帧
							// 非法、整轮失败 —— OpenAI 官方 API 从不发这种条目，丢掉。
							if _, ok := call["function"]; ok || call["id"] != nil || call["type"] != nil {
								kept = append(kept, call)
							}
						}
						tc = kept
						if len(tc) > 0 {
							delta["tool_calls"] = tc
							sawToolCalls = true
							if stat != nil {
								stat.ToolCalls = 1
							}
						}
					}
				}
				if len(delta) > 0 {
					sawModel = true
					if err := writeChunk(delta, ""); err != nil {
						return lastUsage, err
					}
				}
			case "token_usage":
				pendingUsage, lastUsage = ev.Usage, ev.Usage
			case "done":
				// 已写过 error 帧（error 分支自己也写了 [DONE]）：上游随后再补一个 done
				// 会变成第二套收尾帧（agent2api 记录过这个形状）。只留第一套。
				if errSent {
					break
				}
				// SOLO 的 done 帧恒报 finish_reason:"stop"，即使模型发起了工具调用
				// （continue.go 顶部也记着这条上游行为）。原样透传会让下游以为回合已
				// 正常结束、不执行工具，表现为“话说一半没了”。见过 tool_calls 时把
				// stop/空改写为 tool_calls；length 等真实截断信号保持透传。
				fr := ev.FinishReason
				if sawToolCalls && (fr == "" || fr == "stop") {
					fr = "tool_calls"
				}
				if err := writeChunk(map[string]any{}, fr); err != nil {
					return lastUsage, err
				}
				if err := writeDONE(); err != nil {
					return lastUsage, err
				}
				sawDone = true
			case "error":
				// 上游业务错误：回调 + 写一条 error 事件 + [DONE]。
				se := &SOLOStreamError{Code: ev.ErrorCode, Msg: ev.ErrorMessage}
				if onErr != nil {
					onErr(se)
				}
				// 首包门（照 agent2api forward.rs 的 prefetch_head）：账号级错误落在
				// 第一帧、且一个字节都还没写给客户端时，把错误交回调用方换号重试。
				// 上游的额度/凭证失败是 HTTP 200 + 流内 error，直接下发等于让客户端
				// 拿到「成功但报错」，这一轮连号都不换。请求级错误（4001/4026，
				// Kind()==ErrNone）不换：换几个号都是同一个答复，只会白打上游。
				wmu.Lock()
				early := !committed
				wmu.Unlock()
				if early && se.Kind() != ErrNone {
					return lastUsage, se
				}
				// 错误帧要给 OpenAI 的**对象**信封：Agent 侧的 SDK（openai-python 等）
				// 只在 data 里看到 error 键才会抛 APIError；从前这里发的是裸字符串，
				// 客户端解析不出任何一个 chunk，于是「上游明确报错」变成了「模型返回空」。
				msg := fmt.Sprintf("solo error code=%d msg=%s", ev.ErrorCode, ev.ErrorMessage)
				body, _ := json.Marshal(map[string]any{"error": map[string]any{
					"message": msg, "code": ev.ErrorCode, "type": "upstream_error",
				}})
				if err := writeRaw("event: error\n" + "data: " + string(body) + "\n\n"); err != nil {
					return lastUsage, err
				}
				if err := writeDONE(); err != nil {
					return lastUsage, err
				}
				sawDone = true
				errSent = true // 收尾帧已发出，后面的 output/done 都不再写
			}
		}
		if err == io.EOF {
			break
		}
	}
	if !sawDone {
		// 空流：上游 200 但一个模型事件都没有，且我们一个字节都没写给客户端
		// → 交给调用方换号重试（见 ErrEmptyStream）。已写过（保活注释帧也算）就只能按
		// 现状收尾：补 [DONE]，客户端看到一次空回复。
		// 判定与「保活线程可否再写」必须在同一把锁里定案，否则注释帧会插到错误体前面。
		wmu.Lock()
		empty := !sawModel && !committed && lastUsage == nil
		stopped = true
		wmu.Unlock()
		if empty {
			return nil, ErrEmptyStream
		}
		// 幂等兜底：上游中断（无 done）仍写 [DONE]。
		if err := writeDONE(); err != nil {
			return lastUsage, err
		}
	}
	return lastUsage, nil
}

func jsonEscape(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// tcSeen 某个 index 已下发的 id / name（OpenAI 语义里这两个字段只在首帧出现）。
type tcSeen struct{ id, name string }

// dedupeToolCallDelta 清掉重复/空的 id、type、name，只留客户端该拼接的部分。
//
// 国际版 gpt-5.2 / gpt-5.4 在正常增量之外，末尾会**把首帧原样重发一遍**（id 与 name 再来
// 一次，arguments 为空）。OpenAI 语义里 name 只在首帧出现、客户端按字符串拼接累计，收到
// "get_weatherget_weather" 就再也对不上工具表 —— Agent 侧表现为「模型调了个不存在的工具」，
// 而流式（Agent 唯一走法）全中，非流式聚合是覆盖写所以看不出来。
//
// 三种形态都要照顾：原样重发（丢弃）、累积式重发（只补差值）、拆片下发（照旧拼接）。
func dedupeToolCallDelta(call map[string]any, sent map[int]tcSeen) {
	idx := 0
	if v, ok := call["index"].(float64); ok {
		idx = int(v)
	}
	s := sent[idx]
	if id, _ := call["id"].(string); id == "" || id == s.id {
		delete(call, "id")
	} else {
		s.id = id
	}
	if typ, _ := call["type"].(string); typ == "" {
		delete(call, "type")
	}
	if fn, ok := call["function"].(map[string]any); ok {
		name, _ := fn["name"].(string)
		switch {
		case name == "" || name == s.name:
			delete(fn, "name")
		case strings.HasPrefix(name, s.name):
			fn["name"] = strings.TrimPrefix(name, s.name) // 累积式重发：客户端那边已有的前缀不能再发一遍
			s.name = name
		default:
			s.name += name
		}
		if args, _ := fn["arguments"].(string); args == "" {
			delete(fn, "arguments")
		}
		if len(fn) == 0 {
			delete(call, "function")
		}
	}
	sent[idx] = s
}
