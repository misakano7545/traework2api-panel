// continue.go 上游截断自动续写（SOLO 版）：同一账号同一模型，把多段上游流
// 拼成一条客户端可见的连续 SSE。
//
// 背景：SOLO 对单次响应有 32000 输出 token 的硬上限（实测 cn:deepseek-v4.1-flash
// 在 32000 处截断，但 done 事件仍报 finish_reason:"stop"——截断信号被上游伪装），
// Codex 这类不带输出限额的客户端写大文件/长分析会在半途被截而毫无察觉。
//
// 这里挂在 SOLO 帧层（StreamWithError 之下、上游 body 之上）：识别「截断特征」
// （token_usage.completion_tokens >= soloCutCap 且正常 stop 收尾/静默 EOF）后，
// 用「已输出内容作 assistant + 续写指令」补发同模型请求（实测：模型从中断处
// 无缝接续），后续事件直接接进同一条流；本段的 done 被吞、usage 跨段合成为
// 累计值、客户端只看到一次连续输出。
//
// ponytail: 只续纯文本段（有 tool_calls 分片不续——残缺参数拼接风险大）；每请求
// 最多 maxContinueSegments 段；客户端显式限额（max_tokens 等）不续（尊重意图）。
package upstream

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"strings"

	"traework2api/internal/auth"
)

const (
	// soloCutCap SOLO 单次输出的 32000 token 上限（实测 2026-10-02）。kimi 等其他
	// 模型是否同值待验；用 >= 判定，上游将来抬高上限时本特性自动静默。
	soloCutCap = 32000

	// maxContinueSegments 单请求最多上游段数（1 段原生 + 最多 2 次续写），防失控级联。
	maxContinueSegments = 3

	// continueNudge 续写指令：线上实测该措辞能让模型从断点无缝接续（不重头、不道歉）。
	continueNudge = "你上一条回复因达到输出长度上限被截断。请从中断处继续输出剩余内容；不要重复已输出过的部分，不要道歉或解释，直接继续。"
)

// ContinueReader 读 SOLO SSE 并做事件级续写拼接。上层（StreamWithError/
// Aggregate 消费方）看到的永远是「一条完整且恰好一个 done 的流」。
type ContinueReader struct {
	c    *Client
	acct *auth.Auth
	body []byte // 出站请求体（OpenAI 方言，ChatStream 内部再 PrepareBody）

	cur io.ReadCloser
	br  *bufio.Reader
	out []byte

	seg      int
	limitSet bool

	// 事件组装（对齐 solosse 的 event:/data: 跨行累积）。
	evEvent string
	evData  strings.Builder
	pend    []string // 当前事件的原始行（含 \n），决定转发/改写/吞掉

	text   strings.Builder
	reason strings.Builder
	usage  map[string]any // 跨段累计（数字叶子求和），下游记账用
	segTok float64        // 本段 token_usage 的 completion_tokens（截断判定用）

	toolSeen bool
	errSeen  bool
	done     bool
	// readErr 上游读取错误（非 EOF）。吞掉它，中途断连就会被当成「正常收尾」——
	// 补一个 [DONE]、token_usage 已有就记成功，客户端拿到一条看似完整实则截断的回复。
	// 这正是本文件要防的事（截断不可见，见文件头注释）。Read 把已写出的字节交完后再返回它。
	readErr error
}

// NewContinueReader 包装一段 SOLO 上游 body：截断自动续写（同账号同模型）。
func NewContinueReader(c *Client, a *auth.Auth, body []byte, rc io.ReadCloser) *ContinueReader {
	r := &ContinueReader{c: c, acct: a, body: body, cur: rc}
	r.br = bufio.NewReaderSize(rc, 64*1024)
	r.limitSet = bodyHasOutputLimit(body)
	return r
}

// bodyHasOutputLimit 请求体显式带输出限额则尊重之，不续写。解析失败按有限额处理。
func bodyHasOutputLimit(body []byte) bool {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return true
	}
	for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if v, ok := obj[k]; ok && v != nil {
			return true
		}
	}
	return false
}

func (r *ContinueReader) Read(p []byte) (int, error) {
	for len(r.out) == 0 && !r.done {
		r.step()
	}
	if len(r.out) > 0 {
		n := copy(p, r.out)
		r.out = r.out[n:]
		return n, nil
	}
	// 已写出的字节先交完，错误最后再抛：上游中途断连必须让上层看见（否则会补 [DONE]
	// 并按成功记账），而不是伪装成 io.EOF 的正常结束。
	if r.readErr != nil {
		return 0, r.readErr
	}
	return 0, io.EOF
}

func (r *ContinueReader) Close() error {
	if r.cur != nil {
		return r.cur.Close()
	}
	return nil
}

// step 处理一行：累积当前事件的原始行，事件边界（空行）时按类型处理。
func (r *ContinueReader) step() {
	line, err := r.br.ReadString('\n')
	trimmed := strings.TrimRight(line, "\r\n")
	switch {
	case trimmed == "":
		if len(r.pend) == 0 {
			r.emitRaw("\n")
			break
		}
		r.flushEvent()
	case strings.HasPrefix(trimmed, "event:"):
		r.evEvent = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		r.pend = append(r.pend, line)
	case strings.HasPrefix(trimmed, "data:"):
		r.evData.WriteString(strings.TrimPrefix(trimmed, "data:"))
		r.pend = append(r.pend, line)
	case strings.HasPrefix(trimmed, ":"):
		r.pend = append(r.pend, line) // 注释行原样保留
	default:
		r.pend = append(r.pend, line)
	}
	if err != nil {
		// EOF 或读错误：收尾当前事件；未见的 done 走 canContinue 抉择。
		// 读错误要记住（EOF 不算）：正常收尾由 handleDone 清掉，没清掉就说明这段流是断的。
		if err != io.EOF {
			r.readErr = err
		}
		if len(r.pend) > 0 {
			r.flushEvent()
		}
		if !r.done {
			r.endSegment(false)
		}
	}
}

// flushEvent 一个完整事件（event:/data: 块）到达边界时的处置。
func (r *ContinueReader) flushEvent() {
	pend := r.pend
	r.pend = nil
	data := r.evData.String()
	ev, perr := ParseSOLOLine(r.evEvent, data)
	r.evEvent = ""
	r.evData.Reset()
	if perr != nil || ev == nil {
		r.emitBlock(pend)
		return
	}
	switch ev.Event {
	case "output":
		if ev.Response != "" {
			r.text.WriteString(ev.Response)
		}
		if ev.Reasoning != "" {
			r.reason.WriteString(ev.Reasoning)
		}
		if len(ev.ToolCalls) > 0 && string(ev.ToolCalls) != "null" {
			r.toolSeen = true
		}
		// 续写段的思考是机制内部产物（第一段已展示过）：对客户端剥离，否则 responses
		// 侧会在 message item 还打开时开第二个 reasoning item，codex 后续文本 delta
		// 全部报 "OutputTextDelta without active item"（wb 同款实测 12332 条）。
		// 仍累计进 r.reason 供下一段上下文；剥离后空载事件下游会自然忽略。
		if r.seg > 0 && ev.Reasoning != "" {
			var obj map[string]any
			if json.Unmarshal([]byte(data), &obj) == nil {
				delete(obj, "reasoning_content")
				if raw, err := json.Marshal(obj); err == nil {
					r.emitBlock(rewriteDataLine(pend, string(raw)))
					return
				}
			}
		}
		r.emitBlock(pend)
	case "token_usage":
		if ev.Usage != nil {
			if v, ok := ev.Usage["completion_tokens"].(float64); ok {
				r.segTok = v
			}
			if r.usage == nil {
				r.usage = map[string]any{}
			}
			mergeUsageInto(r.usage, ev.Usage)
			// 改写为跨段累计值再下发：下游（StreamWithError 的 lastUsage →
			// handler 记账）拿到的就是合计，不重写只会记到最后一段。
			if raw, err := json.Marshal(r.usage); err == nil {
				r.emitBlock(rewriteDataLine(pend, string(raw)))
				return
			}
		}
		r.emitBlock(pend)
	case "done":
		r.handleDone(pend)
	default:
		r.emitBlock(pend)
	}
}

// handleDone 最终段 done 的处理：能续 → 吞掉（客户端等最终段的 done）；否则原样回放。
func (r *ContinueReader) handleDone(pend []string) {
	if r.done {
		r.emitBlock(pend)
		return
	}
	if r.canContinue() && r.switchSegment() {
		return
	}
	r.emitBlock(pend)
	r.done = true
	// 上游正常收尾：即便之前某段是断的（续写救回来了），整体也是完整的，不算错误。
	r.readErr = nil
}

// endSegment 无 done 的 EOF 收尾：能续就换段，否则直接结束（solosse 兜底补 [DONE]）。
func (r *ContinueReader) endSegment(sawDone bool) {
	if r.done {
		return
	}
	if r.canContinue() && r.switchSegment() {
		return
	}
	r.done = true
}

// canContinue 续写资格：本段真的撞了 cap（completion >= 32000）且是纯文本段。
func (r *ContinueReader) canContinue() bool {
	switch {
	case r.seg >= maxContinueSegments-1:
		return false
	case r.errSeen || r.toolSeen || r.limitSet:
		return false
	case r.text.Len() == 0:
		return false
	case r.segTok < soloCutCap:
		return false
	}
	return true
}

// switchSegment 用「已输出内容 + 续写指令」补发同模型请求；成功则换段并清本段判定位。
// 注意：done 块已由调用方暂存（未 emitBlock），续写成功即隐式吞掉。
func (r *ContinueReader) switchSegment() bool {
	body, err := r.continuationBody()
	if err != nil {
		log.Printf("WARN: [continue] 续写请求体构造失败: %v", err)
		return false
	}
	r.cur.Close()
	rc, status, _, terr := r.c.ChatStream(r.acct, body)
	if terr != nil || status >= 400 || rc == nil {
		if rc != nil {
			rc.Close()
		}
		log.Printf("WARN: [continue] seg=%d 续写失败（status=%d err=%v）→ 降级为截断终态", r.seg+1, status, terr)
		return false
	}
	r.seg++
	r.cur, r.br = rc, bufio.NewReaderSize(rc, 64*1024)
	r.segTok = 0 // 本段判定重置；usage/文本/工具位跨段保留
	log.Printf("[continue] seg=%d model=%s text=%dB → 续写", r.seg, modelOfBody(r.body), r.text.Len())
	return true
}

// continuationBody 原请求 messages + assistant（已输出正文）+ 续写指令。
// ponytail: 实测形状只有纯 content（P2 探针验证无缝接续）；不带 reasoning_content，
// 避免上游对 assistant 附带字段的兼容性未知数。其余字段（model/function/参数）原样保留。
func (r *ContinueReader) continuationBody() ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(r.body, &obj); err != nil {
		return nil, err
	}
	msgs, _ := obj["messages"].([]any)
	msgs = append(msgs,
		map[string]any{"role": "assistant", "content": r.text.String()},
		map[string]any{"role": "user", "content": continueNudge})
	obj["messages"] = msgs
	return json.Marshal(obj)
}

func (r *ContinueReader) emitRaw(s string) {
	r.out = append(r.out, s...)
}

// emitBlock 原样写出事件块（各行已带 \n，块尾补一个空行分隔）。
func (r *ContinueReader) emitBlock(lines []string) {
	for _, l := range lines {
		r.out = append(r.out, l...)
	}
	r.out = append(r.out, '\n')
}

// rewriteDataLine 把块里的 data: 行换成新载荷（保留 event: 等其余行原样）。
func rewriteDataLine(lines []string, payload string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "data:") {
			out = append(out, "data:"+payload+"\n")
			continue
		}
		out = append(out, l)
	}
	return out
}

// mergeUsageInto 跨段累计 usage：数字叶子求和，结构取并集（同 wb 版口径）。
func mergeUsageInto(dst, src map[string]any) {
	for k, v := range src {
		switch sv := v.(type) {
		case float64:
			if dv, ok := dst[k].(float64); ok {
				dst[k] = dv + sv
			} else {
				dst[k] = sv
			}
		case map[string]any:
			if dm, ok := dst[k].(map[string]any); ok {
				mergeUsageInto(dm, sv)
			} else {
				nm := map[string]any{}
				mergeUsageInto(nm, sv)
				dst[k] = nm
			}
		default:
			dst[k] = v
		}
	}
}

func modelOfBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &obj) != nil {
		return "?"
	}
	return obj.Model
}
