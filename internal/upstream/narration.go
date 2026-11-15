// narration.go 「说一句预告就停」的救援续写（SOLO 侧）。
//
// 与前两种收尾的分工（都在 continue.go 的一条流里）：
//   - canContinue：本段撞了该模型的输出 cap（done 仍假报 stop）→ 接着写；
//   - canBlindRetry：上游干脆不发 done 就干净 EOF → 盲重试一次；
//   - 这里：finish_reason=stop、零工具调用、零报错，正文却停在**一句预告**上
//     （「……：」「……，」「……」）就结束了。上游认为这轮说完了，客户端把
//     「说到一半没了」渲染成中断——这正是最难查的那种：没有任何一层报错，
//     日志里既没有 length 也没有断流，前两层一次都不会触发。
//
// 救援动作与续写段相同（复用 switchSegment）：把已输出正文作为 assistant 前缀、
// 换一句「你停在一句预告上，接着写完」重发一次，新段的帧接进同一条客户端可见流。
//
// 判据刻意窄，五层同时成立才救（照参考实现 workbuddy2api 的 narration.go，
// 那边线上取证过误救的两种代价）：
//  1. finish_reason=stop、正文非空、零 tool_calls、无 error 帧、非传输层读错误；
//  2. 请求体声明了工具，**且历史里有工具轮**——角色扮演/闲聊客户端也常常挂一堆
//     工具却只用纯文本回答，它们以冒号收尾是正常文风，不能救；
//  3. 正文尾部是「未说完」形态（冒号/逗号/省略号/破折号/左括号/未闭合代码块/
//     没有句末标点），且不是「把控制权交回用户」的问句（问号收尾、「请确认」类措辞）；
//  4. 只有标点型尾部受长度闸门（narrationMaxBytes）限制：结构上确实没写完的
//     （未闭合代码块、截断的文本化调用标记）不限长；
//  5. 每请求最多一次，不套娃。
//
// 与参考实现的差别：那边 narration 之外还有第二层 toolonly.go（句号收尾但仍该动手
// 的形态，只采纳「续写段回来的是完整 tool_calls」）——那需要把整段缓冲下来再决定
// 要不要采纳，属于另一笔改动；本仓先只做「尾部悬空」这一条能在流内边写边判的形态。
package upstream

import (
	"encoding/json"
	"log"
	"strings"
)

// narrationNudge 救援续写的指令。与 continueNudge 的差别：那条针对「撞长度上限」，
// 强调不要重复；这条针对「自己以为说完了」，必须点明「你停在一句预告上」，
// 否则模型会以为任务已完成而再重复一遍结论。
const narrationNudge = "你上一条回复停在一句预告（例如以冒号结尾的「……：」）上就结束了，剩余内容还没写。请紧接着把剩下的部分写完：需要查看文件、执行命令或调用工具就直接调用，不要重复已经写过的内容，不要重新开头，不要道歉或解释。"

// narrationMaxBytes narration 的长度闸门（只对「标点型尾部」生效）。
//
// 预告句实测集中在 40~200B，400B 已覆盖全部真实预告样本；更长的「停在冒号上」
// 更可能是正常排版（agent 客户端的「说明 + 列表」几乎总以冒号或逗号收尾）。
// 未闭合代码块与截断的调用标记不受这条限制：那是结构上确实没写完，与长度无关。
const narrationMaxBytes = 400

// toolCallMarkupMarkers 模型把工具调用写成**文本标记**时用到的特征串。
//
// 这类正文本身就是一次工具调用，客户端会自己解析并执行。网关只看到「正文」，
// 若因为尾部不是句末标点就判定「话没说完」而补发续写，模型会再发一次调用、
// 客户端再执行一次——用户看到的是「任务早该结束了却一直在工具调用」。所以
// 标记必须按**完整性**分流（见 toolCallMarkupState），与长度闸门一起构成
// narrationTailSuspends 的第一道判断。
var toolCallMarkupMarkers = []string{
	"｜｜DSML｜｜", "|DSML|", // DSML 标记（全角/半角竖线两种写法都出现过）
	"</function_calls>", "<function_calls>",
	"antml:invoke", "<invoke name=",
	"</tool_calls>", "<tool_calls>", // 少数 prompt 会把工具调用格式教成这种 XML
}

// markupState 正文里「写成文本的工具调用」的完整程度。
type markupState int

const (
	markupNone     markupState = iota // 没有工具调用标记
	markupComplete                    // 标记块已闭合：这是一次完整的调用
	markupPartial                     // 标记中间断了：调用写了一半
)

// toolCallMarkupState 判断正文里工具调用标记的状态。
func toolCallMarkupState(text string) markupState {
	if text == "" {
		return markupNone
	}
	found := false
	for _, m := range toolCallMarkupMarkers {
		if strings.Contains(text, m) {
			found = true
			break
		}
	}
	if !found {
		return markupNone
	}
	t := strings.TrimRight(text, " \t\r\n")
	switch {
	case strings.HasSuffix(t, "</function_calls>"), strings.HasSuffix(t, "</tool_calls>"):
		return markupComplete
	case strings.HasSuffix(t, "calls>") && strings.Contains(t, "DSML"):
		return markupComplete // DSML 的调用块以 "</｜｜DSML｜｜ calls>" 收尾
	}
	return markupPartial
}

// userYieldMarkers 正文里「把控制权交回用户」的措辞。
//
// 救援的语义是「模型没说完，替它补上」，但有一类收尾是**故意的**：模型在等用户
// 拍板（「邮件已经写好，请确认后我再发送。」）。这时补一句「继续执行下一步」会拿到
// 工具调用并被采纳，等于替用户按下发送键（参考实现的 issue #209 线上已发生两例：
// 用户要求先确认再发，结果 agent 直接群发）。
//
// 判据取「措辞 + 问号」，不猜语义：宁可漏救（客户端自己重试一轮的代价是几秒），
// 也不能替用户按下发送键。
var userYieldMarkers = []string{
	"请确认", "确认后", "等你确认", "待你确认", "等你回复",
	"是否需要", "需不需要", "要不要我", "要不要", "你希望", "请告知",
	"让我知道", "请你确认", "你确认后", "确认无误", "可以吗", "行吗", "好吗",
}

// userYieldMarkersEN 英文形态（模型偶发用英文收尾）。
var userYieldMarkersEN = []string{
	"please confirm", "let me know", "should i ", "do you want", "would you like",
	"awaiting your", "waiting for your", "confirm before", "shall i ",
}

// textYieldsToUser 正文是否在「等用户回话」：以问号收尾，或含请确认类措辞。
//
// 问号收尾在 narrationTailSuspends 里已被判为「说完了」（句末标点），这条判据
// 挡的是另一类：正文里带着「请确认后我再发送，正在等你确认：」这种既有预告符、
// 又是刻意等人的写法。
func textYieldsToUser(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	if strings.HasSuffix(t, "？") || strings.HasSuffix(t, "?") {
		return true
	}
	for _, m := range userYieldMarkers {
		if strings.Contains(t, m) {
			return true
		}
	}
	low := strings.ToLower(t)
	for _, m := range userYieldMarkersEN {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// narrationTailStructurallyOpen 尾部是否「结构上没写完」（未闭合代码块 / 截断的
// 调用标记）：这类形态不受 narrationMaxBytes 限制。
func narrationTailStructurallyOpen(text string) bool {
	if toolCallMarkupState(text) == markupPartial {
		return true
	}
	return strings.Count(strings.TrimRight(text, " \t\r\n"), "```")%2 == 1
}

// narrationTailSuspends 正文尾部是否「话没说完」。
//
// 只认结构性的收尾符，不猜语义：
//   - 全角/半角冒号、逗号、顿号、破折号、省略号、左括号/左引号 → 后面必然还有东西；
//   - 未闭合的 ``` 代码块 → 代码块写了一半；
//   - 末尾既不是句末标点也不是收尾符号 → 视为未写完（英文旁白
//     "Reading failfirst.sh mutation script" 这种就是这一形态：现在分词短语，
//     没有任何标点）。
//
// 例外：文本化的工具调用标记按**完整性**分流——完整块返回 false（客户端会执行它，
// 续写会让模型再发一次），截断块返回 true（调用写到一半断了，必须续写把块补完，
// 否则客户端只留下半截标记垃圾）。
func narrationTailSuspends(text string) bool {
	switch toolCallMarkupState(text) {
	case markupComplete:
		return false
	case markupPartial:
		return true
	}
	t := strings.TrimRight(text, " \t\r\n")
	if t == "" {
		return false
	}
	// 结构性收尾：该块已经完整。
	if strings.HasSuffix(t, "</summary>") || strings.HasSuffix(t, "</details>") ||
		strings.HasSuffix(t, "</analysis>") {
		return false
	}
	if strings.Count(t, "```")%2 == 1 {
		return true
	}
	rs := []rune(t)
	last := rs[len(rs)-1]
	if strings.ContainsRune("：:，,、—–…（(【[", last) {
		return true
	}
	// 句末标点与收尾符：正常说完的形态。
	if strings.ContainsRune("。.!！?？\"'”’）)》】]}`*~&", last) {
		return false
	}
	return true
}

// bodyToolShape 请求体是否「工具类请求」：声明了工具、且历史里有工具轮，以及
// 最后一条消息是不是工具结果（模型刚看完工具输出，本来该继续动手）。
//
// 前两个条件缺一不可：只声明工具不够（角色扮演/闲聊客户端常常挂一堆工具却只用纯文本
// 回答，它们的「以冒号收尾」是正常文风）；有工具轮说明这是一条 agent 会话，模型本来
// 就该在这一步调工具或继续写。解析失败按「不是工具请求」处理（宁可少救不可救错）。
func bodyToolShape(body []byte) (declared, toolTurn, lastTool bool) {
	var obj struct {
		Tools []any `json:"tools"`
		Msgs  []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []any  `json:"tool_calls"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &obj) != nil {
		return false, false, false
	}
	declared = len(obj.Tools) > 0
	for _, m := range obj.Msgs {
		if m.Role == "tool" || m.ToolCallID != "" || len(m.ToolCalls) > 0 {
			toolTurn = true
		}
	}
	if n := len(obj.Msgs); n > 0 {
		last := obj.Msgs[n-1]
		lastTool = last.Role == "tool" || last.ToolCallID != ""
	}
	return declared, toolTurn, lastTool
}

// toolOnlyMaxBytes 「说完了但其实该动手」的长度上限：实测这类收尾是一眼扫过的一句话
// （参考实现 156 条样本），更长的正文正常说完的概率远高于「该动手却停了」。
const toolOnlyMaxBytes = 400

// toolOnlyCandidate 「尾巴看着说完了、其实没干完」的候选形态：
//
//	finish=stop、正文很短、尾部是句末标点（不是预告句，那种由 narration 救）、
//	不在等用户拍板，且请求是「声明了工具 + 历史里有工具轮 + 最后一条是工具结果」。
//
// 参考实现（workbuddy2api 的 toolonly.go）在这条判据上直接救援，但它的采纳条件是
// **续写段必须回完整的 tool_calls**、否则整段丢弃 —— 意味着每命中一次都要先缓冲一整段
// 上游输出再决定去留（多打一次上游，大上下文下按本仓实测单价是每万 prompt token
// ≈0.28 积分）。本仓先只观测不救援：攒到真实样本比例再决定值不值这笔钱。
func toolOnlyCandidate(body []byte, text string) bool {
	declared, toolTurn, lastTool := bodyToolShape(body)
	if !declared || !toolTurn || !lastTool {
		return false
	}
	t := strings.TrimSpace(text)
	if t == "" || len(t) >= toolOnlyMaxBytes {
		return false
	}
	if textYieldsToUser(t) {
		return false
	}
	// 尾部悬空的归 narration（那条已经会救），别让两条救援抢同一个请求。
	return !narrationTailSuspends(t)
}

// noteToolOnlyCandidate 命中「说完了但其实该动手」形态时留一行 WARN（不改行为）。
func (r *ContinueReader) noteToolOnlyCandidate(finish string) {
	if r.toolSeen || r.errSeen || finish != "stop" {
		return
	}
	txt := r.text.String()
	if !toolOnlyCandidate(r.body, txt) {
		return
	}
	prompt := int64(-1)
	if v, ok := r.usage["prompt_tokens"].(float64); ok {
		prompt = int64(v)
	}
	log.Printf("WARN: [toolonly] model=%s seg=%d prompt_tokens=%d text=%dB 尾 60B=%q → 「说完了但该动手」候选（仅观测，不救援）",
		modelOfBody(r.body), r.seg, prompt, len(txt), tailText(txt, 60))
}

// canNarrate 预告句救援资格（判据见文件头）。finish 是本段 done 事件的原始 finish_reason。
func (r *ContinueReader) canNarrate(finish string) bool {
	switch {
	case r.narrationTried, r.errSeen, r.toolSeen, r.readErr != nil:
		return false
	case finish != "stop":
		return false
	case r.text.Len() == 0:
		return false
	case r.seg >= r.segBudget-1:
		return false
	case r.limitTok >= 0 && r.cumTok() >= float64(r.limitTok):
		return false
	}
	txt := r.text.String()
	// 模型在等用户拍板：故意的收尾，补「继续」等于替用户按下发送键。
	if textYieldsToUser(txt) {
		return false
	}
	// 长度闸门只压「标点型尾部」；结构上没写完的不限（见 narrationMaxBytes）。
	if !narrationTailStructurallyOpen(txt) && len(txt) > narrationMaxBytes {
		return false
	}
	if !narrationTailSuspends(txt) {
		return false
	}
	declared, toolTurn, _ := bodyToolShape(r.body)
	return declared && toolTurn
}
