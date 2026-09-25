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
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

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
