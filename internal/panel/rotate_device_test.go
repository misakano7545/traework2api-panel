package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
	"traework2api/internal/upstream"
)

// TestRotateDeviceChangesFingerprint 换指纹：种子 +1、派生的 ug 设备号跟着换、落盘持久化，
// 且原账号（种子里 0）与全新账号的身份不受影响。
func TestRotateDeviceChangesFingerprint(t *testing.T) {
	dir := t.TempDir()
	a := &auth.Auth{UID: "u1", Domain: "trae.cn", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: 9999999999, FilePath: filepath.Join(dir, "trae-u1.json")}
	before := upstream.UGDeviceID(a)
	pl := pool.New("")
	pl.Add(a)
	p := New(Config{Pool: pl, APIKey: "k", Logs: NewRing(20)})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/accounts/u1/rotate-device", nil)
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out struct {
		DeviceID string `json:"device_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)

	if a.DeviceSeedValue() != 1 {
		t.Errorf("种子应为 1，实得 %d", a.DeviceSeedValue())
	}
	after := upstream.UGDeviceID(a)
	if after == before || out.DeviceID != after {
		t.Fatalf("设备号没换：before=%s after=%s 响应=%s", before, after, out.DeviceID)
	}
	if len(after) != 15 {
		t.Errorf("派生的设备号长度应为 15，实得 %d (%s)", len(after), after)
	}
	// 落盘：重启后种子不丢（否则换指纹白换）
	buf, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatalf("落盘读回失败: %v", err)
	}
	raw, err := auth.Parse(buf)
	if err != nil {
		t.Fatalf("解析落盘内容失败: %v", err)
	}
	if raw.DeviceSeedValue() != 1 {
		t.Errorf("落盘里的种子=%d 期望 1", raw.DeviceSeedValue())
	}
	if upstream.UGDeviceID(raw) != after {
		t.Error("落盘读回的设备号与原对象不一致")
	}

	// 再换一次必须再变（种子递增，不是随机撞运气）
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/panel/api/accounts/u1/rotate-device", nil)
	req2.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec2, req2)
	if third := upstream.UGDeviceID(a); third == after {
		t.Error("第二次换指纹应该换出第三个身份")
	}
}

// TestRotateDeviceAll 一键换全部：每个号都换，面板回账号列表。
func TestRotateDeviceAll(t *testing.T) {
	dir := t.TempDir()
	a1 := &auth.Auth{UID: "u1", AccessToken: "a", RefreshToken: "r", ExpiresAt: 9999999999, FilePath: filepath.Join(dir, "a.json")}
	a2 := &auth.Auth{UID: "u2", AccessToken: "b", RefreshToken: "r", ExpiresAt: 9999999999, FilePath: filepath.Join(dir, "b.json")}
	before := []string{upstream.UGDeviceID(a1), upstream.UGDeviceID(a2)}
	pl := pool.New("")
	pl.Add(a1)
	pl.Add(a2)
	p := New(Config{Pool: pl, APIKey: "k", Logs: NewRing(20)})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/accounts/rotate-device-all", nil)
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if upstream.UGDeviceID(a1) == before[0] || upstream.UGDeviceID(a2) == before[1] {
		t.Fatal("一键换指纹没换全部")
	}
	if a1.DeviceSeedValue() != 1 || a2.DeviceSeedValue() != 1 {
		t.Errorf("种子应为 1/1，实得 %d/%d", a1.DeviceSeedValue(), a2.DeviceSeedValue())
	}
}
