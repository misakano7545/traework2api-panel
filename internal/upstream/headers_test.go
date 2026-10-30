package upstream

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"traework2api/internal/auth"
)

// trae-mate gen2 派生算法的测试向量（与 AiCheckin 参考实现交叉验证）。
func TestTraeDeviceIdentityVectors(t *testing.T) {
	dev, market, sess := traeDeviceIdentity("1234567890123456", 0)
	if dev != "413174708280782" {
		t.Errorf("device_id=%q", dev)
	}
	if market != "746608f9-7f37-4960-b4c7-8553cec6d366" {
		t.Errorf("market_user_id=%q", market)
	}
	if sess != "fbabf7aa1e173b90385c623e1ec49157860cc07a4b01f53f7ca141b17d876eae" {
		t.Errorf("session_id=%q", sess)
	}
	// 确定性：同一 uid 必得同一身份；不同 uid 必不同。
	if d2, m2, s2 := traeDeviceIdentity("1234567890123456", 0); d2 != dev || m2 != market || s2 != sess {
		t.Error("派生不确定：同一 uid 两次结果不同")
	}
	if d3, m3, _ := traeDeviceIdentity("1234567890123457", 0); d3 == dev || m3 == market {
		t.Error("不同 uid 派生出相同身份")
	}
	// 换指纹（种子非 0）必须换出一整套新身份，且同一账号 + 同一种子仍确定性。
	d4, m4, s4 := traeDeviceIdentity("1234567890123456", 1)
	if d4 == dev || m4 == market || s4 == sess {
		t.Fatal("换指纹没有换出新身份")
	}
	if d5, m5, s5 := traeDeviceIdentity("1234567890123456", 1); d5 != d4 || m5 != m4 || s5 != s4 {
		t.Error("换指纹后的派生不确定")
	}
	if s6, _, _ := traeDeviceIdentity("1234567890123456", 2); s6 == d4 {
		t.Error("不同种子应派生出不同身份")
	}
}

// ug 族必须带齐完整客户端伪装头（9074 修复的关键面）。
func TestUgHeadersCompleteImpersonation(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://api.trae.cn/x", nil)
	UgHeaders(req, &auth.Auth{AccessToken: "at", UID: "1234567890123456"})

	want := map[string]string{
		"Authorization":      "Cloud-IDE-JWT at",
		"Accept":             "*/*",
		"User-Agent":         "VSCode 1.107.1 (TRAE SOLO CN)",
		"X-Market-Client-Id": "VSCode 1.107.1",
		"X-Market-User-Id":   "746608f9-7f37-4960-b4c7-8553cec6d366",
		"X-User-Region":      "CN",
		"X-Device-Id":        "413174708280782",
		"X-Lgw-Req-Sdk-Type": "3",
		"Package-Type":       "stable_cn",
		"X-Lscbd-Aid":        "787976",
		"X-Lscbd-Platform":   "windows",
		"App-Version":        IdeVersion,
		"Vscode-Sessionid":   "fbabf7aa1e173b90385c623e1ec49157860cc07a4b01f53f7ca141b17d876eae",
		"Sec-Fetch-Dest":     "empty",
		"Sec-Fetch-Mode":     "no-cors",
		"Sec-Fetch-Site":     "none",
	}
	for k, v := range want {
		if got := req.Header.Get(k); got != v {
			t.Errorf("头 %s=%q want %q", k, got, v)
		}
	}
	if tc := req.Header.Get("X-Tt-Trace-Id"); len(tc) != 22 || !strings.HasPrefix(tc, "00-") || !strings.HasSuffix(tc, "-01") {
		t.Errorf("X-Tt-Trace-Id=%q", tc)
	}
	if rid := req.Header.Get("X-Request-Id"); len(rid) != 36 || strings.Count(rid, "-") != 4 {
		t.Errorf("X-Request-Id=%q", rid)
	}
}

// 国际账号的 ug 族头必须走另一套：stable_i18n + 3.6.4 + Trae 品牌 UA，
// 且**不能**带国内的 x-user-region / x-lscbd-*（实测客户端不发，硬编码等于贴错区）。
func TestUgHeadersIntlRealm(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://growsg-normal.trae.ai/x", nil)
	UgHeaders(req, &auth.Auth{AccessToken: "at", Domain: "trae.ai", UID: "1234567890123456"})

	want := map[string]string{
		"Package-Type":     "stable_i18n",
		"App-Version":      IdeVersionIntl,
		"User-Agent":       "VSCode 1.107.1 (Trae)",
		"X-Market-User-Id": "746608f9-7f37-4960-b4c7-8553cec6d366",
		"X-Device-Id":      "413174708280782",
	}
	for k, v := range want {
		if got := req.Header.Get(k); got != v {
			t.Errorf("INTL ug 头 %s=%q want %q", k, got, v)
		}
	}
	for _, k := range []string{"X-User-Region", "X-Lscbd-Aid", "X-Lscbd-Platform"} {
		if got := req.Header.Get(k); got != "" {
			t.Errorf("INTL ug 不该发 %s，得到 %q", k, got)
		}
	}
	// 派生设备身份必须两区一致（同 uid 同 seed 同结果）—— 换了就是换指纹，不是分地区。
	if got := req.Header.Get("X-Device-Id"); got != "413174708280782" {
		t.Errorf("派生身份被地区污染了：%q", got)
	}
}

// chat 路的伪装头**键集**必须钉住。
//
// 口径：2026-10-30 对官方客户端（Trae CN 1.107.1，appVersion 3.4.11）Agent 模式的真请求
// 抓包，见 constants.go / identity.go 的注释与 .reverse/agent_headers_cn_vs_intl.txt。
// 与旧口径的差别：删了 X-Cloudide-Token / X-Uid（客户端对话链路不发），换成客户端的
// 原生网络栈自证五件套 + ug 族 + trace 三件套。头是「像不像客户端」的开关：少一个不报错，
// 但少得越多越像自建代理。
func TestSOLOHeadersKeySet(t *testing.T) {
	want := []string{
		"Accept", "Authorization", "Content-Type", "Package-Type", "Request-Traffic-Type",
		"Traceparent", "User-Agent",
		"X-Agent-Host-Type", "X-Ahanet-Timeout", "X-App-Id", "X-App-Version", "X-App-Version-Code",
		"X-Bridge-Transport", "X-Custom-Trace-Id",
		"X-Device-Brand", "X-Device-Cpu", "X-Device-Id", "X-Device-Type",
		"X-Flow-Traceparent", "X-Harness",
		"X-Ide-Token", "X-Ide-Version", "X-Ide-Version-Code", "X-Ide-Version-Type",
		"X-Lgw-Req-Sdk-Type", "X-Lscbd-Aid", "X-Lscbd-Platform",
		"X-Machine-Id", "X-Os-Version",
		"X-Request-Id", "X-Ss-Dp", "X-User-Region",
	}
	for _, stream := range []bool{true, false} {
		req, _ := http.NewRequest(http.MethodPost, "https://api.trae.cn/x", nil)
		SOLOHeaders(req, &auth.Auth{AccessToken: "JWT-ABC", DeviceID: "d-1", MachineID: "m-1", UID: "u-1"}, stream)

		got := make([]string, 0, len(req.Header))
		for k := range req.Header {
			got = append(got, k)
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("stream=%v 头集合不符\n got=%v\nwant=%v", stream, got, want)
		}
		// 头不能是空值：空 X-Device-Id 上游判「缺失」而不是「没有这个账号」。
		for _, k := range want {
			if req.Header.Get(k) == "" {
				t.Errorf("stream=%v 头 %s 为空", stream, k)
			}
		}
		// 客户端对两条路径都发 */*：流是靠 body.stream 决定的，不是 Accept 协商。
		if got := req.Header.Get("Accept"); got != "*/*" {
			t.Errorf("stream=%v Accept=%q want */*", stream, got)
		}
		if req.Header.Get("X-Ide-Version") != IdeVersion || req.Header.Get("X-Ide-Version-Code") != IdeVersionCode {
			t.Errorf("版本头与常量不一致：%q/%q", req.Header.Get("X-Ide-Version"), req.Header.Get("X-Ide-Version-Code"))
		}
		// 国内版 x-app-version 发版本号（国际版才发 "default"）。
		if got := req.Header.Get("X-App-Version"); got != IdeVersion {
			t.Errorf("stream=%v X-App-Version=%q want %q", stream, got, IdeVersion)
		}
		// 客户端不发的那两个、以及会把明文 body 逼成密文的加密伴随头：一个都不能有。
		// X-Request-Pin / X-Requested-At 带上去上游直接 400 base64 decode failed（实测）。
		for _, k := range []string{"X-Cloudide-Token", "X-Uid", "X-Request-Pin", "X-Requested-At"} {
			if req.Header.Get(k) != "" {
				t.Errorf("stream=%v 不该发 %s", stream, k)
			}
		}
	}
}

// 国际版头必须走另一套：x-app-version=default / stable_i18n / X-SS-DP=677332，
// 且**不能**带国内的 x-user-region 与 x-lscbd-*（实测客户端没有这两个）。
func TestSOLOHeadersIntlRealm(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://coresg-normal.trae.ai/x", nil)
	SOLOHeaders(req, &auth.Auth{AccessToken: "JWT-ABC", Domain: "trae.ai", DeviceID: "d-1", MachineID: "m-1", UID: "u-1"}, true)

	want := map[string]string{
		"X-App-Version":      "default",
		"X-App-Version-Code": IdeVersionCodeIntl,
		"X-Ide-Version":      IdeVersionIntl,
		"X-Ide-Version-Code": IdeVersionCodeIntl,
		"Package-Type":       "stable_i18n",
		"X-Ss-Dp":            "677332",
	}
	for k, v := range want {
		if got := req.Header.Get(k); got != v {
			t.Errorf("INTL 头 %s=%q want %q", k, got, v)
		}
	}
	for _, k := range []string{"X-User-Region", "X-Lscbd-Aid", "X-Lscbd-Platform"} {
		if got := req.Header.Get(k); got != "" {
			t.Errorf("INTL 不该发 %s（实测客户端没有），得到 %q", k, got)
		}
	}
	if ua := req.Header.Get("User-Agent"); !strings.Contains(ua, "Trae/"+UABaseVersion) {
		t.Errorf("INTL UA=%q 应含 Trae/%s", ua, UABaseVersion)
	}
}

// 国内版 UA 必须是完整 Chromium 串（TraeCN/… 家族），不是 "Trae/<ver>"。
func TestSOLOHeadersCNUserAgent(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://api5-normal.mchost.guru/x", nil)
	SOLOHeaders(req, &auth.Auth{AccessToken: "at", UID: "u1"}, true)
	ua := req.Header.Get("User-Agent")
	for _, frag := range []string{"Mozilla/5.0", "TraeCN/" + UABaseVersion, "Chrome/" + UAChromeVersion, "Electron/" + UAElectronVersion} {
		if !strings.Contains(ua, frag) {
			t.Errorf("UA=%q 缺片段 %q", ua, frag)
		}
	}
}
