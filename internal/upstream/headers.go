// headers.go SOLO 三类请求头：对话（SOLOHeaders）/ ug（UgHeaders）/ oauth（OAuthHeaders）。
package upstream

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"

	"traework2api/internal/auth"
)

// 出站身份走 identity.go 的运行期覆盖（默认仍是 Trae/<IdeVersion>）。

// SOLOHeaders 设置 llm_utils_chat / get_detail_param 所需的 SOLO 专属头。
// 规则来自 SPEC §1 SOLO headers（实测必须）。
func SOLOHeaders(req *http.Request, a *auth.Auth, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", currentUA())
	at := a.JWT() // 读锁快照，防与 RefreshToken 写并发竞态
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+at)
	req.Header.Set("X-Cloudide-Token", at)
	req.Header.Set("X-Ide-Token", at)
	if a.UID != "" {
		req.Header.Set("X-Uid", a.UID)
	}
	req.Header.Set("X-App-Id", AppID)
	req.Header.Set("X-App-Version", "default")
	// 版本组按账号地区：国际版是另一套（X-App-Version-Code 必须非空，缺了上游 4001）。
	ver, verCode := identFor(a)
	req.Header.Set("X-Ide-Version", ver)
	req.Header.Set("X-Ide-Version-Code", verCode)
	req.Header.Set("X-App-Version-Code", verCode)
	req.Header.Set("X-Ide-Version-Type", "stable")
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("X-OS-Version", OSVersion)
	req.Header.Set("X-Device-Brand", DeviceBrand)
	req.Header.Set("Request-Traffic-Type", "prod")
	if a.MachineID != "" {
		req.Header.Set("X-Machine-Id", a.MachineID)
	}
	if a.DeviceID != "" {
		req.Header.Set("X-Device-Id", a.DeviceID)
	}
}

// ── ug 族（签到/积分）的客户端伪装头 ──────────────────────────────────────
//
// 2026-10-02 实测：只带零星设备头（旧版 6 个头）时 claim 接口 100% 回 9074
// 「当前参与用户太多」（fork 三天 ~150 次全灭，连凌晨时段也不通）；换成下面
// 这套完整客户端伪装头 + 按 UID 确定性派生的伪设备身份后实测一次通过。
// 参考 AiCheckin / trae-mate 的实现（派生算法测试向量已交叉验证）。

// ugUA ug 族固定 UA：client 伪装要求，不跟随面板的运行期 UA 覆盖。
const ugUA = "VSCode 1.107.1 (TRAE SOLO CN)"

// traeDeviceIdentity 按 uid 确定性派生伪设备身份（trae-mate gen2 算法）：
// 同一账号永远同一套标识，服务端按设备维度记账保持稳定。
//
// seed != 0 时把种子拌进派生输入 = 换一整套新标识（面板「换指纹」用：9074 是设备维度
// 风控，换个设备号就能脱开旧标记）。seed == 0 保持历史算法逐位不变，老账号身份不动。
func traeDeviceIdentity(uid string, seed int64) (deviceID, marketUserID, sessionID string) {
	base := uid
	if seed != 0 {
		base = fmt.Sprintf("%s#%d", uid, seed)
	}
	dv := seededStream(base, "devid", 15)
	dev := make([]byte, len(dv))
	for i, b := range dv {
		dev[i] = '0' + b%10
	}
	deviceID = string(dev)

	bs := seededStream(base, "market", 16)
	bs[6] = (bs[6] & 0x0F) | 0x40 // UUID v4 版本位
	bs[8] = (bs[8] & 0x3F) | 0x80 // RFC 4122 variant 位
	h := hex.EncodeToString(bs)
	marketUserID = h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]

	sessionID = hex.EncodeToString(seededStream(base, "sess", 32))[:64]
	return
}

// seededStream 确定性字节流：SHA-256("<salt>:<seed>" ‖ counter_be32) 连续拼接。
func seededStream(seed, salt string, n int) []byte {
	base := []byte(salt + ":" + seed)
	out := make([]byte, 0, n+32)
	for i := 0; len(out) < n; i++ {
		var ctr [4]byte
		binary.BigEndian.PutUint32(ctr[:], uint32(i))
		sum := sha256.Sum256(append(append([]byte{}, base...), ctr[:]...))
		out = append(out, sum[:]...)
	}
	return out[:n]
}

func ugRandHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b) // crypto/rand 在 Linux 上不会失败；失败也仅退化为零值
	return hex.EncodeToString(b)
}

func ugUUIDv4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0F) | 0x40
	b[8] = (b[8] & 0x3F) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// UGDeviceID 面板展示用：这个账号 ug 族当前会发出的 X-Device-Id（换指纹后随之变）。
func UGDeviceID(a *auth.Auth) string {
	dev, _, _ := traeDeviceIdentity(a.UID, a.DeviceSeedValue())
	return dev
}

// UgHeaders 设置签到/积分（api.trae.cn）所需头：完整客户端伪装 + 派生设备身份。
func UgHeaders(req *http.Request, a *auth.Auth) {
	dev, market, sess := traeDeviceIdentity(a.UID, a.DeviceSeedValue())
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "*/*")
	h.Set("User-Agent", ugUA)
	h.Set("Authorization", "Cloud-IDE-JWT "+a.JWT()) // 读锁快照
	h.Set("X-Market-Client-Id", "VSCode 1.107.1")
	h.Set("X-Market-User-Id", market)
	h.Set("X-User-Region", "CN")
	h.Set("X-Device-Id", dev)
	h.Set("X-Lgw-Req-Sdk-Type", "3")
	h.Set("Package-Type", "stable_cn")
	h.Set("X-Lscbd-Aid", "787976")
	h.Set("X-Lscbd-Platform", "windows")
	h.Set("App-Version", "0.1.45")
	h.Set("X-Tt-Trace-Id", "00-"+ugRandHex(8)+"-01")
	h.Set("Vscode-Sessionid", sess)
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "no-cors")
	h.Set("Sec-Fetch-Site", "none")
	h.Set("X-Request-Id", ugUUIDv4())
	// ponytail: 不带 accept-encoding —— Go transport 自动加 gzip 并透明解压，
	// 手动设置反而会关掉自动解压（参考实现里的显式 gzip 行故意省略）。
}

// OAuthHeaders 设置 ExchangeToken / GetUserInfo 所需头（无签名，仅 UA）。
func OAuthHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", currentUA())
}
