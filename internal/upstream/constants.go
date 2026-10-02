// constants.go SOLO 上游技术常量（SPEC §1，来自实测；改动须附实测证据）。
//
// **这一组值 = TRAE Work（原 TRAE SOLO）对话通道的身份**，不是 TRAE IDE 的。
// function / AppID / ClientID / 主机 / 版本码 任意一项换成 IDE 口径，模型表、门控与
// 计费归属都会跟着变；`TestWorkChannelIdentityFrozen` 用字面值把它们钉住，改前先看那条测试。
package upstream

const (
	AgentHost   = "https://trae-api-cn.mchost.guru"
	UgHost      = "https://api.trae.cn"
	OAuthHost   = "https://api.trae.com.cn"
	ConsoleHost = "https://www.trae.cn"
	ClientID    = "en1oxy7wnw8j9n" // SOLO stable
	AppID       = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	// IdeVersionCode 是上游的**放量开关**，不是装饰：
	//   20260716 → 表 39 条、kimi-k3 的 access.identity_list=[2,3,100]（免费档被挡）
	//   20260811 → 表 42 条、k3 放开到 [0,5,1,2,3,100]，多出 glm-5.3 / step-5-preview /
	//              deepseek-v4.1-flash，少掉 glm-5 / DeepSeek-V4-Flash / DeepSeek-V4-Pro
	// 版本号字符串本身不影响结果（0.1.43/0.1.50/1.0.0 同码同表），跟着版本码标成同批客户端即可。
	IdeVersion     = "0.1.50"
	IdeVersionCode = "20260811"
	DeviceBrand    = "83DG"
	OSVersion      = "Windows 11 Pro"
	Function       = "solo_work_lite"
	// FunctionCoder 是同一个 agent 主机的另一条通道：`solo_coder`。实测 2026-09-28，
	// 同 token 同路径，只换 function，返回的模型表就是网页端（work.trae.ai）那 14 个
	// （gemini-3.1-pro / gemini-3-flash-solo / minimax-m2.7 / kimi-k2.5 / gpt-5.4 / gpt-5.2 …）。
	// 反过来 Work 通道的 gpt-6-sol/kimi-k3 在 coder 表里没有 —— 两张表是两个产品面。
	FunctionCoder = "solo_coder"

	// 端点
	EpChat          = "/api/agent/v3/llm_utils_chat"
	EpModels        = "/api/ide/v1/get_detail_param"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"
)

// 国际版（realm=intl，实测 2026-09-28，见 README「国际版」）：
// **同一套 API、同样的 function=solo_work_lite 和同样的 SSE 事件**，只有域与身份版本组不同。
//   - agent host 换成 a0ai-api-sg.byteintlapi.com；oauth host 不写死：登录回调里的
//     `host` 参数会给（本次实测是 api-sg-central.trae.ai），落在 auth.ApiHost 上。
//   - ClientID / AppID 与国内版相同（回调里的 userJwt 与我手上常量逐字一致）。
//   - X-App-Version-Code 必须非空：缺了上游直接 4001 "missing required parameter"（实测）。
//   - 签到/积分（EpCheckin*/EpEntUsage）在国际两个域上都是 404，没有对应接口。
const (
	AgentHostIntl = "https://a0ai-api-sg.byteintlapi.com"
	// DefaultConfigNameIntl 国际版的旗舰/默认模型（别名与空 model 都落到它）。
	DefaultConfigNameIntl = "gpt-5.2"
	IdeVersionIntl        = "1.0.2"
	IdeVersionCodeIntl    = "20260811" // ponytail: 只要非空就能过，暂借国内版的值；有实测到国际专有码再换
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
