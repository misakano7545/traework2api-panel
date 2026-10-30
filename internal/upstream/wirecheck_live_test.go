//go:build wirecheck

// 临时实测桩：用仓库自己的 SOLOHeaders 打真上游，确认改完仍然 200 出流。
// 跑法：TW2A_CAP=<capture.jsonl> go test -tags wirecheck ./internal/upstream -run TestWireCheckLive -v
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

func TestWireCheckLive(t *testing.T) {
	capPath := os.Getenv("TW2A_CAP")
	if capPath == "" {
		t.Skip("未设 TW2A_CAP")
	}
	raw, err := os.ReadFile(capPath)
	if err != nil {
		t.Fatal(err)
	}
	var tok, devID, machID string
	for _, line := range strings.Split(string(raw), "\n") {
		var rec struct {
			Kind    string            `json:"kind"`
			Path    string            `json:"path"`
			Headers map[string]string `json:"headers"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil || rec.Kind != "req" {
			continue
		}
		if !strings.Contains(rec.Path, "llm_utils_chat") {
			continue
		}
		for k, v := range rec.Headers {
			switch strings.ToLower(k) {
			case "authorization":
				tok = strings.TrimPrefix(v, "Cloud-IDE-JWT ")
			case "x-device-id":
				devID = v
			case "x-machine-id":
				machID = v
			}
		}
		if tok != "" {
			break
		}
	}
	if tok == "" {
		t.Fatal("capture 里没有 llm_utils_chat 的 token")
	}

	a := &auth.Auth{AccessToken: tok, DeviceID: devID, MachineID: machID, UID: "4192911412971131"}
	t.Logf("devID=%s machID=%s tokLen=%d", devID, machID[:8]+"…", len(tok))

	body := `{"messages":[{"role":"user","content":[{"type":"text","text":"只回两个字：收到"}]}],` +
		`"function":"solo_work_lite","stream":true,"config_name":"glm-5.2","model":"glm-5.2"}`
	req, err := http.NewRequest(http.MethodPost, AgentHost+EpChat, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	SOLOHeaders(req, a, true)

	c := &http.Client{Timeout: 60 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	head, _ := io.ReadAll(io.LimitReader(resp.Body, 700))
	for k, v := range req.Header {
		t.Logf("  SENT %s: %s", k, strings.Join(v, ","))
	}
	t.Logf("status=%d content-type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	t.Logf("首帧: %s", strings.ReplaceAll(string(head), "\n", "\\n"))
	if resp.StatusCode != 200 {
		t.Fatalf("上游非 200：%d", resp.StatusCode)
	}
	if !strings.Contains(string(head), "event:") {
		t.Fatalf("没有 SSE 事件：%s", head)
	}

	// 同一套头打模型表端点（SOLOHeaders 的另一个调用方），确认版本码没把阵容打没。
	mreq, err := http.NewRequest(http.MethodPost, AgentHost+EpModels,
		strings.NewReader(`{"function":"solo_work_lite","need_prompt":false}`))
	if err != nil {
		t.Fatal(err)
	}
	SOLOHeaders(mreq, a, false)
	mresp, err := c.Do(mreq)
	if err != nil {
		t.Fatalf("模型表请求失败: %v", err)
	}
	defer mresp.Body.Close()
	mraw, _ := io.ReadAll(io.LimitReader(mresp.Body, 4<<20))
	var doc struct {
		ConfigInfoList []struct {
			ConfigName string `json:"config_name"`
		} `json:"config_info_list"`
		Code int `json:"code"`
	}
	if err := json.Unmarshal(mraw, &doc); err != nil {
		t.Fatalf("模型表解析失败 status=%d body=%.200s", mresp.StatusCode, mraw)
	}
	t.Logf("模型表: status=%d code=%d 条数=%d", mresp.StatusCode, doc.Code, len(doc.ConfigInfoList))
	if mresp.StatusCode != 200 || len(doc.ConfigInfoList) == 0 {
		t.Fatalf("模型表空/失败：status=%d code=%d", mresp.StatusCode, doc.Code)
	}
}
