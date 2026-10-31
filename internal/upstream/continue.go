// continue.go 上游截断自动续写（SOLO 版）：同一账号同一模型，把多段上游流
// 拼成一条客户端可见的连续 SSE。
//
// 背景：SOLO 对单次响应有输出 token 硬上限，且**按模型不同**（实测 cn:deepseek-v4.1-flash
// 在 32000 处截断、glm-5.3 在 16000 处截断；两者 done 事件都仍报 finish_reason:"stop"
// ——截断信号被上游伪装），Codex 这类不带输出限额的客户端写大文件/长分析会在半途被截
// 而毫无察觉。
//
// 这里挂在 SOLO 帧层（StreamWithError 之下、上游 body 之上）：识别「截断特征」
// （token_usage.completion_tokens >= 该模型 cap 且正常 stop 收尾/静默 EOF）后，
// 用「已输出内容作 assistant + 续写指令」补发同模型请求（实测：模型从中断处
// 无缝接续），后续事件直接接进同一条流；本段的 done 被吞、usage 跨段合成为
// 累计值、客户端只看到一次连续输出。
//
// ponytail: 只续纯文本段（有 tool_calls 分片不续——残缺参数拼接风险大）；每请求段数
// 无显式限额时取 maxContinueSegments，显式限额 ≥ cap 时按 ceil(限额/cap) 扩容
// （硬顶 maxContinueSegmentsHard）；显式限额只在**小于**该模型 cap 时才视为「尊重意图」
// 不续——客户端 max_completion_tokens=128000 这类「上限而非目标」的声明不该把续写关掉
// （实测 2026-10-31：ZCode 按 128k 请求、glm-5.3 在 16000 截断，续写被双重挡住）；
// 限额 ≥ cap 时续写照常，但跨段累计到限额即止，不越客户端的权。
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
	// soloCutCapDefault 单次输出硬上限的默认值（实测 2026-10-02，cn:deepseek-v4.1-flash）。
	// 上游按模型区分这个值，查不到的模型回落默认；用 >= 判定，上游将来抬高上限时本特性
	// 自动静默。
	soloCutCapDefault = 32000

	// maxContinueSegments 单请求最多上游段数的默认值（1 段原生 + 最多 2 次续写），防失控级联。
	maxContinueSegments = 3

	// maxContinueSegmentsHard 显式限额扩容段数的硬顶。限额再大也不无限级联：
	// 每次续写都整段重发对话+已输出内容，段数越大输入成本与失败面越大。
	maxContinueSegmentsHard = 8

	// continueNudge 续写指令：线上实测该措辞能让模型从断点无缝接续（不重头、不道歉）。
	continueNudge = "你上一条回复因达到输出长度上限被截断。请从中断处继续输出剩余内容；不要重复已输出过的部分，不要道歉或解释，直接继续。"
)

// soloCutCaps 按模型实测的单次输出硬上限。键用出站体的裸模型 ID（config_name 口径，
// 带 `__dev`/`__max` 变体后缀的会剥后缀再查）；实测值变动时更新此处。
var soloCutCaps = map[string]int{
	"glm-5.3": 16000, // 实测 2026-10-31：done 仍报 stop，completion 恰停在 16000
}

// cutCapFor 出站请求体 → 该模型的单次输出硬上限。出站体 model 是变体名
// （glm-5.3__max）、config_name 是裸 ID（见 client.go 出站实测注释），两个键都试、
// 变体名再剥 `__` 后缀兜底；都查不到回落 soloCutCapDefault。解析失败同样回落默认
// （与旧行为一致：宁可漏判截断，不可凭空续写）。
func cutCapFor(body []byte) int {
	var obj struct {
		Model      string `json:"model"`
		ConfigName string `json:"config_name"`
	}
	if json.Unmarshal(body, &obj) != nil {
		return soloCutCapDefault
	}
	keys := []string{obj.ConfigName, obj.Model}
	if i := strings.Index(obj.Model, "__"); i > 0 {
		keys = append(keys, obj.Model[:i])
	}
	for _, k := range keys {
		if k == "" {
			continue
		}
		if cap, ok := soloCutCaps[k]; ok {
			return cap
		}
	}
	return soloCutCapDefault
}

// ContinueReader 读 SOLO SSE 并做事件级续写拼接。上层（StreamWithError/
// Aggregate 消费方）看到的永远是「一条完整且恰好一个 done 的流」。
type ContinueReader struct {
	c    *Client
	acct *auth.Auth
	body []byte // 出站请求体（OpenAI 方言，ChatStream 内部再 PrepareBody）

	cur io.ReadCloser
	br  *bufio.Reader
	out []byte

	seg       int
	limitTok  int64 // 客户端显式输出限额（min(max_tokens, max_completion_tokens, max_output_tokens)）；-1 = 未设
	cutCap    int   // 本模型的单次输出硬上限（soloCutCaps / soloCutCapDefault）
	segBudget int   // 本请求允许的上游段数（continueSegBudget 算好存住，段间不重算）

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
	r.cutCap = cutCapFor(body)
	if limit, ok := bodyOutputLimit(body); ok {
		r.limitTok = limit
	} else {
		r.limitTok = -1
	}
	r.segBudget = continueSegBudget(r.limitTok, r.cutCap)
	return r
}

// bodyOutputLimit 请求体的显式输出限额（三个常见字段取最小在场值）。ok=false 表示
// 未设限额。解析失败按「已设 0」保守处理（0 < 任何 cap → 不续写，与旧的
// bodyHasOutputLimit 行为一致）；字段在场但值非正数同样按 0 处理——客户端声明
// max_tokens=0/负值不是「想要完整输出」，是坏参数，不替它扩权。
func bodyOutputLimit(body []byte) (int64, bool) {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return 0, true
	}
	found := false
	minTok := int64(0)
	for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		v, ok := obj[k]
		if !ok || v == nil {
			continue
		}
		tok := int64(0)
		if f, ok := v.(float64); ok && f > 0 {
			tok = int64(f)
		}
		if !found || tok < minTok {
			minTok = tok
		}
		found = true
	}
	if !found {
		return 0, false
	}
	return minTok, true
}

// continueSegBudget 本请求允许的上游段数。无显式限额（limitTok<0）或限额比 cap 还小
// （续写本来就不成立）→ 默认 maxContinueSegments；限额 ≥ cap → 按 ceil(limit/cap)
// 扩容（下限仍是默认值，硬顶 maxContinueSegmentsHard）——让「客户端要 128k」的请求
// 真能续到接近它的意图，同时保住防失控级联的上限。
func continueSegBudget(limitTok int64, cutCap int) int {
	if limitTok < 0 || limitTok < int64(cutCap) {
		return maxContinueSegments
	}
	segs := int(limitTok) / cutCap
	if int64(segs*cutCap) < limitTok {
		segs++
	}
	if segs < maxContinueSegments {
		segs = maxContinueSegments
	}
	if segs > maxContinueSegmentsHard {
		segs = maxContinueSegmentsHard
	}
	return segs
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
	line, err := sseLine(r.br)
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
		// 流内 error 事件：原样下发给客户端，同时记一笔——canContinue 靠它别在报错之后
		// 再续写一段正文（客户端已收到 error+[DONE]，后面又冒出内容就是协议错乱）。
		if ev.Event == "error" {
			r.errSeen = true
		}
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

// canContinue 续写资格：本段真的撞了**该模型**的 cap（completion >= cutCap）且是
// 纯文本段，且没有违背客户端的显式限额——限额比 cap 还小（客户端本来就不想要更多）
// 不续；限额 ≥ cap 时续写照常，但跨段累计到限额即止（再续就越了客户端的权）。
func (r *ContinueReader) canContinue() bool {
	switch {
	case r.seg >= r.segBudget-1:
		return false
	case r.errSeen || r.toolSeen:
		return false
	case r.text.Len() == 0:
		return false
	case r.segTok < float64(r.cutCap):
		return false
	case r.limitTok >= 0:
		if r.limitTok < int64(r.cutCap) || r.cumTok() >= float64(r.limitTok) {
			return false
		}
	}
	return true
}

// cumTok 跨段累计的 completion_tokens（usage 合并帧口径，与记账同源）。
func (r *ContinueReader) cumTok() float64 {
	if v, ok := r.usage["completion_tokens"].(float64); ok {
		return v
	}
	return 0
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
	// 上一段的读错误到此已被续写救回：不清掉的话，下面整段流最后会拿这个陈旧错误
	// 把「已拼完整的回复」上报成传输失败（用量记失败 + 解绑会话粘性）。
	r.readErr = nil
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
