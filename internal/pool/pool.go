// Package pool 账号池：内存索引 + 冷却/禁用/在途状态机 + state.json 持久化。
//
// 挑选策略（PickExcluding）：在 healthy 且未达在途上限的账号里按权重取最大者——
//   - 权重基数 = 剩余积分（优先花掉大额余额，SPEC §4.7）；
//   - 闲置补偿：每闲置 1 小时给 idle_weight_per_hour% 的加成，封顶 idle_weight_max%；
//   - 快过期优先：若存在"到期时间在 expiring_soon 窗口内"的账号，只在它们之间选。
//
// 两个入口，差别只在"是否占用在途名额"：PickExcluding 占（对话路径，调用方负责 Release），
// PeekExcluding 不占（只读探询）。
//
// 失败分两类，冷却策略不同（都不叠加：取更长者）：
//   - 罚号失败（429/5xx/传输层）→ 熔断：连败达 breaker_threshold 出池，反复触发指数退避到 max；
//   - 不罚号失败（4xx 参数/404 等）→ 降权：连败达 degrade_threshold 临时出池，时长固定。
package pool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"traework2api/internal/auth"
)

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolPlan CoolKind = iota // 1005 plan 权益不足 → 12h 长冷却
	CoolSoft                 // 429/404 → 60s 短冷却（404 不累计 errCount）
	CoolErr                  // 连续错误 → 10m 中冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolPlan:
		return "plan_limit"
	case CoolSoft:
		return "soft_rate"
	case CoolErr:
		return "error_threshold"
	}
	return "unknown"
}

// Status 单个账号对外暴露的状态（脱敏，不含 token）。
type Status struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Credits  int64  `json:"credits"`
	// CreditsTotal 上游给的积分总额（各包 credits_limit 之和），只作分母：
	// 「积分」列显示 剩余/总额。0 表示还没查到过，界面只显示剩余。
	CreditsTotal int64     `json:"credits_total,omitempty"`
	Expire       int64     `json:"expire,omitempty"` // 最早到期时间（Unix 秒），0 表示无到期信息
	Cooling      bool      `json:"cooling"`
	Until        time.Time `json:"until,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	Disabled     bool      `json:"disabled"`
	ErrCount     int       `json:"err_count,omitempty"`     // 熔断连败计数
	Degraded     int       `json:"degrade_count,omitempty"` // 降权连败计数
	InFlight     int       `json:"in_flight,omitempty"`     // 当前在途请求数

	// 签到域冷却：只挡自动签到轮次，不参与 healthy()/选号。
	// 签到接口拥塞（9074）不代表对话额度不可用，两者不能混。
	CheckinCooling bool      `json:"checkin_cooling,omitempty"`
	CheckinUntil   time.Time `json:"checkin_until,omitempty"`
	CheckinReason  string    `json:"checkin_reason,omitempty"`
}

type entry struct {
	a       *auth.Auth
	credits int64
	// creditsTotal 是上游总额（分母），只由余额查询路径写；见 SetCredits。
	creditsTotal int64
	expire       int64
	// 签到域冷却，与网关冷却（until/reason）分开：前者只挡签到轮次。
	coolUntil  time.Time
	coolReason string
	disabled   bool
	reason     string
	until      time.Time
	errCount   int // 熔断计数（连败达阈出池）
	// degradeCount 降权计数（不罚号失败）；breakerStreak 熔断已触发次数，用于指数退避。
	degradeCount  int
	breakerStreak int
	softStreak    int // 软限流退避档位（429/404）
	// sessionDeadFails 连续 session 失效计数（持久化：重启不该给故障号免费重试）。
	// 上游偶发 401 不代表号真死，连续 sessionDeadThreshold 次才禁用，见 NoteSessionDead。
	sessionDeadFails int
	// 在途请求数（不持久化：重启即在途归零，没有需要恢复的在途请求）。
	inFlight int
	// lastUsed 上次被选中时间，用于闲置补偿（不持久化：重启后视为全部闲置）。
	lastUsed time.Time
}

// Limits 池的运行期参数（面板热改，见 ApplyLimits）。
type Limits struct {
	MaxInFlight        int
	BreakerThreshold   int
	BreakerCooldown    time.Duration
	BreakerCooldownMax time.Duration
	DegradeThreshold   int
	DegradeCooldown    time.Duration
	DegradeCooldownMax time.Duration
	IdleWeightPerHour  float64
	IdleWeightMax      float64
	ExpiringSoon       time.Duration
	PlanCooldown       time.Duration // 1005 硬冷却
	SoftCooldown       time.Duration // 429/404 软冷却基数
	SoftCooldownMax    time.Duration // 软冷却指数退避封顶
}

// DefaultLimits 未配置时的兜底（零值 Limits 也能安全工作）。
func DefaultLimits() Limits {
	return Limits{
		MaxInFlight:        3,
		BreakerThreshold:   3,
		BreakerCooldown:    10 * time.Minute,
		BreakerCooldownMax: 2 * time.Hour,
		DegradeThreshold:   5,
		DegradeCooldown:    10 * time.Minute,
		DegradeCooldownMax: 2 * time.Hour,
		IdleWeightPerHour:  0.5,
		IdleWeightMax:      5,
		ExpiringSoon:       168 * time.Hour,
		PlanCooldown:       12 * time.Hour,
		SoftCooldown:       60 * time.Second,
		SoftCooldownMax:    2 * time.Hour,
	}
}

func (e *entry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	return true
}

// stateFile 持久化格式。
type stateFile struct {
	Accounts map[string]struct {
		Credits      int64     `json:"credits"`
		CreditsTotal int64     `json:"credits_total,omitempty"`
		Expire       int64     `json:"expire,omitempty"`
		Disabled     bool      `json:"disabled"`
		Reason       string    `json:"reason,omitempty"`
		Until        time.Time `json:"until,omitempty"`
		CoolUntil    time.Time `json:"checkin_until,omitempty"`
		CoolReason   string    `json:"checkin_reason,omitempty"`
		// 连败/退避档位要持久化：重启丢掉退避档位等于每次重启都给故障号一次免费重试。
		Breaker  int `json:"breaker_streak,omitempty"`
		Soft     int `json:"soft_streak,omitempty"`
		Degrade  int `json:"degrade_count,omitempty"`
		SessDead int `json:"session_dead_fails,omitempty"`
	} `json:"accounts"`
}

// Pool 账号池。
type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
	lim     Limits
}

// New 构建池；stateFp 非空时尝试加载旧状态。
func New(stateFp string) *Pool {
	p := &Pool{byUID: map[string]*entry{}, stateFp: stateFp, lim: DefaultLimits()}
	if stateFp != "" {
		p.load()
	}
	return p
}

// ApplyLimits 热改池参数（面板保存配置时调用）。零值字段保持原值，
// 只有显式配置才覆盖，避免"未填"被读成"关掉"。
func (p *Pool) ApplyLimits(l Limits) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.lim
	if l.MaxInFlight >= 0 {
		cur.MaxInFlight = l.MaxInFlight
	}
	if l.BreakerThreshold > 0 {
		cur.BreakerThreshold = l.BreakerThreshold
	}
	if l.BreakerCooldown > 0 {
		cur.BreakerCooldown = l.BreakerCooldown
	}
	if l.BreakerCooldownMax > 0 {
		cur.BreakerCooldownMax = l.BreakerCooldownMax
	}
	if l.DegradeThreshold > 0 {
		cur.DegradeThreshold = l.DegradeThreshold
	}
	if l.DegradeCooldown > 0 {
		cur.DegradeCooldown = l.DegradeCooldown
	}
	if l.DegradeCooldownMax > 0 {
		cur.DegradeCooldownMax = l.DegradeCooldownMax
	}
	if l.IdleWeightPerHour > 0 {
		cur.IdleWeightPerHour = l.IdleWeightPerHour
	}
	if l.IdleWeightMax > 0 {
		cur.IdleWeightMax = l.IdleWeightMax
	}
	if l.ExpiringSoon > 0 {
		cur.ExpiringSoon = l.ExpiringSoon
	}
	if l.PlanCooldown > 0 {
		cur.PlanCooldown = l.PlanCooldown
	}
	if l.SoftCooldown > 0 {
		cur.SoftCooldown = l.SoftCooldown
	}
	if l.SoftCooldownMax > 0 {
		cur.SoftCooldownMax = l.SoftCooldownMax
	}
	p.lim = cur
}

// Limits 当前池参数快照（面板/日志用）。
func (p *Pool) Limits() Limits {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lim
}

// Add 加入账号；已存在则保留原状态、更新凭证。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range auths {
		seen[a.UID] = true
		if e, ok := p.byUID[a.UID]; ok {
			e.a = a
		} else {
			p.byUID[a.UID] = &entry{a: a}
		}
	}
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
		}
	}
}

// Pick 返回 healthy 中积分最高的账号；无可用返回 nil。
func (p *Pool) Pick() *auth.Auth {
	return p.PickExcluding(nil)
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
//
// **会占用在途名额**：这是对话路径的入口，选中即占（调用方必须 Release，否则名额泄漏
// 会把账号慢慢挤出池）。只读探询（取模型列表、状态展示）请用 PeekExcluding。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.pickLocked(tried)
	if e == nil {
		return nil
	}
	e.inFlight++
	e.lastUsed = time.Now()
	return e.a
}

// Peek / PeekExcluding 与 Pick 同策略，但**不占在途名额**：给只读探询用。
// 探询要是也占名额，几次取模型列表就能把账号占到不可用。
func (p *Pool) Peek() *auth.Auth { return p.PeekExcluding(nil) }

func (p *Pool) PeekExcluding(tried map[string]bool) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e := p.pickLocked(tried); e != nil {
		return e.a
	}
	return nil
}

// pickLocked 选号策略本体（调用方持锁）：healthy + 未达在途上限 → 快过期优先 → 权重最大。
func (p *Pool) pickLocked(tried map[string]bool) *entry {
	now := time.Now()
	var cands []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if p.lim.MaxInFlight > 0 && e.inFlight >= p.lim.MaxInFlight {
			continue
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		return nil
	}

	// 快过期优先：窗口内到期的积分先花掉，避免过期作废。credits 为 0 的号没有可花的
	// 积分，不算候选（否则会把空号排到前面）。
	if p.lim.ExpiringSoon > 0 {
		var expiring []*entry
		for _, e := range cands {
			if e.credits > 0 && e.expire > 0 && time.Unix(e.expire, 0).Sub(now) <= p.lim.ExpiringSoon {
				expiring = append(expiring, e)
			}
		}
		if len(expiring) > 0 {
			cands = expiring
		}
	}

	best := cands[0]
	bestScore := p.score(best, now)
	for _, e := range cands[1:] {
		if s := p.score(e, now); s > bestScore {
			best, bestScore = e, s
		}
	}
	return best
}

// score 选号权重：积分基数 + 闲置加成（百分比，按积分比例算，避免被积分量级淹没）。
//
// 闲置加成 = 积分 × min(闲置小时数 × idle_weight_per_hour, idle_weight_max) / 100。
// 用比例而不是绝对分：trae 的积分是几百到几千的量级，绝对分值（wb 的 0.5 分）在这里
// 小到看不出效果，等于这个开关没用。
func (p *Pool) score(e *entry, now time.Time) float64 {
	base := float64(e.credits)
	if p.lim.IdleWeightPerHour <= 0 || e.lastUsed.IsZero() {
		// 从未用过（含刚重启）视为全新闲置，但没历史就不加分——否则重启会把选号顺序打乱。
		return base
	}
	hours := now.Sub(e.lastUsed).Hours()
	if hours <= 0 {
		return base
	}
	bonusPct := hours * p.lim.IdleWeightPerHour
	if bonusPct > p.lim.IdleWeightMax {
		bonusPct = p.lim.IdleWeightMax
	}
	return base * (1 + bonusPct/100)
}

// Acquire 显式占用在途名额（不经 PickExcluding 的场景，如手工指定账号）；
// 超出上限返回 false。
func (p *Pool) Acquire(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	if p.lim.MaxInFlight > 0 && e.inFlight >= p.lim.MaxInFlight {
		return false
	}
	e.inFlight++
	return true
}

// AcquireIfHealthy 指定账号可用（healthy 且未达在途上限）时占用一个在途名额并返回凭证；
// 否则返回 false（调用方换号）。会话粘性用它把"粘住的号"钉住。
func (p *Pool) AcquireIfHealthy(uid string) (*auth.Auth, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	e, ok := p.byUID[uid]
	if !ok || !e.healthy(now) {
		return nil, false
	}
	if p.lim.MaxInFlight > 0 && e.inFlight >= p.lim.MaxInFlight {
		return nil, false
	}
	e.inFlight++
	e.lastUsed = now
	return e.a, true
}

// Release 释放一次在途名额（幂等：不会减到负数）。
func (p *Pool) Release(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && e.inFlight > 0 {
		e.inFlight--
	}
}

// SetCredits 更新账号积分（不动总额：分母只由余额查询路径 SetCreditsExpire 写）。
func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
	}
	p.saveLocked()
}

// SetCreditsExpire 同时更新积分、积分总额与最早到期时间（签到/刷新余额后调用）。
func (p *Pool) SetCreditsExpire(uid string, credits, total, expire int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
		e.creditsTotal = total
		e.expire = expire
	}
	p.saveLocked()
}

// Cooldown 冷却账号至 now+d（通用入口：计划冷却/签到域之外的显式冷却走它）。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.reason = reason
		e.errCount = 0
	}
	p.saveLocked()
}

// Disable 永久禁用（session 失效），需人工重登后手工恢复或文件替换。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
	}
	p.saveLocked()
}

// sessionDeadThreshold 连续 session 失效（ErrSessionDead）达到该次数才永久禁用。
// 一次 401 多是上游抖动或瞬时鉴权失败，见到就杀号会让可用账号凭空下线（WorkBuddy
// 那边一次性禁用号里全是这类误判）。连续失败才判定号真死；期间任何一次刷新/调用成功
// 都会 ClearSessionDead 清零，误判的号自己回来。
const sessionDeadThreshold = 3

// SessionDeadThreshold 暴露连续失效的禁用阈值（日志与文档引用）。
func SessionDeadThreshold() int { return sessionDeadThreshold }

// NoteSessionDead 记一次 session 失效；连续达阈值才禁用。
// 返回 true 表示本次触发禁用（调用方据此打日志）。
func (p *Pool) NoteSessionDead(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.sessionDeadFails++
	if e.sessionDeadFails < sessionDeadThreshold {
		p.saveLocked()
		return false
	}
	e.disabled = true
	e.reason = "session dead"
	p.saveLocked()
	return true
}

// ClearSessionDead 清零连续失效计数（刷新/调用成功即调用）。
// 计数本来为 0 时不写状态文件，避免热路径每请求落盘。
func (p *Pool) ClearSessionDead(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || e.sessionDeadFails == 0 {
		return
	}
	e.sessionDeadFails = 0
	p.saveLocked()
}

// Enable 重新启用被禁用的账号：清禁用标记、连续失效计数与冷却，让它下一轮就能被选中。
// 没有它，被误判禁用的号只能「移除 + 重登」（Add 对已知 uid 保留旧状态，不会自己解禁）。
func (p *Pool) Enable(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.disabled = false
	e.reason = ""
	e.until = time.Time{}
	e.errCount = 0
	e.degradeCount = 0
	e.breakerStreak = 0
	e.softStreak = 0
	e.sessionDeadFails = 0
	p.saveLocked()
	return true
}

// ClearCooldown 解除冷却。禁用账号保持原状。
func (p *Pool) ClearCooldown(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || e.disabled {
		return false
	}
	e.until = time.Time{}
	e.reason = ""
	e.errCount = 0
	e.degradeCount = 0
	e.breakerStreak = 0
	e.softStreak = 0
	p.saveLocked()
	return true
}

// Remove 从池中删除账号。文件由调用方删除。
func (p *Pool) Remove(uid string) (*auth.Auth, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil, false
	}
	delete(p.byUID, uid)
	p.saveLocked()
	return e.a, true
}

// CooldownCheckin 签到失败后记一次签到域冷却，避免下一轮对拥塞的上游反复重试。
//
// 与 Cooldown（网关冷却）分开：签到接口失败不代表对话额度不可用，
// 混在一起会把账号从代理选号里踢掉。已在冷却中的不覆盖，避免更短的冷却缩短它。
func (p *Pool) CooldownCheckin(uid string, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || e.disabled {
		return
	}
	if !e.coolUntil.IsZero() && time.Now().Before(e.coolUntil) {
		return
	}
	e.coolUntil = time.Now().Add(d)
	e.coolReason = reason
	p.saveLocked()
}

// ClearCheckinCooldown 签到成功后清掉签到域冷却。
func (p *Pool) ClearCheckinCooldown(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.coolUntil = time.Time{}
		e.coolReason = ""
	}
	p.saveLocked()
}

// CheckinCooling 报告账号是否处于签到域冷却中（自动签到轮次据此跳过）。
func (p *Pool) CheckinCooling(uid string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	return ok && !e.coolUntil.IsZero() && time.Now().Before(e.coolUntil)
}

// ReenableIfCredits 签到后解冻：仅当 remain > 0 且账号处于冷却（非禁用）时恢复。
func (p *Pool) ReenableIfCredits(uid string, remain int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = remain
		if remain > 0 && !e.disabled {
			e.until = time.Time{}
			e.reason = ""
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// NoteError 记一次"罚号失败"（429/5xx/传输层）：连败达 breaker_threshold 触发熔断，
// 出池时长按触发次数指数退避（base × 2^(n-1)），封顶 breaker_cooldown_max。
//
// 为什么退避：反复撞同一个号的上游故障会让它刚出池就再被打穿，倍增加长才有静默期。
func (p *Pool) NoteError(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	e.errCount++
	if e.errCount < p.lim.BreakerThreshold {
		p.saveLocked()
		return
	}
	e.errCount = 0
	e.breakerStreak++
	d := p.lim.BreakerCooldown << (e.breakerStreak - 1)
	if d <= 0 || d > p.lim.BreakerCooldownMax {
		d = p.lim.BreakerCooldownMax
	}
	e.until = time.Now().Add(d)
	e.reason = fmt.Sprintf("breaker x%d", e.breakerStreak)
	p.saveLocked()
}

// NoteDegrade 记一次"不罚号失败"（4xx 参数/超时以外的客户端侧问题）：连败达
// degrade_threshold 临时出池，时长固定（不指数退避、也不叠加），超 degrade_cooldown_max 才钳制。
func (p *Pool) NoteDegrade(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	e.degradeCount++
	if e.degradeCount < p.lim.DegradeThreshold {
		p.saveLocked()
		return
	}
	e.degradeCount = 0
	d := p.lim.DegradeCooldown
	if d > p.lim.DegradeCooldownMax {
		d = p.lim.DegradeCooldownMax
	}
	e.until = time.Now().Add(d)
	e.reason = "degraded"
	p.saveLocked()
}

// CooldownSoft 软限流冷却（429/404）：基数软冷却 × 2^(n-1)，封顶 soft_cooldown_max。
// 连续被限流的号需要比单次更长的静默期，否则只是把 429 摊薄。
func (p *Pool) CooldownSoft(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	e.softStreak++
	d := p.lim.SoftCooldown << (e.softStreak - 1)
	if d <= 0 || d > p.lim.SoftCooldownMax {
		d = p.lim.SoftCooldownMax
	}
	e.until = time.Now().Add(d)
	e.reason = reason
	e.errCount = 0
	p.saveLocked()
}

// CooldownPlan 1005 权益不足的硬冷却（plan_credit，固定时长不叠加）。
func (p *Pool) CooldownPlan(uid string) {
	p.Cooldown(uid, CoolPlan, p.lim.PlanCooldown, "plan 权益不足")
}

// NoteSuccess 成功请求重置连败计数与退避档位（成功即认为账号恢复正常）。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount = 0
		e.degradeCount = 0
		e.breakerStreak = 0
		e.softStreak = 0
	}
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}

func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	return Status{
		UID:          uid,
		Nickname:     e.a.Nickname,
		Credits:      e.credits,
		CreditsTotal: e.creditsTotal,
		Expire:       e.expire,
		Cooling:      !e.until.IsZero() && now.Before(e.until),
		Until:        e.until,
		Reason:       e.reason,
		Disabled:     e.disabled,
		ErrCount:     e.errCount,
		Degraded:     e.degradeCount,
		InFlight:     e.inFlight,

		CheckinCooling: !e.coolUntil.IsZero() && now.Before(e.coolUntil),
		CheckinUntil:   e.coolUntil,
		CheckinReason:  e.coolReason,
	}
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	for uid, s := range sf.Accounts {
		p.byUID[uid] = &entry{
			a:                &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:          s.Credits,
			creditsTotal:     s.CreditsTotal,
			expire:           s.Expire,
			disabled:         s.Disabled,
			reason:           s.Reason,
			until:            s.Until,
			coolUntil:        s.CoolUntil,
			coolReason:       s.CoolReason,
			breakerStreak:    s.Breaker,
			softStreak:       s.Soft,
			degradeCount:     s.Degrade,
			sessionDeadFails: s.SessDead,
		}
	}
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{Accounts: map[string]struct {
		Credits      int64     `json:"credits"`
		CreditsTotal int64     `json:"credits_total,omitempty"`
		Expire       int64     `json:"expire,omitempty"`
		Disabled     bool      `json:"disabled"`
		Reason       string    `json:"reason,omitempty"`
		Until        time.Time `json:"until,omitempty"`
		CoolUntil    time.Time `json:"checkin_until,omitempty"`
		CoolReason   string    `json:"checkin_reason,omitempty"`
		Breaker      int       `json:"breaker_streak,omitempty"`
		Soft         int       `json:"soft_streak,omitempty"`
		Degrade      int       `json:"degrade_count,omitempty"`
		SessDead     int       `json:"session_dead_fails,omitempty"`
	}{}}
	for uid, e := range p.byUID {
		sf.Accounts[uid] = struct {
			Credits      int64     `json:"credits"`
			CreditsTotal int64     `json:"credits_total,omitempty"`
			Expire       int64     `json:"expire,omitempty"`
			Disabled     bool      `json:"disabled"`
			Reason       string    `json:"reason,omitempty"`
			Until        time.Time `json:"until,omitempty"`
			CoolUntil    time.Time `json:"checkin_until,omitempty"`
			CoolReason   string    `json:"checkin_reason,omitempty"`
			Breaker      int       `json:"breaker_streak,omitempty"`
			Soft         int       `json:"soft_streak,omitempty"`
			Degrade      int       `json:"degrade_count,omitempty"`
			SessDead     int       `json:"session_dead_fails,omitempty"`
		}{
			Credits:      e.credits,
			CreditsTotal: e.creditsTotal,
			Expire:       e.expire,
			Disabled:     e.disabled,
			Reason:       e.reason,
			Until:        e.until,
			CoolUntil:    e.coolUntil,
			CoolReason:   e.coolReason,
			Breaker:      e.breakerStreak,
			Soft:         e.softStreak,
			Degrade:      e.degradeCount,
			SessDead:     e.sessionDeadFails,
		}
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.stateFp)
}
