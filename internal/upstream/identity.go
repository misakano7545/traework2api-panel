// identity.go 出站身份（UA 与 Trae 版本段）的运行期覆盖。
//
// 内置默认对齐官方客户端分发包；面板的 upstream.user_agent / client_version 可以整体改写。
// 用包级变量而不是 Client 字段：头部构造函数（SOLOHeaders/UgHeaders/OAuthHeaders）是
// 无接收者的纯函数，全进程一份身份即可，没必要给每个请求传一遍。
package upstream

import (
	"sync"

	"traework2api/internal/auth"
)

var (
	identityMu  sync.RWMutex
	uaOverride  string
	verOverride string

	deviceMu    sync.RWMutex
	brandOverrd string
	osOverrd    string
)

// SetIdentity 覆盖出站 UA 与 Trae 版本段。空值 = 用内置默认（不覆盖）。
func SetIdentity(userAgent, version string) {
	identityMu.Lock()
	defer identityMu.Unlock()
	uaOverride = userAgent
	verOverride = version
}

// SetDevice 覆盖出站 X-Device-Brand / X-OS-Version（空值 = 用内置兜底）。
//
// 官方客户端这两个头读的是**真机**（实测这台机器发的是 "OpenStack Compute" /
// "Windows Server 2022 Standard"）。常量只是兜底，面板负责用真机信息覆盖。
func SetDevice(brand, osVersion string) {
	deviceMu.Lock()
	defer deviceMu.Unlock()
	brandOverrd = brand
	osOverrd = osVersion
}

// currentUA 出站 User-Agent：显式覆盖优先，否则按地区给客户端 UA 家族。
func currentUA() string { return currentUAFor(auth.RealmCN) }

// currentUAFor 按地区给出站 UA（覆盖优先）。
//
// 2026-10-30 抓包实测，客户端 UA 是完整 Chromium 串，不是 "Trae/<ver>"：
//
//	CN   ... (KHTML, like Gecko) TraeCN/1.107.1 Chrome/142.0.7444.235 Electron/39.2.7 Safari/537.36
//	INTL ... (KHTML, like Gecko) Trae/1.107.1   Chrome/142.0.7444.235 Electron/39.2.7 Safari/537.36
func currentUAFor(realm string) string {
	identityMu.RLock()
	ov := uaOverride
	identityMu.RUnlock()
	if ov != "" {
		return ov
	}
	return uaFor(realm)
}

// uaFor 拼客户端 UA 串。品牌段是 TraeCN / Trae，后面跟 VSCode 底座与 Chromium 版本。
func uaFor(realm string) string {
	brand := "TraeCN"
	if realm == auth.RealmIntl {
		brand = "Trae"
	}
	return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) " +
		brand + "/" + UABaseVersion + " Chrome/" + UAChromeVersion + " Electron/" + UAElectronVersion +
		" Safari/537.36"
}

// identOverrideVersion 运维显式钉死的 IDE 版本（空 = 没设）。国际版也要吃这个覆盖。
func identOverrideVersion() string {
	identityMu.RLock()
	defer identityMu.RUnlock()
	return verOverride
}

// currentIdeVersion 出站 X-Ide-Version：显式覆盖优先，否则内置 IdeVersion。
func currentIdeVersion() string { return currentIdeVersionFor(auth.RealmCN) }

// currentIdeVersionFor 出站 X-Ide-Version（按地区）。
func currentIdeVersionFor(realm string) string {
	identityMu.RLock()
	ov := verOverride
	identityMu.RUnlock()
	if ov != "" {
		return ov
	}
	if realm == auth.RealmIntl {
		return IdeVersionIntl
	}
	return IdeVersion
}

// appVersionFor 出站 X-App-Version（按地区）。两区口径不同，这是实测出来的：
//
//	CN   x-app-version: 3.4.11   —— 与 x-ide-version 同值
//	INTL x-app-version: default  —— 字面量 "default"，不是版本号
func appVersionFor(realm string) string {
	if realm == auth.RealmIntl {
		return "default"
	}
	return currentIdeVersionFor(realm)
}

// deviceBrand 出站 X-Device-Brand：覆盖优先，否则内置兜底。
func deviceBrand() string {
	deviceMu.RLock()
	defer deviceMu.RUnlock()
	if brandOverrd != "" {
		return brandOverrd
	}
	return DeviceBrand
}

// osVersion 出站 X-OS-Version：覆盖优先，否则内置兜底。
func osVersion() string {
	deviceMu.RLock()
	defer deviceMu.RUnlock()
	if osOverrd != "" {
		return osOverrd
	}
	return OSVersion
}

// realmOf 取账号地区，nil 账号按国内版（头部构造函数允许 a == nil）。
func realmOf(a *auth.Auth) string {
	if a == nil {
		return auth.RealmCN
	}
	return a.Realm()
}
