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

// chat 路的伪装头**键集**必须钉住。判据：与 cpa-multi-plugins/trae 参考实现的
// 对拍答案卷逐键交叉核对（2026-10-08）—— 键集完全一致，只有版本号/版本码是我们按
// 自己的探针钉的（参考实现报 0.1.61/20260820，我们钉 20260811，见 constants.go 与
// README「版本码矩阵」）。头是「能不能过」的开关：X-App-Version-Code 缺了上游直接
// 4001（实测），X-Ide-Version-Code 又是模型表的放量开关 —— 少一个头是静默掉模型/掉通道。
func TestSOLOHeadersKeySet(t *testing.T) {
	want := []string{
		"Accept", "Authorization", "Content-Type", "Request-Traffic-Type", "User-Agent",
		"X-App-Id", "X-App-Version", "X-App-Version-Code", "X-Cloudide-Token",
		"X-Device-Brand", "X-Device-Id", "X-Device-Type", "X-Ide-Token", "X-Ide-Version",
		"X-Ide-Version-Code", "X-Ide-Version-Type", "X-Machine-Id", "X-Os-Version", "X-Uid",
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
		// 头不能是空值：空 X-Device-Id / X-Uid 上游判「缺失」而不是「没有这个账号」。
		for _, k := range want {
			if req.Header.Get(k) == "" {
				t.Errorf("stream=%v 头 %s 为空", stream, k)
			}
		}
		accept := "application/json"
		if stream {
			accept = "text/event-stream"
		}
		if req.Header.Get("Accept") != accept {
			t.Errorf("stream=%v Accept=%q", stream, req.Header.Get("Accept"))
		}
		if req.Header.Get("X-Ide-Version") != IdeVersion || req.Header.Get("X-Ide-Version-Code") != IdeVersionCode {
			t.Errorf("版本头与常量不一致：%q/%q", req.Header.Get("X-Ide-Version"), req.Header.Get("X-Ide-Version-Code"))
		}
	}
}
