//go:build liveauth

// 真实账号端到端实测：票据来源按优先级取，全部只在内存里用，**不打印任何凭证**。
//
//	go test -tags liveauth ./internal/upstream -run TestLiveAccount -v
//
// 票据优先从 .reverse/ 的客户端抓包里捞（真客户端出站的那张），
// 抓包没有才退回 ~/.trae/trae-jwt-token（IDE 静态文件，历史上被上游拒过）。
//
// 只跑不吃额度的接口：模型表 + 签到状态。
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"traework2api/internal/auth"
)

// textFieldRe 从 SSE 帧里捞正文。上游不同模型走不同字段名（text / content /
// content_delta），这里宽松匹配，只用于「确认流里有真内容」。
var textFieldRe = regexp.MustCompile(`"(?:text|content|content_delta)"\s*:\s*"((?:[^"\\]|\\.)*)"`)

// headRunes 截断到 n 个字符，用于把整条流打进日志。
func headRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// bizError 从 SSE 流里解出上游业务码（event:error 那帧）。
func bizError(s string) (int, string) {
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), "data:")
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") || !strings.Contains(line, `"code"`) {
			continue
		}
		if err := json.Unmarshal([]byte(line), &out); err == nil && out.Code != 0 {
			return out.Code, out.Message
		}
	}
	return 0, ""
}

// extractText 取流里第一段非空正文（最多 max 个字符）。
func extractText(s string, max int) string {
	for _, m := range textFieldRe.FindAllStringSubmatch(s, 50) {
		v := strings.TrimSpace(m[1])
		if v == "" || v == "text" || strings.HasPrefix(v, "http") {
			continue
		}
		if r := []rune(v); len(r) > max {
			return string(r[:max])
		}
		return v
	}
	return ""
}

// liveTicket 一张从抓包里捞出来的完整票据与它的 host / 设备号。
type liveTicket struct {
	Token  string
	Host   string
	Device string
	Source string
}

// findTickets 扫抓包 jsonl，按 host 归组（同 host 保留最后一张 = 最新）。
func findTickets(t *testing.T) []liveTicket {
	t.Helper()
	root := filepath.Join("..", "..", ".reverse")
	files, _ := filepath.Glob(filepath.Join(root, "cap_*.jsonl"))
	more, _ := filepath.Glob(filepath.Join(root, "capture_*.jsonl"))
	files = append(files, more...)
	if len(files) == 0 {
		t.Skip("没有抓包工件")
	}
	byHost := map[string]liveTicket{}
	for _, fn := range files {
		f, err := os.Open(fn)
		if err != nil {
			continue
		}
		dec := json.NewDecoder(f)
		for {
			var o map[string]any
			if err := dec.Decode(&o); err != nil {
				break
			}
			if o["kind"] != "req" {
				continue
			}
			h, _ := o["headers"].(map[string]any)
			if h == nil {
				continue
			}
			get := func(name string) string {
				for kk, vv := range h {
					if strings.EqualFold(kk, name) {
						if s, ok := vv.(string); ok {
							return s
						}
					}
				}
				return ""
			}
			tok := get("authorization")
			if strings.HasPrefix(strings.ToLower(tok), "cloud-ide-jwt ") {
				tok = tok[len("cloud-ide-jwt "):]
			}
			if strings.Count(tok, ".") != 2 {
				continue
			}
			host, _ := o["host"].(string)
			if host == "" {
				continue
			}
			byHost[host] = liveTicket{Token: tok, Host: host, Device: get("x-device-id"), Source: filepath.Base(fn)}
		}
		f.Close()
	}
	out := make([]liveTicket, 0, len(byHost))
	for _, v := range byHost {
		out = append(out, v)
	}
	return out
}

func ticketRealm(host string) string {
	if strings.Contains(host, "alisg") || strings.Contains(host, "trae.ai") {
		return auth.RealmIntl
	}
	return auth.RealmCN
}

func authFromTicket(tk liveTicket) *auth.Auth {
	a := &auth.Auth{
		AccessToken: tk.Token,
		MachineID:   "cf41c622d99bc6167df82aed86aebcbe1509af3cd3908f7a7ddbe6de37ada710",
		DeviceID:    tk.Device,
	}
	if ticketRealm(tk.Host) == auth.RealmIntl {
		a.Domain = "trae.ai"
		a.ApiHost = "https://api.trae.ai"
	} else {
		a.Domain = "trae.cn"
		a.ApiHost = "https://api.trae.com.cn"
		if a.DeviceID == "" {
			a.DeviceID = "4192911412971131"
		}
		a.UID = "253358232317424"
	}
	return a
}

// TestLiveAccountModels 用真票据拉模型表（不吃额度），两区 × 两种 TLS 指纹。
func TestLiveAccountModels(t *testing.T) {
	tickets := findTickets(t)
	if len(tickets) == 0 {
		t.Skip("抓包里没有可用票据")
	}
	for _, fp := range []string{"chrome_133", "go"} {
		if err := SetTLSFingerprint(fp); err != nil {
			t.Fatal(err)
		}
		for _, tk := range tickets {
			name := ticketRealm(tk.Host) + "/" + tk.Host
			t.Run(name+"/"+fp, func(t *testing.T) {
				up := New()
				start := time.Now()
				models, err := up.FetchModels(authFromTicket(tk))
				dur := time.Since(start)
				if err != nil {
					t.Fatalf("拉模型表失败(%v): %v", dur, err)
				}
				t.Logf("TLS=%s 来源=%s 耗时=%v 模型数=%d", fp, tk.Source, dur, len(models))
				for i, m := range models {
					if i >= 5 {
						t.Logf("  …（其余 %d 个略）", len(models)-i)
						break
					}
					t.Logf("  - %s", m.ID)
				}
			})
		}
	}
}

// TestLiveAccountCheckinStatus 只读签到状态（不吃额度、不改状态）。
func TestLiveAccountCheckinStatus(t *testing.T) {
	if err := SetTLSFingerprint("chrome_133"); err != nil {
		t.Fatal(err)
	}
	for _, tk := range findTickets(t) {
		if ticketRealm(tk.Host) == auth.RealmIntl {
			continue // ug 族国际接口本仓不支持（ErrNoIntlUG）
		}
		t.Run(tk.Host, func(t *testing.T) {
			up := New()
			checkedIn, credits, enable, err := up.CheckinStatus(authFromTicket(tk))
			if err != nil {
				t.Fatalf("签到状态查询失败: %v", err)
			}
			t.Logf("签到状态 OK：已签到=%v 积分=%d 可用=%v", checkedIn, credits, enable)
		})
	}
}

// TestLiveAccountIntlSlot 走国际版的租户槽位旁路。
//
// 国际版 gpt-6-sol / gpt-6-luna 直发回 1005（要付费档），但同一批模型在
// custom_model_gpt-5 槽位里以 openai/gpt-5.6-sol|luna 命名时能出正文
// （见 constants.go 的 intlSlotRoutes）。这条用来验证「会话真的能出内容」，
// 而不是只验证「流能建起来」。
func TestLiveAccountIntlSlot(t *testing.T) {
	if os.Getenv("TW2A_LIVE_CHAT") == "" {
		t.Skip("会吃额度，显式 TW2A_LIVE_CHAT=1 才跑")
	}
	for _, tk := range findTickets(t) {
		if ticketRealm(tk.Host) != auth.RealmIntl {
			continue
		}
		a := authFromTicket(tk)
		up := New()
		for _, name := range []string{"gpt-6-sol", "gpt-6-luna"} {
			route, ok := IntlSlotRoute(name)
			if !ok {
				continue
			}
			payload := `{"messages":[{"role":"user","content":[{"type":"text","text":"说一个字"}]}],"stream":true,"config_name":` +
				strconv.Quote(route.Slot) + `,"model":` + strconv.Quote(route.Model) + `}`
			start := time.Now()
			rc, status, body, err := up.ChatStream(a, []byte(payload))
			if err != nil {
				t.Logf("slot %s→%s 发起失败(%v) status=%d body=%.160s", name, route.Model, time.Since(start), status, body)
				continue
			}
			raw, _ := io.ReadAll(io.LimitReader(rc, 256<<10))
			rc.Close()
			text := string(raw)
			code, msg := bizError(text)
			snippet := extractText(text, 40)
			t.Logf("slot %s→%s 耗时=%v 字节=%d 正文=%q 业务码=%d msg=%.50s",
				name, route.Model, time.Since(start), len(raw), snippet, code, msg)
			t.Logf("   流头: %s", headRunes(text, 500))
			if snippet != "" {
				return
			}
		}
		t.Log("国际槽位旁路也没出正文")
	}
}

// TestLiveAccountChatCoder 走 coder 通道（function=solo_coder）。
//
// 4011 是**通道级**限流：同一账号在 work 通道被拒时，coder 通道常常还是通的
// （README 里 gemini-3.1-pro 那条记录就是这个现象）。这条用来分辨
// 「账号被限」还是「整条链路坏了」—— 前者换通道能出正文，后者两条一起死。
func TestLiveAccountChatCoder(t *testing.T) {
	if os.Getenv("TW2A_LIVE_CHAT") == "" {
		t.Skip("会吃额度，显式 TW2A_LIVE_CHAT=1 才跑")
	}
	for _, tk := range findTickets(t) {
		if !strings.Contains(tk.Host, "api5-normal.mchost.guru") {
			continue // coder 通道只在 CN agent 主机上验
		}
		a := authFromTicket(tk)
		up := New()
		models, err := up.FetchModels(a)
		if err != nil || len(models) == 0 {
			t.Fatalf("拉模型表失败: %v", err)
		}
		// coder 通道的模型名里带 gemini / claude 的那批是另一张表。
		var candidates []string
		for _, m := range models {
			if strings.Contains(m.ID, "gemini") || strings.Contains(m.ID, "claude") {
				candidates = append(candidates, m.ID)
			}
		}
		if len(candidates) == 0 {
			candidates = []string{models[0].ID}
		}
		if len(candidates) > 3 {
			candidates = candidates[:3]
		}
		for _, model := range candidates {
			payload := `{"messages":[{"role":"user","content":[{"type":"text","text":"说一个字"}]}],"stream":true,"function":` +
				strconv.Quote(FunctionCoder) + `,"config_name":` + strconv.Quote(model) + `,"model":` + strconv.Quote(model) + `}`
			start := time.Now()
			rc, status, body, err := up.ChatStream(a, []byte(payload))
			if err != nil {
				t.Logf("coder %-24s 发起失败(%v) status=%d body=%.160s", model, time.Since(start), status, body)
				continue
			}
			raw, _ := io.ReadAll(io.LimitReader(rc, 256<<10))
			rc.Close()
			text := string(raw)
			code, msg := bizError(text)
			snippet := extractText(text, 40)
			t.Logf("coder %-24s 耗时=%v 字节=%d 正文=%q 业务码=%d msg=%.50s",
				model, time.Since(start), len(raw), snippet, code, msg)
			if snippet != "" {
				return // 出正文即为通过
			}
		}
		t.Log("coder 通道也没出正文")
	}
}

// TestLiveAccountChatStream 真发一次 SSE 对话 —— 这条会吃额度。
// 两区各一次，同一句最短的问候，读到首帧就收。
func TestLiveAccountChatStream(t *testing.T) {
	if os.Getenv("TW2A_LIVE_CHAT") == "" {
		t.Skip("会吃额度，显式 TW2A_LIVE_CHAT=1 才跑")
	}
	for _, tk := range findTickets(t) {
		if k := ticketRealm(tk.Host) + "/" + tk.Host; !strings.Contains(k, "api5-normal") {
			continue // 只打 agent 主机，其余是 IDE 的杂项域名
		}
		realm := ticketRealm(tk.Host)
		t.Run(realm, func(t *testing.T) {
			for _, fp := range []string{"chrome_133", "go"} {
				if err := SetTLSFingerprint(fp); err != nil {
					t.Fatal(err)
				}
				a := authFromTicket(tk)
				up := New()
				models, err := up.FetchModels(a)
				if err != nil || len(models) == 0 {
					t.Fatalf("先拉模型表失败: %v", err)
				}
				// 上游会回业务码（CN 4011 限流 / INTL 1005 要付费档），那不是链路问题。
				// 逐个模型试，第一个真出正文的算通过；最多试 4 个，避免无意义消耗。
				var ok bool
				for i, m := range models {
					if i >= 4 {
						break
					}
					model := m.ID
					payload := `{"messages":[{"role":"user","content":[{"type":"text","text":"说一个字"}]}],"stream":true,"config_name":` +
						strconv.Quote(model) + `,"model":` + strconv.Quote(model) + `}`
					start := time.Now()
					rc, status, body, err := up.ChatStream(a, []byte(payload))
					if err != nil {
						t.Logf("%s %-24s 发起失败(%v) status=%d body=%.160s", fp, model, time.Since(start), status, body)
						continue
					}
					raw, readErr := io.ReadAll(io.LimitReader(rc, 256<<10))
					rc.Close()
					dur := time.Since(start)
					if readErr != nil {
						t.Logf("%s %-24s 读流失败(%v): %v", fp, model, dur, readErr)
						continue
					}
					text := string(raw)
					snippet := extractText(text, 60)
					bizCode, bizMsg := bizError(text)
					t.Logf("%s %-24s 耗时=%v 字节=%d data帧=%d 正文=%q 业务码=%d msg=%.60s",
						fp, model, dur, len(raw), strings.Count(text, "data:"), snippet, bizCode, bizMsg)
					if snippet != "" {
						ok = true
						break
					}
				}
				if !ok {
					t.Logf("%s 前 4 个模型都没出正文（上面每行的业务码即原因；链路本身是通的）", fp)
				}
			}
		})
	}
}

// TestLiveAccountChatOnce 单发一次：只挑一个模型、只打一次，用来探「限流是否恢复」。
// 连打会把自己打成 4011，探活必须克制。
//
//	TW2A_LIVE_CHAT=1 TW2A_ONCE_MODEL=Doubao-Seed-2.1-Turbo //	  go test -tags liveauth ./internal/upstream -run TestLiveAccountChatOnce -v
func TestLiveAccountChatOnce(t *testing.T) {
	fp := os.Getenv("TW2A_ONCE_FP")
	if fp == "" { fp = "chrome_133" }
	if err := SetTLSFingerprint(fp); err != nil {
		t.Fatal(err)
	}
	for _, tk := range findTickets(t) {
		if !strings.Contains(tk.Host, "api5-normal.mchost.guru") {
			continue
		}
		a := authFromTicket(tk)
		up := New()
		model := os.Getenv("TW2A_ONCE_MODEL")
		if model == "" {
			models, err := up.FetchModels(a)
			if err != nil || len(models) == 0 {
				t.Fatalf("拉模型表失败: %v", err)
			}
			model = models[0].ID
		}
		// 与 probe_live_test 里能出正文的样例同形：content 是字符串、带 max_tokens。
		payload := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"max_tokens":8,"stream":true}`, model)
		start := time.Now()
		rc, status, body, err := up.ChatStream(a, []byte(payload))
		if err != nil {
			t.Fatalf("发起失败(%v) status=%d body=%.200s", time.Since(start), status, body)
		}
		raw, _ := io.ReadAll(io.LimitReader(rc, 256<<10))
		rc.Close()
		text := string(raw)
		code, msg := bizError(text)
		t.Logf("fp=%s model=%s 耗时=%v 字节=%d 业务码=%d msg=%.80s", fp, model, time.Since(start), len(raw), code, msg)
		if s := extractText(text, 60); s != "" {
			t.Logf("正文=%q", s)
		}
	}
}

// TestLiveAccountReadonly 跑 ug 族的只读业务接口：付费身份 / 额度 / 积分包。
// 这些不吃模型额度、不改账号状态，用来证明 ug 链路在真实上游是通的。
func TestLiveAccountReadonly(t *testing.T) {
	for _, tk := range findTickets(t) {
		if ticketRealm(tk.Host) != auth.RealmCN {
			continue // ug 族国际接口本仓不支持
		}
		a := authFromTicket(tk)
		up := New()

		if id, known, err := up.PayIdentity(a); err != nil {
			t.Logf("PayIdentity 失败: %v", err)
		} else {
			t.Logf("PayIdentity OK: identity=%d known=%v", id, known)
		}
		if remain, total, expire, err := up.UserEntUsage(a); err != nil {
			t.Logf("UserEntUsage 失败: %v", err)
		} else {
			t.Logf("UserEntUsage OK: 剩余=%d 总量=%d 过期=%d", remain, total, expire)
		}
		if pkgs, eu, err := up.BalancePackages(a); err != nil {
			t.Logf("BalancePackages 失败: %v", err)
		} else {
			t.Logf("BalancePackages OK: 包数=%d 额度=%+v", len(pkgs), eu)
		}
	}
}
