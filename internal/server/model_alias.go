// model_alias.go 客户端自带的模型名 → 本项目真能调的模型。
//
// 做法照社区现成实现（ZedeX/trae-local-api 的 model-config.json：客户端名字 → config_name 表；
// linqiu919/trae2api 的 convertModelName 同理）：Claude Code / Cursor / Cline 这类工具把
// claude-*、gpt-4o 写死在配置里，别名层让它们零改造接进来，不用手改每个客户端的模型名。
//
// ponytail: 只登记「目标确实存在且实测出正文」的映射；这里不做智能路由、不做 tier 回退
// （参考实现有 tiers/fallback 那套，等真需要多家竞速再加）。
package server

import (
	"strings"

	"traework2api/internal/auth"
	"traework2api/internal/upstream"
)

// strongModel 各地区的旗舰模型：别名没有更贴切的目标时就落到它。
func strongModel(realm string) string {
	if realm == auth.RealmIntl {
		return upstream.DefaultConfigNameIntl
	}
	return upstream.DefaultConfigName
}

// modelAliases 显式别名（小写、已去 __ 后缀）。
// ponytail: 网页端（work.trae.ai）那 11 个名字里，gemini/minimax/kimi-k2.5 只在
// solo_coder 通道存在，且账号配额可能为 0（实测 4011）——这里仍照实映射，
// 让客户端拿到上游的真话，而不是"unknown model"。
var modelAliases = map[string]map[string]string{
	auth.RealmCN: {
		"gpt-4o": "glm-5.2", "gpt-4o-mini": "glm-5.2", "gpt-4.1": "glm-5.2",
		"gpt-4-turbo": "glm-5.2", "gpt-4": "glm-5.2", "gpt-3.5-turbo": "glm-5.2",
	},
	auth.RealmIntl: {
		"gpt-4o": "gpt-5.2", "gpt-4o-mini": "gpt-5.2", "gpt-4.1": "gpt-5.2",
		"gpt-4-turbo": "gpt-5.2", "gpt-4": "gpt-5.2", "gpt-3.5-turbo": "gpt-5.2",
		"gpt-5": "gpt-5.2", "gpt-5.1": "gpt-5.2",
		// 网页端显示名 → 上游真名（coder 通道）。同一个模型的两个名字才映射；
		// minimax-m3 这台账号的表里根本没有（只有 m2.7），不做降级映射，照实 400。
		"gemini-3.1-pro-preview": "gemini-3.1-pro",
		"gemini-3-flash-preview": "gemini-3-flash-solo",
		"kimi-k2.5":              "kimi-k2.5",
	},
}

// otherRealm 另一个地区（别名兜底用）。
func otherRealm(realm string) string {
	if realm == auth.RealmIntl {
		return auth.RealmCN
	}
	return auth.RealmIntl
}

// aliasModel 查别名。claude-* 走前缀（Claude 客户端会带日期后缀，逐个登记不划算）。
// 入参可能是 normalizeModelName 出来的「Claude-sonnet-4-5」（首字母被大写），统一转小写再比。
// 本地区表里没有就查另一地区：网页端显示名（Gemini-3.1-Pro-Preview）不带地区信息，
// 命中的目标名会由调用方回查 modelRealm 决定真正发哪个地区的号。
func aliasModel(realm, model string) (string, bool) {
	model = strings.ToLower(model)
	if strings.HasPrefix(model, "claude") {
		return strongModel(realm), true
	}
	for _, r := range []string{realm, otherRealm(realm)} {
		if m, ok := modelAliases[r][model]; ok {
			return m, true
		}
	}
	return "", false
}
