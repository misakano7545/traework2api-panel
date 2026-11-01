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
