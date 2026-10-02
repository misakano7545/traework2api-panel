// resolve_model.go 模型名里的地区前缀协议：`[realm:]model`（realm 只认 cn / intl）。
// 口径照 workbuddy2api-panel 的 internal/server/resolve_model.go（那边叫 global，这边叫 intl）：
// 前缀是**网关侧的路由协议**，上游不认；出站/选号/台账一律用剥掉前缀的裸名。
package server

import (
	"strings"

	"traework2api/internal/auth"
)

// resolveModel 取第一个 ":"，前段恰为 "cn"/"intl"（大小写敏感）才当地区前缀；否则整串按裸名、
// 地区取国内版。裸名时 bare == 原串，老客户端的请求行为零变化。
func resolveModel(model string) (realm, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return auth.RealmCN, model
	}
	prefix := model[:idx]
	if prefix != auth.RealmCN && prefix != auth.RealmIntl {
		return auth.RealmCN, model
	}
	return prefix, model[idx+1:]
}
