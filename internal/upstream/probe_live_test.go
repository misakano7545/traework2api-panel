package upstream

// probe_live_test.go 只读诊断：打印 get_detail_param 的**决策表**与 ide_user_ent_usage 的结构，
// 用于复核「哪些是账号真正能用的官方模型」，以及上游表是否变了。
//
// 只读查询，不签到、不改任何本地状态，不打印任何凭据。
// 默认跳过，手动跑：TW2A_PROBE=1 go test ./internal/upstream -run TestProbeLive -v
//
// 2026-09 实测：上游 39 条配置 = 15 条官方 + 9 条 is_invisible_to_user（子代理等）
// + 14 条 custom_model_* 槽位 + 1 条 summary。过滤口径见 pickOfficialModels。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"traework2api/internal/auth"
)

func probeKeys(m map[string]any) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

func TestProbeLive(t *testing.T) {
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
	t.Logf("账号 uid=%s（不打印 token）", a.UID)

	c := New()

	// ---------- 1. get_detail_param 决策表 ----------
	body := map[string]any{
		"function": Function, "config_names": nil, "need_prompt": false,
		"current_config_info": nil, "poly_prompt": true,
		"mode_type": nil, "agent_type": nil,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.AgentHost+EpModels, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	SOLOHeaders(req, a, false)
	data, err := c.doJSON(req)
	if err != nil {
		t.Fatalf("get_detail_param: %v", err)
	}
	var top struct {
		ConfigInfoList []paramConfig `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("get_detail_param parse: %v", err)
	}
	lst := top.ConfigInfoList
	fmt.Printf("\n===== get_detail_param: 上游返回 %d 条\n", len(lst))
	fmt.Println("----- name | invis | usage | ctx | keep | display_name | capability | thinking")
	kept := 0
	for _, cfg := range lst {
		keep := !cfg.IsInvisibleToUser && cfg.Usage == usageChat && cfg.ConfigName != ""
		mark := "."
		if keep {
			mark = "KEEP"
			kept++
		}
		think := ""
		if len(cfg.ModelDetailList) > 0 {
			think = thinkingType(cfg.ModelDetailList[0].ModelExtraConfig)
		}
		effort := ""
		if len(cfg.ReasoningEffortConfig) > 0 && string(cfg.ReasoningEffortConfig) != "null" {
			effort = string(cfg.ReasoningEffortConfig)
		}
		fmt.Printf("  %-32s %-5v %-16s %-8d %-4s %s | %s | %s %s\n",
			cfg.ConfigName, cfg.IsInvisibleToUser, cfg.Usage,
			cfg.ContextWindowTokens.Dev, mark, cfg.DisplayConfig.DisplayName,
			cfg.DisplayConfig.ModelCapability, think, effort)
	}
	fmt.Printf("----- 官方模型 %d 条（= pickOfficialModels 的结果数）\n", kept)

	// 直接调被改的那个函数，确认线上走查出来的就是过滤后的官方模型。
	if infos, ferr := c.FetchModels(a); ferr != nil {
		fmt.Printf("----- FetchModels 错误: %v\n", ferr)
	} else {
		fmt.Printf("----- FetchModels 返回 %d 条:", len(infos))
		for _, mi := range infos {
			fmt.Printf(" %s/%d", mi.ID, mi.ContextWindow)
		}
		fmt.Println()
	}

	// ---------- 3. 思考强度档位：原样 dump（低/中/高这类到底有没有上游来源） ----------
	var rawTop struct {
		ConfigInfoList []map[string]any `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &rawTop); err == nil {
		keys := map[string]any{}
		for _, cfg := range rawTop.ConfigInfoList {
			for k := range cfg {
				keys[k] = true
			}
		}
		fmt.Printf("\n===== 配置项顶层字段并集: %s\n", probeKeys(keys))
		fmt.Println("----- 官方模型原样（找思考强度档位）")
		for _, cfg := range rawTop.ConfigInfoList {
			if cfg["usage"] != usageChat || cfg["is_invisible_to_user"] == true {
				continue
			}
			fmt.Printf("  %-32s reasoning_effort_config=%v\n", cfg["config_name"], cfg["reasoning_effort_config"])
			fmt.Printf("      display_config=%v\n      extra_config=%v\n      config_switch=%v\n",
				cfg["display_config"], cfg["extra_config"], cfg["config_switch"])
			if dl, ok := cfg["model_detail_list"].([]any); ok && len(dl) > 0 {
				if dm, ok := dl[0].(map[string]any); ok {
					extra, _ := dm["model_extra_config"].(string)
					if len(extra) > 300 {
						extra = extra[:300] + "…（截断）"
					}
					fmt.Printf("      detail keys: [%s]\n      model_extra_config=%.300s\n",
						probeKeys(dm), extra)
				}
			}
		}
	}

	// ---------- 2. ide_user_ent_usage：{} vs 真实客户端体 ----------
	for _, b := range []string{`{}`, `{"require_usage":true,"full_data":true}`} {
		req, rerr := http.NewRequest(http.MethodPost, c.UgHost+EpEntUsage, bytes.NewReader([]byte(b)))
		if rerr != nil {
			t.Fatal(rerr)
		}
		UgHeaders(req, a)
		data, err := c.doJSON(req)
		fmt.Printf("\n===== ide_user_ent_usage body=%s\n", b)
		if err != nil {
			fmt.Printf("  错误: %v\n", err)
			continue
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			fmt.Printf("  非 JSON: %.200s\n", string(data))
			continue
		}
		fmt.Printf("  顶层字段: %s\n", probeKeys(m))
		if pl, ok := m["user_entitlement_pack_list"].([]any); ok {
			fmt.Printf("  pack 数量: %d\n", len(pl))
			for _, p := range pl {
				if pm, ok := p.(map[string]any); ok {
					fmt.Printf("    pack 字段: %s\n", probeKeys(pm))
				}
			}
		}
	}
}

// TestProbeLiveReasoningEffort 花额度的一次性探测：上游 llm_utils_chat 到底认不认客户端传来的
// reasoning_effort？原理是发个非法值——上游要是解析这个字段就会 400，静默忽略就是 200。
// 加一个控制组（随机未知键）才能定责：万一 400 是因为「拒绝一切未知字段」，那就不是认得 effort，
// 而是我们透传客户端设置会让请求直接失败（更糟，得改）。
//
// 三次极短请求（prompt "hi"、max_tokens 8）：
//
//	A 基线，无额外字段           200 → 链路本身通
//	B reasoning_effort=__bogus__ 400 → 上游解析该字段 / 200 → 静默忽略
//	C 未知键 __x_unknown_key__   400 → 上游拒绝一切未知字段（那 B 的 400 不算数）
//
// 手动跑：TW2A_PROBE_CHAT=1 go test ./internal/upstream -run TestProbeLiveReasoningEffort -v
// 换模型：TW2A_PROBE_MODEL=doubao-seed-evolving（默认 kimi-k3，它的 Thinking.Type=enabled）
func TestProbeLiveReasoningEffort(t *testing.T) {
	if os.Getenv("TW2A_PROBE_CHAT") == "" {
		t.Skip("置 TW2A_PROBE_CHAT=1 才跑（真实发聊天请求，吃额度）")
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
	// 探测用的短请求不该挂死：给个总超时（生产流式客户端故意不设）。
	c.StreamHTTP = &http.Client{Timeout: 60 * time.Second, Transport: c.StreamHTTP.Transport}

	model := os.Getenv("TW2A_PROBE_MODEL")
	if model == "" {
		model = "kimi-k3"
	}
	base := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"max_tokens":8`
	short := func(b []byte) string {
		if len(b) > 240 {
			b = b[:240]
		}
		return string(b)
	}
	// send 只发不回放：SSE 读 240 字节就掐（够看开头，不整条烧完输出）。
	send := func(body string) (status int, head, rejected []byte) {
		rc, st, respBody, err := c.ChatStream(a, []byte(body))
		if err != nil {
			return 0, nil, []byte(err.Error())
		}
		if rc != nil {
			b, _ := io.ReadAll(io.LimitReader(rc, 240))
			rc.Close()
			return st, b, nil
		}
		return st, nil, respBody
	}

	// A 基线兼「账号还活着吗」探针：session 死的账号上游 401，不花额度，直接换下一个。
	fmt.Printf("\n===== 探测 %s：上游认不认 reasoning_effort（每请求 prompt=hi, max_tokens=8）\n", model)
	live := false
	for _, cand := range as {
		a = cand
		status, _, rejected := send(base + "}")
		fmt.Printf("----- A 基线（账号 %s，无额外字段）→ %d %s\n", a.UID, status, short(rejected))
		if status < 400 {
			live = true
			break
		}
		fmt.Println("      这个账号不可用，换下一个")
	}
	if !live {
		t.Fatal("没有可用账号，探测无意义（先跑 cmd/signin 刷新 token）")
	}

	for _, tc := range []struct{ label, extra string }{
		{"B reasoning_effort=__bogus__（非法枚举值）", `,"reasoning_effort":"__bogus__"`},
		{"C 控制组：未知键 __x_unknown_key__", `,"__x_unknown_key__":"1"`},
		// D/E 用「类型不对」比枚举值更硬：上游 struct 里真有 reasoning_effort 这个字段
		// （哪怕类型是 string），塞个数字进去就会反序列化失败 → 400。
		// E 是对照：拿一个上游肯定有的字段（max_tokens）塞字符串，验证这个端点的类型错误确实会 400，
		// 否则 D 的 200 什么也证明不了。
		{"D reasoning_effort=12345（类型不对）", `,"reasoning_effort":12345`},
		{"E 对照：max_tokens=\"eight\"（已知字段，类型不对）", `,"max_tokens":"eight"`},
	} {
		body := base + tc.extra + "}"
		status, head, rejected := send(body)
		fmt.Printf("----- %s\n  发出: %s\n  → %d ", tc.label, body, status)
		if rejected != nil {
			fmt.Printf("拒绝，上游原话: %s\n", short(rejected))
			continue
		}
		fmt.Printf("通了，SSE 前 240 字节: %s\n", short(head))
	}
}
