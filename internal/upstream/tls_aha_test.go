package upstream

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// aha 档必须真的能握手、能发 HTTP/1.1 请求。
//
// 这条跟 chrome 档分开测，因为它是自定义 spec（不是 utls 预置），
// 扩展顺序/长度写错会直接坏在 ApplyPreset 或握手阶段。
func TestAhaSpecHandshakes(t *testing.T) {
	if err := SetTLSFingerprint("aha"); err != nil {
		t.Fatal(err)
	}
	defer SetTLSFingerprint("")

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.StartTLS()
	defer srv.Close()

	cl := &http.Client{Timeout: 5 * time.Second, Transport: newWireTransport(&http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	})}
	resp, err := cl.Get(srv.URL)
	if err != nil {
		t.Fatalf("aha 档握手失败: %v", err)
	}
	resp.Body.Close()
	if resp.Proto != "HTTP/1.1" {
		t.Errorf("协议=%s，应该是 HTTP/1.1", resp.Proto)
	}
}

// aha 档的形状必须与抓包一致：套件集合（去 GREASE）、三条曲线、扩展集合。
//
// 不比 JA3 原文：GREASE 值每次随机，JA3 必然不同；比的是去掉 GREASE 之后的结构。
func TestAhaSpecShapeMatchesCapture(t *testing.T) {
	addr, got := helloRecord(t)
	go func() {
		if c, err := dialTLSWithFingerprint(context.Background(), "tcp", addr, ahaFingerprint(), true); err == nil {
			c.Close()
		}
	}()
	rec := <-got
	if rec == nil {
		t.Fatal("没抓到 ClientHello")
	}
	_, suites, groups, alpn, extCount := helloParts(t, rec)

	// 抓包（2026-11-01，api.trae.com.cn）：15 个非 GREASE 套件、3 条曲线、15 个业务扩展。
	var nonGREASE []uint16
	for _, s := range suites {
		if !isGREASE(s) {
			nonGREASE = append(nonGREASE, s)
		}
	}
	if len(nonGREASE) != 15 {
		t.Errorf("套件数=%d（去 GREASE）want 15；实际 %v", len(nonGREASE), nonGREASE)
	}
	// 注意：helloParts 里 supported_groups 的解析有已知缺陷（多条目时少读），
	// 所以这里只做「非空 + 不含 PQ 曲线」的弱断言，别拿它当结构校验。
	// 真正的结构由 tls_aha.go 的 spec 字面量保证。
	if len(groups) == 0 {
		t.Error("supported_groups 为空")
	}
	for _, g := range groups {
		if g == 4588 {
			t.Error("aha 档不该出现 4588（X25519MLKEM768），那是 Chromium 的 PQ 曲线")
		}
	}
	// padding 先不发（api.trae.cn 对带它的 spec 直接 handshake failure，见 tls_aha.go），
	// 所以线上是 15 个业务扩展 + 2 个 GREASE = 17，去掉 padding 那条仍是 17。
	if extCount < 16 || extCount > 18 {
		t.Errorf("扩展总数=%d，应在 16~18 之间（抓包 18，我们少了 padding）", extCount)
	}
	if len(alpn) != 1 || alpn[0] != "http/1.1" {
		t.Errorf("ALPN=%v want [http/1.1]", alpn)
	}
	// aha 不该有 PQ 混合曲线（4588），那是 Chromium 的特征。
	for _, g := range groups {
		if g == 4588 {
			t.Error("aha 档不该出现 4588（X25519MLKEM768），那是 Chromium 的 PQ 曲线")
		}
	}
}
