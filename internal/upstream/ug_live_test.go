//go:build wirecheck

// 临时实测桩：用仓库自己的 UgHeaders 打两区真上游，确认分地区改动不影响鉴权。
//
// 跑法：
//
//	TW2A_CAP=<CN抓包> TW2A_CAP_INTL=<国际抓包> \
//	  go test -tags wirecheck ./internal/upstream -run TestUgHeadersLive -v
package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"traework2api/internal/auth"
)

// pickToken 从抓包里捞一条指定 host+path 的票据与设备号。
func pickToken(capPath, host, pathFrag string) (tok, dev string) {
	raw, err := os.ReadFile(capPath)
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		var rec struct {
			Kind    string            `json:"kind"`
			Host    string            `json:"host"`
			Path    string            `json:"path"`
			Headers map[string]string `json:"headers"`
			Body    string            `json:"body"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil || rec.Kind != "req" {
			continue
		}
		if rec.Host != host || !strings.Contains(rec.Path, pathFrag) {
			continue
		}
		for k, v := range rec.Headers {
			switch strings.ToLower(k) {
			case "authorization":
				tok = strings.TrimPrefix(v, "Cloud-IDE-JWT ")
			case "x-device-id":
				dev = v
			}
		}
		if tok != "" {
			var body struct {
				DeviceID string `json:"device_id"`
			}
			_ = json.Unmarshal([]byte(rec.Body), &body)
			if body.DeviceID != "" {
				dev = body.DeviceID
			}
			return tok, dev
		}
	}
	return "", ""
}

func TestUgHeadersLive(t *testing.T) {
	cnCap := os.Getenv("TW2A_CAP")
	intlCap := os.Getenv("TW2A_CAP_INTL")
	if cnCap == "" || intlCap == "" {
		t.Skip("需要 TW2A_CAP 与 TW2A_CAP_INTL")
	}
	c := &http.Client{Timeout: 40 * time.Second}

	// ── 国际：额度 v1，走账号 ApiHost（客户端实际打的是 growsg-normal）────
	intlTok, intlDev := pickToken(intlCap, "growsg-normal.trae.ai", "pay_status")
	if intlTok == "" {
		t.Fatal("国际抓包里没有 growsg-normal 的票据")
	}
	t.Logf("INTL token len=%d dev=%s", len(intlTok), intlDev)

	ia := &auth.Auth{AccessToken: intlTok, Domain: "trae.ai", UID: intlDev,
		ApiHost: "https://growsg-normal.trae.ai"}
	ireq, err := http.NewRequest(http.MethodPost,
		ia.ApiHost+EpEntUsageIntl, strings.NewReader(`{"trae_client":"IDE"}`))
	if err != nil {
		t.Fatal(err)
	}
	UgHeaders(ireq, ia)
	iresp, err := c.Do(ireq)
	if err != nil {
		t.Fatalf("INTL 请求失败: %v", err)
	}
	defer iresp.Body.Close()
	iraw, _ := io.ReadAll(io.LimitReader(iresp.Body, 1<<20))

	var idoc struct {
		Code                    int   `json:"code"`
		IsDollarUsageBilling    bool  `json:"is_dollar_usage_billing"`
		UserEntitlementPackList []any `json:"user_entitlement_pack_list"`
	}
	_ = json.Unmarshal(iraw, &idoc)
	t.Logf("INTL ent_usage v1: status=%d code=%d dollar=%v 套餐数=%d",
		iresp.StatusCode, idoc.Code, idoc.IsDollarUsageBilling, len(idoc.UserEntitlementPackList))
	if iresp.StatusCode != 200 {
		t.Fatalf("INTL 额度接口非 200：%d %.200s", iresp.StatusCode, iraw)
	}
	if !idoc.IsDollarUsageBilling || len(idoc.UserEntitlementPackList) == 0 {
		t.Fatalf("INTL 额度响应形状不对（可能是头被拒后回了空壳）：%.300s", iraw)
	}
	// 国际版不该带国内那套 ug 头。
	for _, k := range []string{"X-User-Region", "X-Lscbd-Aid", "Package-Type"} {
		if v := ireq.Header.Get(k); v != "" && k == "Package-Type" && v != "stable_i18n" {
			t.Errorf("INTL package-type=%q 应为 stable_i18n", v)
		}
		if v := ireq.Header.Get(k); v != "" && k != "Package-Type" {
			t.Errorf("INTL 不该发 %s=%q", k, v)
		}
	}

	// ── 国内：签到 status（只读，不碰 claim），确认分地区改动没伤到 9074 修复 ──
	cnTok, cnDev := pickToken(cnCap, "api5-normal.mchost.guru", "llm_utils_chat")
	if cnTok == "" {
		t.Fatal("国内抓包里没有票据")
	}
	ca := &auth.Auth{AccessToken: cnTok, Domain: "trae.cn", UID: cnDev}
	creq, err := http.NewRequest(http.MethodPost, UgHost+EpCheckinStatus, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	UgHeaders(creq, ca)
	cresp, err := c.Do(creq)
	if err != nil {
		t.Fatalf("CN 请求失败: %v", err)
	}
	defer cresp.Body.Close()
	craw, _ := io.ReadAll(io.LimitReader(cresp.Body, 1<<20))
	t.Logf("CN checkin status: status=%d %.200s", cresp.StatusCode, craw)
	if cresp.StatusCode != 200 {
		t.Fatalf("CN 签到 status 非 200：%d", cresp.StatusCode)
	}
	if !strings.Contains(string(craw), `"code":0`) {
		t.Fatalf("CN 签到 status 业务码异常：%.200s", craw)
	}
	if got := creq.Header.Get("Package-Type"); got != "stable_cn" {
		t.Errorf("CN package-type=%q 应为 stable_cn", got)
	}
}
