package upstream

// advisor_probe_test.go 只读/低耗诊断：TRAE 的「专家模型」（AdvisorTool）实测。
//
//   TestAdvisorDump  逐模型导出 advisor_tool 配置：哪些模型带专家、专家是谁、单次上限几次。
//   TestAdvisorTrigger 真发 4 次短请求，验证专家是「客户端声明出来的工具」还是「服务端自己注入」。
//
// 默认跳过，手动跑：
//
//	TW2A_PROBE=1 TW2A_AUTH_DIR=./auths go test ./internal/upstream -run 'TestAdvisor' -v
//
// 2026-10-23 真号（CN，20 个官方模型）实测结论：
//   - 专家是**按 (function 面 × 模型)** 挂在模型配置里的：`model_extra_config.advisor_tool`
//     带 `advisor_config_name` / `advisor_model` / `max_uses_per_request=3` / `trim_*`。
//   - 面板默认面 `solo_work_lite` 的 20 个官方模型**一个都不带**；`chat_v3` 21 个里 1 个带
//     （Doubao-Seed-Code）；`solo_agent` 21 个里 **9 个带**（DeepSeek 三兄弟、MiniMax-M3 两兄弟、
//     qwen3.8-flash / qwen3.8-max / qwen-3.7-plus、Doubao-Seed-Code）；`solo_coder` 12 个都不带。
//   - 但「带配置」≠「会跑」：裸 API 请求（只有 messages + tools）的 prompt_tokens 只有 36~41，
//     流里没有任何 advisor 事件，provider_model_name 也只有主模型一个 —— 服务端**不会**自己
//     把方案送去给专家。
//   - 触发条件是客户端把 `AdvisorTool` 声明成普通 function：同一句「上次方案不行，我很烦」，
//     不声明 → 0 次调用；声明 → 必然调用（14~25 帧 tool_calls，带 question 参数）。
//     论坛抓到的 `advisor_tool_policy` 策略文本只是加剧，不是必要条件（B 组不声明、只给策略，
//     模型只能嘴上提 AdvisorTool，调不出来）。
//   - 积分（TestAdvisorCredit 实测，2026-10-23）：额度接口 usage.credits_amount 是浮点、
//     45 秒内就结算。两臂各 4 次、prompt 都约 12.5k token：
//     A 主模型 deepseek-v4.1-flash（倍率 0.06）12916+323 → 0.5684 积分；
//     B 专家槽 glm-5.2_advisor_default（倍率 0.78）12356+1498 → 3.8024 积分。
//     同样输入量专家单次 ≈ 主模型 6.7 倍（token 单价 13 倍 + 输出更长）。
//     面板显示整数，小请求差值被取整吃掉，才显得「结算很慢」——别拿整数位当计费依据。
//
// ⇒ 所以中继侧唯一有效的拦截点是「出站不让这个工具存在」，见 payload.go 的
// normalizeTools / isAdvisorToolDecl（回归测试 TestPrepareBodyDropsAdvisorTool）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"traework2api/internal/auth"
)

// advisorOf 从一条模型配置里取 advisor_tool（第一个带它的 model_detail 的 model_extra_config）。
func advisorOf(cfg map[string]any) (map[string]any, bool) {
	dets, _ := cfg["model_detail_list"].([]any)
	for _, d := range dets {
		dm, _ := d.(map[string]any)
		ex, _ := dm["model_extra_config"].(string)
		if ex == "" {
			continue
		}
		var j map[string]any
		if json.Unmarshal([]byte(ex), &j) != nil {
			continue
		}
		if at, ok := j["advisor_tool"].(map[string]any); ok {
			return at, true
		}
	}
	return nil, false
}

func TestAdvisorDump(t *testing.T) {
	if os.Getenv("TW2A_PROBE") == "" {
		t.Skip("置 TW2A_PROBE=1 才跑（需要真实凭据与网络）")
	}
	dir := os.Getenv("TW2A_AUTH_DIR")
	if dir == "" {
		dir = "../../auths"
	}
	as, err := auth.LoadDir(dir)
	if err != nil || len(as) == 0 {
		t.Fatalf("加载账号失败: %v (dir=%s)", err, dir)
	}
	a := as[0]
	c := New()
	fmt.Printf("账号 uid=%s realm=%s（不打印 token）\n", a.UID, a.Realm())

	for _, fn := range []string{Function, "chat_v3", "solo_agent", FunctionCoder} {
		body, _ := json.Marshal(map[string]any{
			"function": fn, "config_names": nil, "need_prompt": false,
			"current_config_info": nil, "poly_prompt": true, "mode_type": nil, "agent_type": nil,
		})
		req, _ := http.NewRequest(http.MethodPost, c.agentBaseFor(a)+EpModels, bytes.NewReader(body))
		SOLOHeaders(req, a, false)
		data, err := c.doJSON(req)
		if err != nil {
			fmt.Printf("\n===== %s 失败: %v\n", fn, err)
			continue
		}
		var top struct {
			ConfigInfoList []map[string]any `json:"config_info_list"`
		}
		if err := json.Unmarshal(data, &top); err != nil {
			fmt.Printf("\n===== %s parse 失败: %v\n", fn, err)
			continue
		}
		byName := map[string]map[string]any{}
		for _, cfg := range top.ConfigInfoList {
			if n, _ := cfg["config_name"].(string); n != "" {
				byName[n] = cfg
			}
		}
		var typed struct {
			ConfigInfoList []paramConfig `json:"config_info_list"`
		}
		if err := json.Unmarshal(data, &typed); err != nil {
			t.Fatal(err)
		}
		official := pickOfficialModels(typed.ConfigInfoList, a, fn)
		withAdv := 0
		fmt.Printf("\n===== function=%s：全表 %d 条，官方可选 %d 条\n", fn, len(top.ConfigInfoList), len(official))
		for _, mi := range official {
			cfg := byName[mi.ID]
			if cfg == nil {
				continue
			}
			at, ok := advisorOf(cfg)
			if !ok {
				continue
			}
			en, _ := at["enabled"].(bool)
			if !en {
				continue
			}
			withAdv++
			mu, _ := at["max_uses_per_request"].(float64)
			acn, _ := at["advisor_config_name"].(string)
			am, _ := at["advisor_model"].(string)
			fmt.Printf("  %-30s %-22s %s / %s / max_uses=%v\n", mi.ID, mi.Name, acn, am, mu)
		}
		fmt.Printf("  → 官方模型里带专家 %d 个 / 共 %d 个\n", withAdv, len(official))
	}
}

const advisorPolicy = `## 工具使用策略
在以下情况必须立刻调用 AdvisorTool（单独调用，不要与其他工具并行）：用户卡住/报错、更换方案、
工具反复失败、用户表达负面情绪。调用时转发全部对话历史。拿到专家建议后必须认真采纳。`

func advisorRun(t *testing.T, c *Client, a *auth.Auth, label string, withAdvisor, withPolicy bool) {
	tools := []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "noop_probe", "description": "no-op probe tool",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}}
	if withAdvisor {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "AdvisorTool",
				"description": "咨询专家模型：把当前方案与全部对话历史交给专家，取回建议。",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"question": map[string]any{"type": "string"},
					},
					"required": []string{"question"},
				},
			},
		})
	}
	msgs := []any{}
	if withPolicy {
		msgs = append(msgs, map[string]any{"role": "system", "content": advisorPolicy})
	}
	msgs = append(msgs, map[string]any{
		"role":    "user",
		"content": "你上次给的方案完全不行，我照着做了三次全失败，真的很烦。现在立刻给我一个能用的做法。",
	})
	body, _ := json.Marshal(map[string]any{
		"config_name": "deepseek-v4.1-flash",
		"model":       "deepseek-v4.1-flash__dev",
		"function":    "solo_agent",
		"stream":      true,
		"max_tokens":  400,
		"messages":    msgs,
		"tools":       tools,
	})
	rc, st, eb, err := c.ChatStream(a, body)
	if err != nil || rc == nil {
		fmt.Printf("\n----- %s: status=%d err=%v body=%s\n", label, st, err, truncate(string(eb), 300))
		return
	}
	defer rc.Close()
	b, _ := io.ReadAll(io.LimitReader(rc, 4<<20))
	var promptTok float64
	calls := 0
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var j map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &j) != nil {
			continue
		}
		if v, ok := j["prompt_tokens"].(float64); ok {
			promptTok = v
		}
		if tcs, ok := j["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				if m, ok := tc.(map[string]any); ok {
					if fc, ok := m["function_call"].(map[string]any); ok {
						if strings.EqualFold(asString(fc["name"]), "AdvisorTool") {
							calls++
						}
					}
				}
			}
		}
	}
	fmt.Printf("\n----- %s: status=%d prompt_tokens=%.0f AdvisorTool 调用帧=%d\n", label, st, promptTok, calls)
}

func TestAdvisorTrigger(t *testing.T) {
	if os.Getenv("TW2A_PROBE") == "" {
		t.Skip("置 TW2A_PROBE=1 才跑（需要真实凭据与网络）")
	}
	dir := os.Getenv("TW2A_AUTH_DIR")
	if dir == "" {
		dir = "../../auths"
	}
	as, err := auth.LoadDir(dir)
	if err != nil || len(as) == 0 {
		t.Fatalf("加载账号失败: %v (dir=%s)", err, dir)
	}
	a := as[0]
	c := New()

	advisorRun(t, c, a, "A 只有普通工具（无策略）", false, false)
	advisorRun(t, c, a, "B 普通工具 + 策略文本", false, true)
	advisorRun(t, c, a, "C 声明 AdvisorTool（无策略）", true, false)
	advisorRun(t, c, a, "D 声明 AdvisorTool + 策略文本", true, true)
}

// ---- 积分实测 -------------------------------------------------------------

const advisorFiller = "The relay forwards the plan to the upstream expert and folds the cost into the task. " +
	"Every call re-sends the trimmed history, so the expert pays for the whole conversation again. " +
	"Rates differ per model and the settlement counter is updated asynchronously in whole credits. "

// advisorBigPrompt 造一段约 3.2k token 的输入；nonce 放在最前面，用来打掉 prompt 前缀缓存
// （否则第二轮会命中缓存，测出来的单价偏低）。
func advisorBigPrompt(nonce string) string {
	var b strings.Builder
	b.WriteString("NONCE " + nonce + "\n\n")
	for i := 0; i < 60; i++ {
		b.WriteString(advisorFiller)
	}
	b.WriteString("\n用一句话回答：幂等性是什么？")
	return b.String()
}

// advisorUsedRaw 读额度接口，把每个包的 credits_amount 求和。
//
// **别用面板显示的整数余额做这件事**：那是取整后的值，几十 token 的小请求差值直接消失，
// 会让人误以为「结算延迟十几分钟」。credits_amount 本身是 4 位浮点、45 秒内就更新。
func advisorUsedRaw(t *testing.T, c *Client, a *auth.Auth) (float64, string) {
	req, err := http.NewRequest(http.MethodPost, c.ugBaseFor(a)+entUsagePath(a), bytes.NewReader([]byte(entUsageBody)))
	if err != nil {
		t.Fatal(err)
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		t.Fatalf("ent usage: %v", err)
	}
	var j struct {
		Packs []struct {
			Name  string `json:"display_desc"`
			Usage struct {
				Credits float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(data, &j); err != nil {
		t.Fatal(err)
	}
	var sum float64
	var parts []string
	for _, p := range j.Packs {
		sum += p.Usage.Credits
		if p.Usage.Credits > 0 {
			parts = append(parts, fmt.Sprintf("%s=%.4f", p.Name, p.Usage.Credits))
		}
	}
	return sum, strings.Join(parts, " ")
}

// advisorArm 连发 n 次同尺寸请求，返回 (prompt 合计, completion 合计)。
func advisorArm(t *testing.T, c *Client, a *auth.Auth, cfg, model, tag string, n int) (int64, int64) {
	var ptSum, ctSum int64
	for i := 0; i < n; i++ {
		body, _ := json.Marshal(map[string]any{
			"config_name": cfg,
			"model":       model,
			"function":    "solo_agent",
			"stream":      true,
			"max_tokens":  64,
			"messages": []any{
				map[string]any{"role": "user", "content": advisorBigPrompt(fmt.Sprintf("%s-%d", tag, i))},
			},
		})
		rc, st, eb, err := c.ChatStream(a, body)
		if err != nil || rc == nil {
			fmt.Printf("  %s #%d 失败 status=%d err=%v body=%s\n", tag, i, st, err, truncate(string(eb), 160))
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(rc, 4<<20))
		rc.Close()
		var pt, ct float64
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var j map[string]any
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &j) != nil {
				continue
			}
			if v, ok := j["prompt_tokens"].(float64); ok {
				pt = v
			}
			if v, ok := j["completion_tokens"].(float64); ok {
				ct = v
			}
		}
		ptSum += int64(pt)
		ctSum += int64(ct)
		fmt.Printf("  %s #%d status=%d prompt=%.0f completion=%.0f\n", tag, i, st, pt, ct)
	}
	return ptSum, ctSum
}

// advisorSettle 轮询到 used 相对 base 涨够 minDelta 为止。
func advisorSettle(t *testing.T, c *Client, a *auth.Auth, base, minDelta float64, max time.Duration) (float64, time.Duration) {
	start := time.Now()
	for {
		time.Sleep(30 * time.Second)
		cur, _ := advisorUsedRaw(t, c, a)
		d := cur - base
		fmt.Printf("    [%4.0fs] used=%.4f delta=%+.4f\n", time.Since(start).Seconds(), cur, d)
		if d >= minDelta || time.Since(start) > max {
			return cur, time.Since(start)
		}
	}
}

// TestAdvisorCredit 实测「专家槽」与「主模型」的积分单价（**会花钱**，默认不跑）。
//
//	TW2A_PROBE=1 TW2A_CREDIT=1 TW2A_AUTH_DIR=./auths go test ./internal/upstream -run TestAdvisorCredit -v
//
// 一次约 4.4 积分（8 次 ~12.5k token 请求）。结果见文件头注释。
func TestAdvisorCredit(t *testing.T) {
	if os.Getenv("TW2A_PROBE") == "" || os.Getenv("TW2A_CREDIT") == "" {
		t.Skip("置 TW2A_PROBE=1 TW2A_CREDIT=1 才跑（真花钱）")
	}
	dir := os.Getenv("TW2A_AUTH_DIR")
	if dir == "" {
		dir = "../../auths"
	}
	as, err := auth.LoadDir(dir)
	if err != nil || len(as) == 0 {
		t.Fatalf("加载账号失败: %v (dir=%s)", err, dir)
	}
	a := as[0]
	c := New()
	n := 4
	if v := os.Getenv("TW2A_N"); v != "" {
		fmt.Sscanf(v, "%d", &n)
	}

	base0, parts0 := advisorUsedRaw(t, c, a)
	fmt.Printf("\n===== 基线 used=%.4f\n     %s\n", base0, parts0)

	ptA, ctA := advisorArm(t, c, a, "deepseek-v4.1-flash", "deepseek-v4.1-flash__dev", "A主模型", n)
	fmt.Printf("  A 臂合计 prompt=%d completion=%d，等结算…\n", ptA, ctA)
	afterA, latA := advisorSettle(t, c, a, base0, 0.01, 10*time.Minute)
	dA := afterA - base0

	ptB, ctB := advisorArm(t, c, a, "glm-5.2_advisor_default", "glm-5.2__dev", "B专家", n)
	fmt.Printf("  B 臂合计 prompt=%d completion=%d，等结算…\n", ptB, ctB)
	afterB, latB := advisorSettle(t, c, a, afterA, 0.01, 10*time.Minute)
	dB := afterB - afterA

	fmt.Printf("\n===== 结果\n")
	fmt.Printf("A 主模型 deepseek-v4.1-flash: %d 次 prompt=%d completion=%d → %.4f 积分（结算 %s）\n",
		n, ptA, ctA, dA, latA.Round(time.Second))
	fmt.Printf("B 专家   glm-5.2_advisor_default: %d 次 prompt=%d completion=%d → %.4f 积分（结算 %s）\n",
		n, ptB, ctB, dB, latB.Round(time.Second))
	if ptA > 0 && ptB > 0 && dA > 0 {
		fmt.Printf("→ 同输入量专家/主模型 = %.1f 倍\n", (dB/float64(ptB))/(dA/float64(ptA)))
	}
}
