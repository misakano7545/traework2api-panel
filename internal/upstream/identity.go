// identity.go 出站身份（UA 与 Trae 版本段）的运行期覆盖。
//
// 内置默认对齐官方客户端分发包；面板的 upstream.user_agent / client_version 可以整体改写。
// 用包级变量而不是 Client 字段：头部构造函数（SOLOHeaders/UgHeaders/OAuthHeaders）是
// 无接收者的纯函数，全进程一份身份即可，没必要给每个请求传一遍。
package upstream

import "sync"

var (
	identityMu  sync.RWMutex
	uaOverride  string
	verOverride string
)

// SetIdentity 覆盖出站 UA 与 Trae 版本段。空值 = 用内置默认（不覆盖）。
func SetIdentity(userAgent, version string) {
	identityMu.Lock()
	defer identityMu.Unlock()
	uaOverride = userAgent
	verOverride = version
}

// currentUA 出站 User-Agent：显式覆盖优先，否则 "Trae/<版本>"。
func currentUA() string {
	identityMu.RLock()
	defer identityMu.RUnlock()
	if uaOverride != "" {
		return uaOverride
	}
	return "Trae/" + currentVerLocked()
}

// currentIdeVersion 出站 X-Ide-Version：显式覆盖优先，否则内置 IdeVersion。
func currentIdeVersion() string {
	identityMu.RLock()
	defer identityMu.RUnlock()
	return currentVerLocked()
}

// currentVerLocked 调用方需持锁（RWMutex 不可重入，故拆出内部版本）。
func currentVerLocked() string {
	if verOverride != "" {
		return verOverride
	}
	return IdeVersion
}
