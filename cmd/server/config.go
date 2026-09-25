// config.go 加载 JSON 配置 + TW2A_* 环境变量覆盖。
//
// 结构对齐 workbuddy2api-panel（同段同键，两个面板可以对照着读）。差异只保留真实的那些：
//   - trae 没有对应能力的 wb 键**不写进来**（schedule.travel/activity/blackcat_*、global、
//     upstash、features、pool.max_in_flight_global/cost_explore_interval、
//     upstream.device_token*/cli_version）。空壳字段改了没反应，比没有更误导。
//   - trae 扩展键：default_model、cooldown.plan_credit。
//
// APIKey 只来自 config.json 的 api_key（面板可改、如实回显）。首次运行生成的配置里 api_key 是随机
// 生成的 sk-… 密钥（空 key + 默认监听 0.0.0.0 等于把网关裸暴露，随机优于空值）
// （wb 会随机生成一个落盘；这里不静默往磁盘写密钥，要开鉴权就改文件或在面板里填）。
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"traework2api/internal/prompt"
)

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7864"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权（仅本机用）；单一来源就是这一项
	AuthDir   string `json:"auth_dir"`   // "./auths"
	StateFile string `json:"state_file"` // "./data/state.json"
	// DefaultModel trae 扩展：wb 无此键（它按 /v1/models 列表路由）。请求缺 model 时用它。
	DefaultModel string `json:"default_model"` // "glm-5.2"

	Cooldown struct {
		// PlanCredit trae 扩展：1005 权益不足的硬冷却时长（wb 固定成"到次日 04:00"，没有键）。
		PlanCredit string `json:"plan_credit"` // "12h"
		// SoftRate 软限流（429/404）冷却基数；SoftRateMax 是反复触发时指数退避的封顶。
		SoftRate    string `json:"soft_rate"`     // "60s"
		SoftRateMax string `json:"soft_rate_max"` // "2h"
	} `json:"cooldown"`

	Schedule struct {
		// CheckinHours 每日签到时点；KeepaliveHours token 预刷新时点（均在 0-23）。
		CheckinHours   []int `json:"checkin_hours"`   // [9]
		KeepaliveHours []int `json:"keepalive_hours"` // [3]
		// 开关缺省 true：键缺席保留默认，空数组仍回落默认时点——「禁用」只走开关，语义不混。
		CheckinEnabled   bool `json:"checkin_enabled"`
		KeepaliveEnabled bool `json:"keepalive_enabled"`
		// BalanceRefresh* 两次签到之间也刷新积分（面板/状态观测更准），不做签到、不刷 token。
		BalanceRefreshEnabled bool `json:"balance_refresh_enabled"`
		BalanceRefreshMinutes int  `json:"balance_refresh_minutes"`
	} `json:"schedule"`

	Upstream struct {
		// TimeoutSeconds 短 RPC（refresh/签到/积分/取模型）总时长上限。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 聊天首字节前上限（长推理预留，不限制整流时长）；<=0 回落 TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 聊天 SSE 流内空闲上限（持续吐数据不掐）；<=0 回落 300。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
		// UserAgent 出站 UA 覆盖；空 = 内置 "Trae/<client_version>"。
		UserAgent string `json:"user_agent"`
		// ClientVersion Trae 客户端版本段（出站 UA 与 X-Ide-Version）；空 = 内置默认。
		ClientVersion string `json:"client_version"`
	} `json:"upstream"`

	Pool struct {
		MaxInFlight int `json:"max_in_flight"` // 单账号在途上限；0 = 不限
		// 熔断（罚号失败：429/5xx/传输层）：连败达阈出池 breaker_cooldown，反复触发指数退避到 max。
		BreakerThreshold   int    `json:"breaker_threshold"`    // 3
		BreakerCooldown    string `json:"breaker_cooldown"`     // "10m"
		BreakerCooldownMax string `json:"breaker_cooldown_max"` // "2h"
		// 降权（不罚号失败：4xx 参数/404 等）：连败达阈临时出池，时长固定不叠加（超 max 才钳制）。
		DegradeThreshold   int    `json:"degrade_threshold"`    // 5
		DegradeCooldown    string `json:"degrade_cooldown"`     // "10m"
		DegradeCooldownMax string `json:"degrade_cooldown_max"` // "2h"
		// 闲置补偿：每小时没用到的账号在选号时加分，封顶 idle_weight_max（防冷号永远轮不到）。
		IdleWeightPerHour float64 `json:"idle_weight_per_hour"` // 0.5
		IdleWeightMax     float64 `json:"idle_weight_max"`      // 5
		// ExpiringSoon 快过期窗口（如 "168h"）：窗口内到期的积分优先消耗；空 = 禁用分桶。
		ExpiringSoon string `json:"expiring_soon"`
	} `json:"pool"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 同一会话粘同一账号（上游 prompt 缓存命中）
		TTL        string `json:"ttl"`         // "30m"
		GCInterval string `json:"gc_interval"` // "5m"
	} `json:"session_sticky"`

	Prompt struct {
		Mode string `json:"mode"` // passthrough（默认）/ custom / append
		File string `json:"file"` // 空 = 内置默认提示词
	} `json:"prompt"`

	// PromptText 解析后的提示词文本（custom/append 模式使用）。
	PromptText string `json:"-"`


	// 解析后
	PlanCreditDur       time.Duration `json:"-"`
	SoftRateDur         time.Duration `json:"-"`
	SoftRateMaxDur      time.Duration `json:"-"`
	BreakerCooldownDur  time.Duration `json:"-"`
	BreakerCooldownMaxD time.Duration `json:"-"`
	DegradeCooldownDur  time.Duration `json:"-"`
	DegradeCooldownMaxD time.Duration `json:"-"`
	SessionTTL          time.Duration `json:"-"`
	SessionGCInterval   time.Duration `json:"-"`
	BalanceRefreshInt   time.Duration `json:"-"` // 0 = 不启动（enabled=false）
	ExpiringSoonDur     time.Duration `json:"-"` // 0 = 禁用
}

// Default 返回默认配置。
func Default() *Config {
	c := &Config{
		Listen:       ":7864",
		APIKey:       "",
		AuthDir:      "./auths",
		StateFile:    "./data/state.json",
		DefaultModel: "glm-5.2",
	}
	c.Cooldown.PlanCredit = "12h"
	c.Cooldown.SoftRate = "60s"
	c.Cooldown.SoftRateMax = "2h"
	c.Schedule.CheckinHours = []int{9}
	c.Schedule.KeepaliveHours = []int{3}
	// 开关「缺省 true」靠这几行：Load 先取 Default() 再被 JSON 覆盖，键缺席即保留 true。
	c.Schedule.CheckinEnabled = true
	c.Schedule.KeepaliveEnabled = true
	c.Schedule.BalanceRefreshEnabled = true
	c.Schedule.BalanceRefreshMinutes = 5
	c.Upstream.TimeoutSeconds = 120
	c.Pool.MaxInFlight = 3
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "10m"
	c.Pool.BreakerCooldownMax = "2h"
	c.Pool.DegradeThreshold = 5
	c.Pool.DegradeCooldown = "10m"
	c.Pool.DegradeCooldownMax = "2h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5
	c.Pool.ExpiringSoon = "168h"
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	c.Prompt.Mode = "passthrough"
	return c
}

// Load 从 path 读配置，再用 TW2A_* env 覆盖。path 为空或不存在时用默认 + env。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				// 配置文件可选：不存在 → 纯默认 + env
				c = Default()
			} else {
				return nil, fmt.Errorf("read config: %w", err)
			}
		} else if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// WriteDefault 在 path 落一份推荐配置（首次运行自动生成，省掉手工复制样例）。api_key 用
// crypto/rand 随机生成：默认监听 0.0.0.0 配空 key 会把网关裸暴露给局域网，随机密钥是安全默认。
// 返回生成的 key 供启动日志透出。已存在时 O_EXCL 原子拒绝覆盖——绝不改写用户配置，
// 所以老配置里空着 api_key 就继续空着（改文件或在面板里填都行）。
func WriteDefault(path string) (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("gen api_key: %w", err)
	}
	key := "sk-" + base64.RawURLEncoding.EncodeToString(raw)
	c := Default()
	c.APIKey = key
	if err := c.normalize(); err != nil {
		return "", err
	}
	out, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}
	out = append(out, '\n')
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("mkdir config dir: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(out); err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	return key, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("TW2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("TW2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("TW2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("TW2A_DEFAULT_MODEL"); v != "" {
		c.DefaultModel = v
	}
	if v := os.Getenv("TW2A_PLAN_CREDIT"); v != "" {
		c.Cooldown.PlanCredit = v
	}
	if v := os.Getenv("TW2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("TW2A_SOFT_RATE_MAX"); v != "" {
		c.Cooldown.SoftRateMax = v
	}
	if v := os.Getenv("TW2A_CHECKIN_HOURS"); v != "" {
		if hours, err := parseHours(v); err == nil {
			c.Schedule.CheckinHours = hours
		}
	}
	if v := os.Getenv("TW2A_KEEPALIVE_HOURS"); v != "" {
		if hours, err := parseHours(v); err == nil {
			c.Schedule.KeepaliveHours = hours
		}
	}
	if v := os.Getenv("TW2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("TW2A_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("TW2A_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}
	if v := os.Getenv("TW2A_USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
	if v := os.Getenv("TW2A_CLIENT_VERSION"); v != "" {
		c.Upstream.ClientVersion = v
	}
	if v := os.Getenv("TW2A_MAX_IN_FLIGHT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Pool.MaxInFlight = n
		}
	}
	if v := os.Getenv("TW2A_EXPIRING_SOON"); v != "" {
		c.Pool.ExpiringSoon = v
	}
	if v := os.Getenv("TW2A_PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := os.Getenv("TW2A_PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
}

// parseHours 解析 env 里的时点列表（"9,21" / "9 21" / "[9 21]" 都收）。
func parseHours(v string) ([]int, error) {
	fields := strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == ' ' || r == '[' || r == ']'
	})
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// normalize 补齐缺省、解析时长、校验范围。非法值一律 fail fast（启动/保存即报错），
// 只有"未设置"才回落默认，避免静默改变行为。
func (c *Config) normalize() error {
	var err error
	if c.PlanCreditDur, err = time.ParseDuration(c.Cooldown.PlanCredit); err != nil {
		return fmt.Errorf("cooldown.plan_credit: %w", err)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.SoftRateMaxDur, err = time.ParseDuration(c.Cooldown.SoftRateMax); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max: %w", err)
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.DegradeCooldownDur, err = time.ParseDuration(c.Pool.DegradeCooldown); err != nil {
		return fmt.Errorf("pool.degrade_cooldown: %w", err)
	}
	if c.DegradeCooldownMaxD, err = time.ParseDuration(c.Pool.DegradeCooldownMax); err != nil {
		return fmt.Errorf("pool.degrade_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	if c.Pool.ExpiringSoon != "" {
		if c.ExpiringSoonDur, err = time.ParseDuration(c.Pool.ExpiringSoon); err != nil {
			return fmt.Errorf("pool.expiring_soon: %w", err)
		}
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	if c.Pool.DegradeThreshold <= 0 {
		c.Pool.DegradeThreshold = 5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5
	}
	if c.Pool.MaxInFlight < 0 {
		c.Pool.MaxInFlight = 0
	}
	// 显式 0/负数直接报错（面板填 0 就该得到"必须 > 0"，而不是被静默改成 120）；
	// 键缺席时 Default() 已给 120，所以老配置不会因此启动失败。
	if c.Upstream.TimeoutSeconds <= 0 {
		return fmt.Errorf("upstream.timeout_seconds must be > 0")
	}
	// 首字节缺省回落短超时（保"首字节前换号"语义）；流内空闲缺省走内置大值。
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	if c.DefaultModel == "" {
		c.DefaultModel = "glm-5.2"
	}
	if c.Listen == "" {
		c.Listen = ":7864"
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// 空数组/null 会覆盖掉 Default() 的时点（键缺席才保留），在此补齐：
	// 空 = 未配置 → 回落默认；「禁用」一律走 *_enabled=false。
	if len(c.Schedule.CheckinHours) == 0 {
		c.Schedule.CheckinHours = []int{9}
	}
	if len(c.Schedule.KeepaliveHours) == 0 {
		c.Schedule.KeepaliveHours = []int{3}
	}
	if c.Schedule.BalanceRefreshEnabled {
		if c.Schedule.BalanceRefreshMinutes <= 0 {
			c.Schedule.BalanceRefreshMinutes = 5
		}
		c.BalanceRefreshInt = time.Duration(c.Schedule.BalanceRefreshMinutes) * time.Minute
	} else {
		c.BalanceRefreshInt = 0
	}
	if err := c.validateScheduleHours(); err != nil {
		return err
	}
	return c.normalizePrompt()
}

// normalizePrompt 校验 prompt.mode 并按 file 加载提示词（custom/append 用）。
// mode 非法 → 报错（不静默回落某一分支）；custom/append 下 file 非空但不可读 → 报错。
func (c *Config) normalizePrompt() error {
	switch m := strings.ToLower(strings.TrimSpace(c.Prompt.Mode)); m {
	case "", "passthrough":
		c.Prompt.Mode = "passthrough"
	case "custom":
		c.Prompt.Mode = "custom"
	case "append":
		c.Prompt.Mode = "append"
	default:
		return fmt.Errorf("prompt.mode: %q 不是合法值（passthrough / custom / append）", c.Prompt.Mode)
	}
	c.PromptText = ""
	if c.Prompt.Mode == "custom" || c.Prompt.Mode == "append" {
		text, err := prompt.Load(c.Prompt.Mode, c.Prompt.File)
		if err != nil {
			return err
		}
		c.PromptText = text
	}
	return nil
}

// validateScheduleHours 校验排程时点落在 0-23，错误信息指向正确的开关。
func (c *Config) validateScheduleHours() error {
	if err := checkHourRange("schedule.checkin_hours", "checkin_enabled", c.Schedule.CheckinHours); err != nil {
		return err
	}
	return checkHourRange("schedule.keepalive_hours", "keepalive_enabled", c.Schedule.KeepaliveHours)
}

func checkHourRange(field, switchKey string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("%s: %d 不是合法小时（0-23）；如要关闭该任务请设 schedule.%s=false", field, h, switchKey)
		}
	}
	return nil
}

// Clone 复制配置（含切片），供面板读写，避免改到进程内原件。
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	cp := *c
	cp.Schedule.CheckinHours = append([]int(nil), c.Schedule.CheckinHours...)
	cp.Schedule.KeepaliveHours = append([]int(nil), c.Schedule.KeepaliveHours...)
	return &cp
}

// ParseBody 把面板提交的 JSON 合并进当前配置（含 api_key：面板可以改密钥，改完立即生效）。
func ParseBody(base *Config, raw []byte) (*Config, error) {
	if base == nil {
		base = Default()
	}
	c := base.Clone()
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// RestartFields 列出改了也不会热生效的字段。
//
// 排程整段都在这里：签到时点/开关/周期刷新是 scheduler 启动时读的（没有运行时 setter），
// 面板如实提示"改动需重启"，而不是假装立即生效。会话 GC 周期同理。
func RestartFields(bound, next *Config) []string {
	out := []string{}
	if bound == nil || next == nil {
		return out
	}
	if next.Listen != bound.Listen {
		out = append(out, "listen")
	}
	if next.AuthDir != bound.AuthDir {
		out = append(out, "auth_dir")
	}
	if next.StateFile != bound.StateFile {
		out = append(out, "state_file")
	}
	if !slices.Equal(next.Schedule.CheckinHours, bound.Schedule.CheckinHours) {
		out = append(out, "schedule.checkin_hours")
	}
	if !slices.Equal(next.Schedule.KeepaliveHours, bound.Schedule.KeepaliveHours) {
		out = append(out, "schedule.keepalive_hours")
	}
	if next.Schedule.CheckinEnabled != bound.Schedule.CheckinEnabled {
		out = append(out, "schedule.checkin_enabled")
	}
	if next.Schedule.KeepaliveEnabled != bound.Schedule.KeepaliveEnabled {
		out = append(out, "schedule.keepalive_enabled")
	}
	if next.Schedule.BalanceRefreshEnabled != bound.Schedule.BalanceRefreshEnabled ||
		next.Schedule.BalanceRefreshMinutes != bound.Schedule.BalanceRefreshMinutes {
		out = append(out, "schedule.balance_refresh")
	}
	if next.SessionSticky.GCInterval != bound.SessionSticky.GCInterval {
		out = append(out, "session_sticky.gc_interval")
	}
	return out
}

// SaveFile 原子写入配置。api_key 就写面板/文件当前的值——单一来源，不搞第二份。
func SaveFile(path string, c *Config) error {
	out := *c
	raw, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
