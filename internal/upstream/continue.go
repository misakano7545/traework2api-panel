// continue.go 上游截断自动续写（SOLO 版）：同一账号同一模型，把多段上游流
// 拼成一条客户端可见的连续 SSE。
//
// 背景：SOLO 对单次响应有输出 token 硬上限，且**按变体不同**（实测 cn:deepseek-v4.1-flash
// 在 32000 处截断、cn:glm-5.3-max 在 16000 处截断，裸 glm-5.3 则不截——两者 done 事件
// 都仍报 finish_reason:"stop"，截断信号被上游伪装），Codex 这类不带输出限额的客户端
// 写大文件/长分析会在半途被截而毫无察觉。
//
// 这里挂在 SOLO 帧层（StreamWithError 之下、上游 body 之上）：识别「截断特征」
// （token_usage.completion_tokens >= 该变体的 cap 且正常 stop 收尾）后，
// 用「已输出内容作 assistant + 续写指令」补发同模型请求（实测：模型从中断处
// 无缝接续），后续事件直接接进同一条流；本段的 done 被吞、usage 跨段合成为
// 累计值、客户端只看到一次连续输出。
//
// 第二种截断形态：上游**一个字都不说就干净 EOF**（没有 done 事件）。solosse 的
// 「幂等兜底」会给这种流补一个 [DONE]，而 responses 适配器见到 sawDone 就报
// response.completed —— 半个回答被伪装成完整答复（wb 版线上实测 2 天内 27 次
// 「Codex 莫名中断、要手动继续」就是这个）。这里先用同样的续写体盲重试一次
// （幂等：assistant 前缀带着已输出内容），再失败就如实补 finish_reason=length，
// 让 responses/messages 各自按既有映射报 incomplete / max_tokens。
//
// ponytail: 只续纯文本段（有 tool_calls 分片不续——残缺参数拼接风险大）；每请求段数
// 默认 maxContinueSegments，显式限额 ≥ cap 时按 ceil(限额/cap) 扩容（硬顶
// maxContinueSegmentsHard），续写体的限额按剩余额度下发；限额 < cap 则尊重意图不续。
//
// 没照搬 wb 的两块：① thinking 吃光预算的「关思考续写」——SOLO 侧没有可靠的关思考
// 开关（README「全模型全档实测」：reasoning_effort 各档 reasoning_tokens 中位数与
// 「不发」基本同量级，关不掉），续写只会再烧一份同额度的思考；② 累计输出上限按档位
// 下限 ×4 ——SOLO 没有档位下限概念，段数预算已经是硬闸门。
package upstream

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strings"

	"traework2api/internal/auth"
)

const (
	// soloCutCapDefault 单次输出硬上限的默认值（实测 2026-10-02，cn:deepseek-v4.1-flash）。
	// 用 >= 判定，上游将来抬高上限时本特性自动静默。
	soloCutCapDefault = 32000

	// maxContinueSegments 单请求最多上游段数（1 段原生 + 最多 2 次续写），防失控级联。
	maxContinueSegments = 3

	// maxContinueSegmentsHard 显式限额扩容段数的硬顶。每段都整段重发「原对话 + 已输出
	// 内容」，输入 token 近二次增长：4 段封顶，再大就把账单放大十几倍。
	maxContinueSegmentsHard = 4

	// continueNudge 续写指令：线上实测该措辞能让模型从断点无缝接续（不重头、不道歉）。
	continueNudge = "你上一条回复因达到输出长度上限被截断。请从中断处继续输出剩余内容；不要重复已输出过的部分，不要道歉或解释，直接继续。"
)

// soloCutCaps 按模型实测的单次输出硬上限。**键是出站变体名**：实测（2026-11-03）
// 裸 glm-5.3 能输出 21569 个 completion token 不截断，而 glm-5.3__max 恰在 16000 处
// 被切且 done 仍报 stop。裸名不登记 = 回落默认。
var soloCutCaps = map[string]int{
	"glm-5.3__max": 16000,
}

// cutCapFor 出站请求体 → 该模型的单次输出硬上限。变体名优先于裸名：config_name 恒是
// 裸 ID，排在前面会把变体键遮住。解析失败回落默认（宁可漏判截断，不可凭空续写）。
func cutCapFor(body []byte) int {
	var obj struct {
		Model      string `json:"model"`
		ConfigName string `json:"config_name"`
	}
	if json.Unmarshal(body, &obj) != nil {
		return soloCutCapDefault
	}
	keys := []string{obj.Model}
	if i := strings.Index(obj.Model, "__"); i > 0 {
		keys = append(keys, obj.Model[:i])
	}
	keys = append(keys, obj.ConfigName)
	for _, k := range keys {
		if v, ok := soloCutCaps[k]; ok && v > 0 {
			return v
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
	segBudget int
	limitTok  int64 // 客户端显式输出限额；-1 = 没声明
	cutCap    int

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
	// blindTried 断流盲重试已用（每请求一次，防打断流-重试-再断流的套娃）。
	blindTried bool
	done       bool
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
	r.limitTok = bodyOutputLimit(body)
	r.segBudget = segBudgetFor(r.limitTok, r.cutCap)
	return r
}

// segBudgetFor 段数预算：没声明限额用默认 3 段；限额 ≥ cap 时按 ceil(限额/cap) 扩容
// （硬顶 maxContinueSegmentsHard）。限额 < cap 的不扩容——本来就不续。
func segBudgetFor(limit int64, cutCap int) int {
	segs := maxContinueSegments
	if limit >= int64(cutCap) {
		if n := int((limit + int64(cutCap) - 1) / int64(cutCap)); n > segs {
			segs = n
		}
	}
	if segs > maxContinueSegmentsHard {
		segs = maxContinueSegmentsHard
	}
	return segs
}

// bodyOutputLimit 客户端显式输出限额（三个字段取最小正数）；没声明或解析失败返回 -1。
// 语义是「上限而非目标」：比 cap 大不代表客户端想在 cap 处被截。
func bodyOutputLimit(body []byte) int64 {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return -1
	}
	limit := int64(-1)
	for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		v, ok := obj[k].(float64)
		if !ok || v <= 0 {
			continue
		}
		if limit < 0 || int64(v) < limit {
			limit = int64(v)
		}
	}
	return limit
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
		// 续写段的思考对客户端剥离：续写只在「已写出正文」时发生（canContinue 要求
		// text 非空），所以这里恒有正文在场，删掉 reasoning 不会造成协议问题；反过来
		// 留着会让 responses 侧在 message item 已打开时再开第二个 reasoning item，
		// codex 后续文本 delta 全报 "OutputTextDelta without active item"（wb 实测 12332 条）。
		// 剥离后「续写段纯思考期」的静默由 solosse 的 15s keepalive 注释帧兜住
		// （responses 适配器对注释帧原样透传），不需要像 wb 那样把思考漏给客户端。
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
			// total_tokens 按「末段 prompt + 累计 completion」重算，保持口径自洽：
			// 直接求和会把每段的 prompt（都含整段上下文）累加成 N 倍。
			if p, ok := r.usage["prompt_tokens"].(float64); ok {
				if c, ok := r.usage["completion_tokens"].(float64); ok {
					r.usage["total_tokens"] = p + c
				}
			}
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
	if r.canContinue() && r.switchSegment(false) {
		return
	}
	// 没续写却停在整千 token 边界上：上游硬截断的 completion_tokens 是整数上限
	// （实测 16000 / 32000），自然收尾几乎不会正好落在整千上。cap 表靠实测攒，
	// 这条日志就是扩表的来源——静默漏判会让「半句话」永远只报 stop。
	if capPlateau(r.segTok) && r.segTok < float64(r.cutCap) {
		log.Printf("WARN: [continue] 疑似未登记的截断上限 model=%s completion_tokens=%d finish=%s（若是，请登记到 soloCutCaps）",
			modelOfBody(r.body), int(r.segTok), doneFinishOf(pend))
	}
	r.emitBlock(pend)
	r.done = true
	// 上游正常收尾：即便之前某段是断的（续写救回来了），整体也是完整的，不算错误。
	r.readErr = nil
}

// doneFinishOf 从 done 块的 data 行里取 finish_reason（只用于日志）。
func doneFinishOf(pend []string) string {
	for _, l := range pend {
		if !strings.HasPrefix(strings.TrimSpace(l), "data:") {
			continue
		}
		var obj struct {
			FinishReason string `json:"finish_reason"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(l), "data:")), &obj) == nil {
			return obj.FinishReason
		}
	}
	return "?"
}

// capPlateau 疑似「收尾在整千 token 边界」上（见 handleDone 的日志用途）。
func capPlateau(tok float64) bool {
	if tok < 8000 || tok != float64(int(tok)) {
		return false
	}
	return int(tok)%1000 == 0
}

// endSegment 无 done 的 EOF 收尾：能续就换段，否则盲重试一次，再否则如实补截断终态。
//
// 三种收尾必须区分开（否则就是「截断不可见」的第二种形态）：
//   - 本段撞 cap：续写（同账号同模型，见文件头）；
//   - 干净 EOF（无 done、无读错误）且已产出内容：盲重试一次。上游偶发这么收尾，
//     旧行为下 solosse 会补 [DONE]、responses 报 completed，半个回答被当完整答复；
//   - 仍不行：补一条 finish_reason=length 的 done，让下游按截断处理。
//
// 传输层读错误（readErr 非 nil）不在这里救：它已经会被 Read 上抛，上层按失败换号，
// 硬补一条 done 反而把「上游故障」伪装成「截断的正常结束」。
func (r *ContinueReader) endSegment(sawDone bool) {
	if r.done {
		return
	}
	if r.canContinue() && r.switchSegment(false) {
		return
	}
	if r.canBlindRetry() && r.switchSegment(true) {
		return
	}
	r.emitTruncated()
	r.done = true
}

// canBlindRetry 断流盲重试资格：已产出内容、没见工具分片/error 帧、不是传输层读错误、
// 每请求一次、且还有段数预算。
func (r *ContinueReader) canBlindRetry() bool {
	switch {
	case r.blindTried, r.errSeen, r.toolSeen:
		return false
	case r.readErr != nil:
		return false
	case r.seg >= r.segBudget-1:
		return false
	case r.text.Len() == 0 && r.reason.Len() == 0:
		return false
	}
	return true
}

// emitTruncated 补一条截断终态（finish_reason=length）事实帧。
//
// 补在这一层而不是三个适配器里各写一份：本层是 chat / responses / messages 共用的
// 唯一接缝，它们各自早已把 length 映射正确（responses → response.incomplete +
// max_output_tokens，messages → stop_reason=max_tokens），此前缺的只是「这段没正常
// 收尾」这个事实没被任何一层说出口。零输出段不补：空流仍要走 ErrEmptyStream 换号。
func (r *ContinueReader) emitTruncated() {
	// errSeen：上游发过 event:error，solosse 已经写过 error 帧 + [DONE] 收尾。这里再补
	// 一条 done 就是第二套终态帧，客户端按 SSE 解析会拿到两个终止（Codex 直接报错）。
	if r.errSeen {
		return
	}
	if r.text.Len() == 0 && r.reason.Len() == 0 && !r.toolSeen {
		return
	}
	r.emitBlock([]string{"event:done\n", `data:{"finish_reason":"length"}` + "\n"})
	log.Printf("WARN: [continue] seg=%d model=%s text=%dB 无 done 收尾 → 如实补 finish_reason=length",
		r.seg, modelOfBody(r.body), r.text.Len())
}

// canContinue 续写资格：本段真的撞了**该模型**的 cap 且是纯文本段，且没违背客户端的
// 显式限额——限额比 cap 还小（客户端本来就不想要更多）不续；跨段累计到限额也不续。
func (r *ContinueReader) canContinue() bool {
	switch {
	case r.seg >= r.segBudget-1:
		return false
	case r.errSeen || r.toolSeen:
		return false
	case r.text.Len() == 0:
		return false
	case !r.hitCap():
		return false
	case r.limitTok >= 0 && (r.limitTok < int64(r.cutCap) || r.cumTok() >= float64(r.limitTok)):
		return false
	}
	return true
}

// hitCap 本段的 completion_tokens 落在「该模型 cap」上（不是随便超过它）。
//
// 上界不能省：cap 表只有实测过的几个变体，没登记的大 cap 模型（如上游表声明 64000 的
// __max 条目）用默认 32000 判定时，一个**自然收尾**在 45000 的长回答会落进「>= cap」，
// 于是把完整的 done 吞掉、再烧一次上下文去续写——花的钱和上下文都和截断无关。
// 实测的截断值都是正好压线（32000 / 32003 / 16000），留 5% 余量足够吸收分片计数误差。
func (r *ContinueReader) hitCap() bool {
	capv := float64(r.cutCap)
	return r.segTok >= capv && r.segTok <= capv*1.05
}

// cumTok 跨段累计的 completion_tokens（usage 合并帧口径，与记账同源）。
func (r *ContinueReader) cumTok() float64 {
	if v, ok := r.usage["completion_tokens"].(float64); ok {
		return v
	}
	return 0
}

// switchSegment 用「已输出内容 + 续写指令」补发同模型请求；成功则换段并清本段判定位。
// blind=true 表示由「无 done 收尾」触发的盲重试（记次数、日志区分）。
// 注意：done 块已由调用方暂存（未 emitBlock），续写成功即隐式吞掉。
func (r *ContinueReader) switchSegment(blind bool) bool {
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
	if blind {
		r.blindTried = true
	}
	r.cur, r.br = rc, bufio.NewReaderSize(rc, 64*1024)
	r.segTok = 0 // 本段判定重置；usage/文本/工具位跨段保留
	// 上一段的读错误到此已被续写救回：不清掉的话，下面整段流最后会拿这个陈旧错误
	// 把「已拼完整的回复」上报成传输失败（用量记失败 + 解绑会话粘性）。
	r.readErr = nil
	note := ""
	if blind {
		note = "（断流盲重试）"
	}
	log.Printf("[continue] seg=%d model=%s text=%dB → 续写%s", r.seg, modelOfBody(r.body), r.text.Len(), note)
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
	// 限额必须按剩余额度下发：原样沿用客户端的 max_tokens，段 2 自己就能再吐一个 cap，
	// 声明 20000 实交 32000，越了客户端的权。
	if r.limitTok >= 0 {
		remaining := r.limitTok - int64(r.cumTok())
		if remaining < 1 {
			return nil, errLimitSpent
		}
		for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
			if _, ok := obj[k]; ok {
				obj[k] = remaining
			}
		}
	}
	return json.Marshal(obj)
}

// errLimitSpent 跨段累计已到客户端限额，不再续写（上层按截断终态处理）。
var errLimitSpent = errors.New("continue: 已达客户端输出限额")

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

// mergeUsageInto 跨段累计 usage：
//   - 输出侧（completion_tokens / reasoning_tokens / 各种积分）数字叶子求和；
//   - 输入侧（prompt_* / cache_*）取末段值——续写段的 prompt 天然已包含整个上下文，
//     求和会把上下文重复计入 N 段倍，客户端按 usage 判上下文占用时会虚高到提前全量压缩；
//   - 结构取并集（同 wb 版口径）。
func mergeUsageInto(dst, src map[string]any) {
	for k, v := range src {
		if usageTakesLast(k) {
			dst[k] = v
			continue
		}
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
		case []any:
			dst[k] = sv // 数组（如 completion_tokens_details 之外的列表）没有求和语义，取末段
		default:
			dst[k] = v
		}
	}
}

// usageTakesLast 该 usage 键跨段取「末段值」而不是求和：输入侧的所有键。
// SOLO 的缓存命中量报在 cache_read_input_tokens（见表头 token_usage 样例）。
func usageTakesLast(k string) bool {
	return strings.HasPrefix(k, "prompt") || strings.HasPrefix(k, "cache_")
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
