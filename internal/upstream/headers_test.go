package upstream

import (
	"net/http"
	"strings"
	"testing"

	"traework2api/internal/auth"
)

// trae-mate gen2 派生算法的测试向量（与 AiCheckin 参考实现交叉验证）。
func TestTraeDeviceIdentityVectors(t *testing.T) {
	dev, market, sess := traeDeviceIdentity("1234567890123456")
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
	if d2, m2, s2 := traeDeviceIdentity("1234567890123456"); d2 != dev || m2 != market || s2 != sess {
		t.Error("派生不确定：同一 uid 两次结果不同")
	}
	if d3, m3, _ := traeDeviceIdentity("1234567890123457"); d3 == dev || m3 == market {
		t.Error("不同 uid 派生出相同身份")
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
		"App-Version":        "0.1.45",
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
