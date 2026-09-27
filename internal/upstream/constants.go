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

	// 端点
	EpChat          = "/api/agent/v3/llm_utils_chat"
	EpModels        = "/api/ide/v1/get_detail_param"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"
)
