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

// 出站身份走 identity.go 的运行期覆盖（默认 = 官方客户端的 UA 家族与版本对）。

// SOLOHeaders 设置 llm_utils_chat / get_detail_param 所需的 SOLO 专属头。
//
// 口径 = 2026-10-30 对官方客户端（Trae CN 1.107.1 / Trae 1.107.1）Agent 模式的真请求抓包，
// 两区各自一套，逐项对照见 .reverse/agent_headers_cn_vs_intl.txt。与旧实现相比的变化：
//
//   - UA 换成完整 Chromium 家族串（TraeCN/1.107.1 或 Trae/1.107.1），不是 "Trae/<ver>"；
//   - Accept 用 */*（流是服务端按 body.stream=true 回的 SSE，不靠 Accept 协商）；
//   - 删掉 X-Cloudide-Token 与 X-Uid —— 客户端在对话链路上都不发这两个；
//   - X-App-Version 按地区：国内发版本号，国际发字面量 "default"；
//   - 补齐客户端原生网络栈的自证头：x-harness / x-agent-host-type / x-bridge-transport /
//     x-lgw-req-sdk-type / package-type，以及 trace 三件套与 x-ahanet-timeout。
//   - **故意不发** x-request-pin / x-requested-at：见函数尾部的实测说明。
//
// 这些头里没有一个被验证为「缺了就 400」的硬门槛（旧头打 api5-normal 也是 200），
// 加齐的理由是身份一致性：缺的越多，越像自建代理而不是官方客户端。
func SOLOHeaders(req *http.Request, a *auth.Auth, stream bool) {
	realm := realmOf(a)
	ver := currentIdeVersionFor(realm)

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	// stream 不再影响任何头：客户端对两条路径都发 */*，流是 body.stream=true 决定的。
	_ = stream
	req.Header.Set("User-Agent", currentUAFor(realm))

	at := ""
	if a != nil {
		at = a.JWT() // 读锁快照，防与 RefreshToken 写并发竞态
	}
	if at != "" {
		req.Header.Set("Authorization", "Cloud-IDE-JWT "+at)
		req.Header.Set("X-Ide-Token", at)
	}

	// ── 身份四件套 ──────────────────────────────────────────────────────
	req.Header.Set("X-App-Id", AppID)
	req.Header.Set("X-App-Version", appVersionFor(realm))
	// 版本组按账号地区：国际版是另一套（X-App-Version-Code 必须非空，缺了上游 4001）。
	req.Header.Set("X-App-Version-Code", verCodeFor(realm))
	req.Header.Set("X-Ide-Version", ver)
	req.Header.Set("X-Ide-Version-Code", verCodeFor(realm))
	req.Header.Set("X-Ide-Version-Type", "stable")

	// ── 设备（客户端读真机，这里是常量兜底 + 运维覆盖） ────────────────
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("X-Device-Cpu", DeviceCPU)
	req.Header.Set("X-OS-Version", osVersion())
	req.Header.Set("X-Device-Brand", deviceBrand())
	if a != nil {
		if a.MachineID != "" {
			req.Header.Set("X-Machine-Id", a.MachineID)
		}
		if a.DeviceID != "" {
			req.Header.Set("X-Device-Id", a.DeviceID)
		}
	}

	// ── 原生网络栈自证（aha / TTNet 桥） ───────────────────────────────
	req.Header.Set("X-Harness", "1")
	req.Header.Set("X-Agent-Host-Type", "0")
	req.Header.Set("X-Bridge-Transport", "aha")
	req.Header.Set("X-Lgw-Req-Sdk-Type", "3")
	req.Header.Set("Request-Traffic-Type", "prod")

	// ── 地区差异化（国内发 lscbd/CN，国际发 stable_i18n + SS-DP） ──────
	applyRealmHeaders(req, realm)

	// ── trace / 会话元信息 ─────────────────────────────────────────────
	traceID := ugRandHex(16) // 32 hex，与客户端 x-custom-trace-id 同形
	spanID := ugRandHex(8)   // 16 hex，traceparent 的 parent-id
	req.Header.Set("X-Custom-Trace-Id", traceID)
	req.Header.Set("Traceparent", "00-"+traceID+"-"+spanID+"-01")
	req.Header.Set("X-Flow-Traceparent", "04-"+traceID+"-"+spanID+"-01")
	req.Header.Set("X-Request-Id", "req_"+ugUUIDv4())
	// x-ahanet-timeout 是纯传输层超时提示，单独带不影响明文解析（实测 200），留着更像客户端。
	req.Header.Set("X-Ahanet-Timeout", "86400")
	// 故意不发 x-request-pin / x-requested-at：这两个是客户端**加密载荷**的伴随头
	// （带它们的那次实测 body 是 X-Medusa/X-Helios 密文）。本仓发明文 body，带上它们上游
	// 会按密文去解 → 400 {"error":"base64 decode failed"}（2026-10-30 逐头二分实测复现，
	// 报错原文直接写 "x-request-pin or x-requested-at"）。
}

// verCodeFor 版本码按地区（两区实测同为 20260924，但走各自常量以便将来分化）。
func verCodeFor(realm string) string {
	if realm == auth.RealmIntl {
		return IdeVersionCodeIntl
	}
	return IdeVersionCode
}

// applyRealmHeaders 两区各自独有的 ug 族头。
//
//	CN   package-type: stable_cn / x-user-region: CN / x-lscbd-aid+platform / X-SS-DP: 787976
//	INTL package-type: stable_i18n / X-SS-DP: 677332 / store-country-code(-src)
//	     国际版**没有** x-user-region 与 x-lscbd-*（实测），不要照抄国内那套。
func applyRealmHeaders(req *http.Request, realm string) {
	if realm == auth.RealmIntl {
		req.Header.Set("Package-Type", "stable_i18n")
		req.Header.Set("X-SS-DP", "677332")
		return
	}
	req.Header.Set("Package-Type", "stable_cn")
	req.Header.Set("X-User-Region", "CN")
	req.Header.Set("X-Lscbd-Aid", "787976")
	req.Header.Set("X-Lscbd-Platform", "windows")
	req.Header.Set("X-SS-DP", "787976")
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
//
// ★ 已知差距（2026-10-30 抓包，尚未改）：国际版账号也走这个函数（见 client.go 的
// ugBaseFor），但这里发的是国内那套 ug 头。国际客户端实测是另一套：
//
//	package-type: stable_i18n / X-SS-DP: 677332 / store-country-code(-src)
//	且**没有** x-user-region 与 x-lscbd-*
//
// 没直接切的原因：手上没有能通过 API 鉴权的国际票据（.trae/trae-jwt-token 在国际各家
// agent/ug 网关上都是 401 code:1001），改了就无法验证「国际额度接口还能不能读」。
// 等有一枚能用的国际 refresh token 时，按 realmOf(a) 分叉即可，测试在
// TestUgHeadersIntlRealm（待补）。
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
	// App-Version 跟客户端对齐：2026-10-30 抓包实测 api.trae.cn 上发的是 3.4.11
	// （旧值 0.1.45 是历史遗留，已经落后好几个季度）。
	h.Set("App-Version", currentIdeVersionFor(auth.RealmCN))
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
