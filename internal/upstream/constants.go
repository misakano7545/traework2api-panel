// constants.go SOLO 上游技术常量（SPEC §1，来自实测；改动须附实测证据）。
//
// **这一组值 = TRAE Work（原 TRAE SOLO）对话通道的身份**，不是 TRAE IDE 的。
// function / AppID / ClientID / 主机 / 版本码 任意一项换成 IDE 口径，模型表、门控与
// 计费归属都会跟着变；`TestWorkChannelIdentityFrozen` 用字面值把它们钉住，改前先看那条测试。
package upstream

const (
	// AgentHost 对话/模型表的 agent 网关。
	//
	// 2026-10-15 实测：合并后的官方客户端（Trae CN 1.1.3，build 2026-10-09，
	// TraeWork 与 TraeCode 已合一）全量改走 api5-normal；trae-api-cn 仍是**活着**的
	// 遗留入口（未鉴权探测两条 host 的 EpChat/EpModels 都回 401 code:1001，不是 404），
	// 旧域名只剩新客户端 referer 里的残留，所以这次是「早改无害」而非救火。
	//
	// 同批实测对拍（真 token）：两条 host × 两套身份版本（0.1.50/20260811 与
	// 3.4.2/20260924）× 单 function 表 / 7-function 批量表，全部 200 且逐字一致
	// （46 条 / 7 函数面 437 条 / client_config 3203B）；明文请求体在 api5-normal 上
	// 照常出完整 SSE 正文 —— 官方客户端的 x-medusa/x-helios 请求体加密**不是强制项**。
	// 新域名 SSE 每帧多一行 `id:N`，两个解析器（solosse.go scanLine / continue.go step）
	// 都只认 event:/data:/: 前缀，该行被忽略，无需改动。
	AgentHost   = "https://api5-normal.mchost.guru"
	UgHost      = "https://api.trae.cn"
	OAuthHost   = "https://api.trae.com.cn"
	ConsoleHost = "https://www.trae.cn"
	// ClientID 保持 SOLO 档，**不要**改成 IDE 档的 ono9krqynydwx5。
	//
	// 2026-10-15 实测：refresh token 与 ClientID **严格绑定**。同一枚令牌，
	// 配对正确 → 200；跨档配对 → 400 10101「无效参数」。2×2 全交叉矩阵在两个
	// host 上、正序倒序各跑一遍，结果一致（BoundDeviceID 也分档：SOLO 28x5y7nrx6rlq5 /
	// IDE 6194p600l8je46）。
	// ⇒ 本仓所有已有账号的 token 都是 SOLO 档登录铸出来的，改成 IDE 档会让**全部账号**
	// 换票失败。SOLO 档本身健在：GetPCAuthCode 两档都 200，伪造 client_id 才 400
	// 「Invalid client.」（有区分力的对照）。
	ClientID = "en1oxy7wnw8j9n" // SOLO stable
	AppID    = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	// IdeVersionCode 是上游的**放量开关**，不是装饰：
	//   20260716 → 表 39 条、kimi-k3 的 access.identity_list=[2,3,100]（免费档被挡）
	//   20260811 → 表 42 条、k3 放开到 [0,5,1,2,3,100]，多出 glm-5.3 / step-5-preview /
	//              deepseek-v4.1-flash，少掉 glm-5 / DeepSeek-V4-Flash / DeepSeek-V4-Pro
	//   20260924 → 48 条（实测 2026-10-30，与 20260811 的模型名**逐字相同**，只是表变长）
	//
	// 2026-10-30 抓包实测（Trae CN 1.107.1 / appVersion 3.4.11，Agent 模式真请求）：
	//   x-app-version / x-ide-version = 3.4.11，两个 version-code 都是 20260924。
	//   注意 product.json 里的 traeVersionCode=20250325 是**死值**，客户端实际发的是运行时
	//   下发的 20260924 —— 在整个安装目录 grep 20260924 零命中，静态扫不出来，只有抓包有。
	// 版本号字符串本身不换表（20260811 与 20260924 实测同表），但它是身份指纹，跟着客户端对齐。
	IdeVersion     = "3.4.11"
	IdeVersionCode = "20260924"
	// DeviceBrand / OSVersion 是**兜底值**：官方客户端读真机（实测这台机器上发的是
	// "OpenStack Compute" / "Windows Server 2022 Standard"）。账号没记真机信息时用这两个，
	// 由 upstream.device_brand / os_version 或 SetIdentity 覆盖成真实值。
	DeviceBrand = "83DG"
	OSVersion   = "Windows 11 Pro"

	// 客户端 UA 家族，2026-10-30 抓包实测（UA 是完整 Chromium 串，不是 "Trae/<ver>"）：
	//	CN   ... TraeCN/1.107.1 Chrome/142.0.7444.235 Electron/39.2.7 Safari/537.36
	//	INTL ... Trae/1.107.1   Chrome/142.0.7444.235 Electron/39.2.7 Safari/537.36
	// UABaseVersion 取 product.json 的 vscodeVersion（两区同为 1.107.1），**不是** appVersion。
	UABaseVersion     = "1.107.1"
	UAChromeVersion   = "142.0.7444.235"
	UAElectronVersion = "39.2.7"

	// DeviceCPU 出站 X-Device-Cpu。客户端读真机（实测这台是 "Intel"）。
	DeviceCPU = "Intel"

	Function = "solo_work_lite"
	// FunctionCoder 是同一个 agent 主机的另一条通道：`solo_coder`。实测 2026-09-28，
	// 同 token 同路径，只换 function，返回的模型表就是网页端（work.trae.ai）那 14 个
	// （gemini-3.1-pro / gemini-3-flash-solo / minimax-m2.7 / kimi-k2.5 / gpt-5.4 / gpt-5.2 …）。
	// 反过来 Work 通道的 gpt-6-sol/kimi-k3 在 coder 表里没有 —— 两张表是两个产品面。
	FunctionCoder = "solo_coder"

	// 端点
	EpChat   = "/api/agent/v3/llm_utils_chat"
	EpModels = "/api/ide/v1/get_detail_param"
	// EpModelsBatch 客户端**模型选择器**用的批量端点：一次要 7 个 function，每组一份配置表。
	// 与单 function 的 EpModels 是两个视图（可见性标志与上下文口径都不同），
	// 见 fetchBatchCatalog 的说明。同一 host 上并存，客户端走这个。
	EpModelsBatch   = "/api/ide/v1/batch_get_detail_param"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	// 活动/权益领取（2026-10-19 对 6 个真号实测，只在 api.trae.cn 上存在；
	// api.trae.com.cn 上全 404，所以必须走 ugBase 而不是账号的 ApiHost）。
	EpActivityInfo   = "/trae/api/v2/ug/activity/info"
	EpActivityAction = "/trae/api/v2/ug/activity/action"
	EpGrantBenefit   = "/trae/api/v2/pay/grant_work_benefit"
	EpFissionGrant   = "/trae/api/v2/ug/work_fission/grant"
	// EpEntUsage 国内版积分/套餐构成（v2）。
	EpEntUsage = "/trae/api/v2/pay/ide_user_ent_usage"
	// EpEntUsageIntl 国际版同一个接口的 **v1** 路径（实测 2026-10-03）：
	//   国际域只提供 v1（`/trae/api/v2/...` 在 api-sg-central 上恒 404），
	//   响应体与国内版同形（user_entitlement_pack_list + quota.credits_limit），
	//   但**没有** is_credits_billing / usage_summary（那两项是 v2 独有）。
	EpEntUsageIntl = "/trae/api/v1/pay/ide_user_ent_usage"
)

// 国际版（realm=intl，实测 2026-09-28，见 README「国际版」）：
// **同一套 API、同样的 function=solo_work_lite 和同样的 SSE 事件**，只有域与身份版本组不同。
//   - agent host 换成 a0ai-api-sg.byteintlapi.com；oauth host 不写死：登录回调里的
//     `host` 参数会给（本次实测是 api-sg-central.trae.ai），落在 auth.ApiHost 上。
//   - ClientID / AppID 与国内版相同（回调里的 userJwt 与我手上常量逐字一致）。
//   - X-App-Version-Code 必须非空：缺了上游直接 4001 "missing required parameter"（实测）。
//   - 签到（EpCheckin*）在国际两个域上都是 404，没有对应接口。
//   - 积分/套餐 **有** 接口，只是走 v1 路径且落在计费网关上（见 EpEntUsageIntl / UgHostIntl，
//     实测 2026-10-03）：国际版免费号返回单条 Free plan（credits_limit=0，按美元计费），
//     付费国际号则带 credits_limit。
const (
	// AgentHostIntl 国际版的 agent 网关。2026-10-30 抓包实测：国际客户端（Trae 1.107.1 /
	// appVersion 3.6.4）的 Referer 与逻辑域名都是 coresg-normal.trae.ai（其它区是
	// core-normal / coreva-normal），传输层再 dispatch 到 api5-normal-alisg.mchost.guru。
	// 旧值 a0ai-api-sg.byteintlapi.com 仍然活着（探测回 401 code:1001，路由在），
	// 但它不是客户端在用的那台 —— 以客户端为准换成 coresg。
	AgentHostIntl = "https://coresg-normal.trae.ai"
	// UgHostIntl 国际版的 UG/计费网关（api.trae.cn 的国际对应物）。账号的 ApiHost
	// （登录回调 host）优先；老账号没有这个字段时回落到这里。
	//
	// 2026-10-30 抓包实测：国际客户端自己的 ug 请求打的是 growsg-normal.trae.ai。
	// 本常量保留 api-sg-central.trae.ai 是因为实测**两台都能路由**（拿同一枚票据、
	// 同一套头打 /trae/api/v1/pay/ide_user_ent_usage，两边都 200 且响应逐字一致），
	// 而 api-sg-central 与登录回调给出的 ApiHost 同域，老账号回落时更稳。
	// 要完全贴客户端就换成 https://growsg-normal.trae.ai。
	UgHostIntl = "https://api-sg-central.trae.ai"
	// DefaultConfigNameIntl 国际版的旗舰/默认模型（别名与空 model 都落到它）。
	DefaultConfigNameIntl = "gpt-5.2"
	// 国际版身份版本组，2026-10-30 抓包实测（国际客户端 Agent 真请求）：
	//   x-ide-version = 3.6.4（= product.json appVersion），x-ide-version-code =
	//   x-app-version-code = 20260924，package-type = stable_i18n，X-SS-DP = 677332。
	//   国际版 x-app-version 发的是字面量 "default"（国内版发版本号）。
	IdeVersionIntl     = "3.6.4"
	IdeVersionCodeIntl = "20260924"
)

// SlotRoute 走租户自定义槽位的模型：config_name 指槽位、model 指槽位里具体那个上游模型。
type SlotRoute struct {
	Slot  string // config_name
	Model string // model
}

// intlSlotRoutes 国际版「没开档位」的模型的旁路（实测 2026-09-28）：
// 官方 gpt-6-sol / gpt-6-luna 直接发给 1005 `{"plan":4}`（要付费档），但同一批模型在
// 租户自定义槽位 custom_model_gpt-5 里以 openai/gpt-5.6-sol|luna 命名，真发出正文。
// ponytail: 只登记实测过的两条；槽位是租户配置，换账号可能没有，没有就当普通模型报错。
var intlSlotRoutes = map[string]SlotRoute{
	"gpt-6-sol":  {"custom_model_gpt-5", "openai/gpt-5.6-sol"},
	"gpt-6-luna": {"custom_model_gpt-5", "openai/gpt-5.6-luna"},
}

// IntlSlotRoute 查国际版模型的槽位旁路；没有则 ok=false（按普通模型走）。
func IntlSlotRoute(model string) (SlotRoute, bool) {
	r, ok := intlSlotRoutes[model]
	return r, ok
}
