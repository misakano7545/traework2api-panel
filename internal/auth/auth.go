// Package auth 解析 TRAE SOLO auth 文件（嵌套形：auth + account），
// 提供原子写回与目录扫描。
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"traework2api/internal/safefile"
)

// validCredFile 判断一份凭证文件内容是否可用（LoadDir 与 SaveAtomic 共用同一口径）。
func validCredFile(b []byte) bool { _, err := Parse(b); return err == nil }

// Auth 归一化后的账号凭证。
//
// 并发模型：mu 保护可变字段（AccessToken/RefreshToken/ExpiresAt），
// 写路径（upstream.RefreshToken）持写锁整段执行 ExchangeToken；
// 读路径（NeedsRefresh/JWT/RefreshTokenValue）持读锁读取快照，
// 杜绝与写并发时的数据竞争（-race 实测确认）。
// 其余字段（Domain/ApiHost/MachineID/DeviceID/UID/...）加载后不变，直接读。
type Auth struct {
	mu sync.RWMutex

	AccessToken  string // Cloud-IDE-JWT 头用
	RefreshToken string // 每次 ExchangeToken 轮换
	ExpiresAt    int64  // Unix 秒（accessToken 过期时刻）
	Domain       string // "trae.cn"
	ApiHost      string // "https://api.trae.com.cn"（ExchangeToken host）
	MachineID    string // x-machine-id
	DeviceID     string // x-device-id
	// DeviceSeed ug 族（签到/积分）伪设备指纹的种子：0 = 按 uid 派生的原始身份，
	// 非 0 = 换过指纹（把种子拌进派生输入）。★ 与 DeviceID 不是一回事：DeviceID 是登录
	// 时拿到的客户端设备号（对话路径用），ug 族用的是这里派生的那套。
	DeviceSeed   int64
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 落盘路径；refresh 后原子写回
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加写锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的写锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// RLock 供读路径持有读锁（与写锁互斥，读读不互斥）。
func (a *Auth) RLock() { a.mu.RLock() }

// RUnlock 释放 a.RLock 获取的读锁。
func (a *Auth) RUnlock() { a.mu.RUnlock() }

// JWT 返回当前 accessToken 的读锁快照，防与 RefreshToken 写并发竞态。
func (a *Auth) JWT() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken
}

// 地区（realm）：国内版与国际版是同一套 API 的两套域 + 两套身份版本组。
const (
	RealmCN   = "cn"
	RealmIntl = "intl"
)

// knownHostDomains 国际版已知域。判据必须按**域名边界**匹配：用 strings.Contains 时
// `https://trae.ai.attacker.com` 也过关，而这个 host 会被写进 a.ApiHost 当换票/取用户
// 信息的基址——真实 token 就发到那台机器上了（面板密钥泄露即全号外泄）。
var knownHostDomains = []string{"trae.ai", "byteintlapi.com"}

// IsKnownHostDomain 判断 s（URL 或裸 host，可带端口）是否落在国际版已知域里。
func IsKnownHostDomain(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s // 回调里的 host 参数可能不带 scheme
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	for _, d := range knownHostDomains {
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	return false
}

// Realm 从凭证自带的 Domain/ApiHost 判地区。老 auth 文件没有这两个字段（或写成
// trae.cn 口径）一律按国内版；国际版是登录回调里的 host 参数写进来的。
func (a *Auth) Realm() string {
	if IsKnownHostDomain(a.Domain) || IsKnownHostDomain(a.ApiHost) {
		return RealmIntl
	}
	return RealmCN
}

// RefreshTokenValue 返回当前 refreshToken 的读锁快照，防与 RefreshToken 写并发竞态。
func (a *Auth) RefreshTokenValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.RefreshToken
}

// DeviceSeedValue 读 ug 族设备指纹种子（同 JWT/RefreshTokenValue 的读锁口径）。
func (a *Auth) DeviceSeedValue() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.DeviceSeed
}

// SetDeviceSeed 换一套 ug 族设备指纹（调用方负责随后 SaveAtomic 落盘）。
func (a *Auth) SetDeviceSeed(seed int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.DeviceSeed = seed
}

// ExpiresAtValue 读 accessToken 到期时刻（Unix 秒），同 JWT 的读锁口径（面板「Token 到期」列）。
func (a *Auth) ExpiresAtValue() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ExpiresAt
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.NeedsRefreshLocked(within)
}

// NeedsRefreshLocked 是 NeedsRefresh 的持锁内部版本；调用方必须已持有 a.mu（读或写锁）。
func (a *Auth) NeedsRefreshLocked(within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// parseNested 兼容现有 trae-*.json 嵌套形：
//
//	{"account":{...},"auth":{...}}
func parseNested(raw []byte) (*Auth, error) {
	var n struct {
		Auth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			ApiHost      string `json:"apiHost"`
			MachineID    string `json:"machineId"`
			DeviceID     string `json:"deviceId"`
			DeviceSeed   int64  `json:"deviceSeed"`
		} `json:"auth"`
		Account struct {
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	return &Auth{
		AccessToken:  n.Auth.AccessToken,
		RefreshToken: n.Auth.RefreshToken,
		ExpiresAt:    n.Auth.ExpiresAt,
		Domain:       n.Auth.Domain,
		ApiHost:      n.Auth.ApiHost,
		MachineID:    n.Auth.MachineID,
		DeviceID:     n.Auth.DeviceID,
		DeviceSeed:   n.Auth.DeviceSeed,
		UID:          n.Account.UID,
		EnterpriseID: n.Account.EnterpriseID,
		Nickname:     n.Account.Nickname,
	}, nil
}

// parseFlat 兼容扁平形（CPA 面板手建等）：
//
//	{"accessToken":...,"uid":...,"machineId":...,"deviceId":...}
func parseFlat(raw []byte) (*Auth, error) {
	var f struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
		ApiHost      string `json:"apiHost"`
		MachineID    string `json:"machineId"`
		DeviceID     string `json:"deviceId"`
		DeviceSeed   int64  `json:"deviceSeed"`
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	return &Auth{
		AccessToken:  f.AccessToken,
		RefreshToken: f.RefreshToken,
		ExpiresAt:    f.ExpiresAt,
		Domain:       f.Domain,
		ApiHost:      f.ApiHost,
		MachineID:    f.MachineID,
		DeviceID:     f.DeviceID,
		DeviceSeed:   f.DeviceSeed,
		UID:          f.UID,
		EnterpriseID: f.EnterpriseID,
		Nickname:     f.Nickname,
	}, nil
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （登录脚本产出，现有 trae-*.json）
//	扁平形 {"accessToken":...,"uid":...}   （手建）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var (
		a   *Auth
		err error
	)
	if _, nested := probe["auth"]; nested {
		a, err = parseNested(raw)
	} else {
		a, err = parseFlat(raw)
	}
	if err != nil {
		return nil, err
	}
	// 只含 refreshToken 的文件同样是可用账号：签到/保活会用它换回 access token。
	// 两个都没有才判无效——否则这种账号会被静默丢掉，面板上凭空少一个号。
	if strings.TrimSpace(a.AccessToken) == "" && strings.TrimSpace(a.RefreshToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken and refreshToken")
	}
	return a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename，0600），保持登录脚本可读格式。
// 加锁外壳：防止与 RefreshToken 并发读写 token 字段导致写回半更新。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveAtomicLocked()
}

// saveAtomicLocked 是 SaveAtomic 的持锁内部版本；调用方必须已持有 a.mu。
func (a *Auth) saveAtomicLocked() error {
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"apiHost":      a.ApiHost,
			"machineId":    a.MachineID,
			"deviceId":     a.DeviceID,
			"deviceSeed":   a.DeviceSeed,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	// safefile 用同目录唯一临时名（.<base>.*）：同一个文件可能被**两个不同的 *Auth 对象**写
	// （面板登录新建的对象 vs 池里已有对象），a.mu 是对象级锁保护不到；固定名 ".tmp" 时两边
	// 会互相截断，rename 上去就是半截 JSON，重启 LoadDir 静默跳过 → 账号无声消失。
	// safefile.WriteKeep：上一代留成 .bak，临时文件 fsync 后才 rename。
	return safefile.WriteKeep(a.FilePath, raw, 0o600)
}

// LoadDir 扫描 dir 下 trae-*.json。解析失败的文件跳过并留日志（静默跳过 = 账号凭空消失）。
//
// 不用 filepath.Glob：目录名里带 [ ] * ? 时 Glob 会把它们当模式，展开出空集或**别的目录**的
// 文件（实测 auths[1]/ 下 1 个账号被读成 0 个）。公开的目录名不该被当模式解析。
func LoadDir(dir string) ([]*Auth, error) {
	entries, err := os.ReadDir(dir)
	// 首次启动没有 auths/ 是常态，不是故障：建出来当 0 个账号，别 fatal 掉整个服务
	// （issue #2：`load auths: open ./auths: The system cannot find the file specified.`）。
	if errors.Is(err, fs.ErrNotExist) {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return nil, mkErr
		}
		log.Printf("[auth] 目录 %s 不存在，已自动创建（往里放 trae-<uid>.json 即接入账号）", dir)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "trae-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		f := filepath.Join(dir, name)
		// 读不出来时回落到 <file>.bak（上一代好副本），别让一个坏文件等于这个号消失。
		raw, rerr := safefile.Read(f, validCredFile)
		if rerr != nil {
			log.Printf("[auth] 账号文件 %s 读不出来，跳过: %v", f, rerr)
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			log.Printf("[auth] 账号文件 %s 解析失败，跳过: %v", f, err)
			continue
		}
		if strings.TrimSpace(a.AccessToken) == "" {
			log.Printf("[auth] %s 只有 refreshToken：下一轮签到/保活会取回 access token", f)
		}
		if strings.TrimSpace(a.UID) == "" {
			// 池按 uid 建索引：uid 为空的两个文件会**互相覆盖**，只剩一个号。
			// 文件名本来就带 uid（trae-<uid>.json），拿它兜底。
			a.UID = strings.TrimSuffix(strings.TrimPrefix(name, "trae-"), ".json")
			log.Printf("[auth] %s 里没有 uid，按文件名补成 %q（否则同为空 uid 的账号会互相覆盖）", f, a.UID)
		}
		a.FilePath = f
		out = append(out, a)
	}
	return out, nil
}
