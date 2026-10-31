// Package pool 账号池：内存索引 + 冷却/禁用/在途状态机 + state.json 持久化。
//
// 挑选策略（PickExcluding）：在 healthy 且未达在途上限的账号里**加权随机抽签**——
//   - 权重 = 1.0 基线 + 积分/最高分×10 + 闲置加成（后者封顶 idle_weight_max）；
//   - 快过期优先：若存在"到期时间在 expiring_soon 窗口内"的账号，只在它们之间选；
//   - 防撞号：minPickGap 内刚被选中的号不进抽签池，全被选过则退到 LRU（usedSeq 最小）。
//
// 抽签而不是取最大：取最大是确定性的，积分最高的号会独占全部流量（见 drawLocked 注释）。
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
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/safefile"
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
	// Realm 账号地区（cn / intl）：国际版账号没有签到接口、只能做国际版模型。
	Realm   string `json:"realm,omitempty"`
	Credits int64  `json:"credits"`
	// CreditsTotal 上游给的积分总额（各包 credits_limit 之和），只作分母：
	// 「积分」列显示 剩余/总额。0 表示还没查到过，界面只显示剩余。
	CreditsTotal int64 `json:"credits_total,omitempty"`
	Expire       int64 `json:"expire,omitempty"` // 最早到期时间（Unix 秒），0 表示无到期信息
	// ExpireCredits 在 Expire 那天真正会作废的积分（不是账号总余额）。多包账号两者
	// 差很多：总余额 4334 分里只有 126 分当天到期时，面板若拿总余额当「该日到期」，
	// 会误导成「今天必须烧完 4334 分」。0 = 无数据，前端回退到 credits。
	ExpireCredits int64     `json:"expire_credits,omitempty"`
	Cooling       bool      `json:"cooling"`
	Until         time.Time `json:"until,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	Disabled      bool      `json:"disabled"`
	// Paused 暂停选号：不参与 Pick，签到等运维照常。面板「暂停选号」按钮写它。
	Paused   bool `json:"paused,omitempty"`
	ErrCount int  `json:"err_count,omitempty"`     // 熔断连败计数
	Degraded int  `json:"degrade_count,omitempty"` // 降权连败计数
	InFlight int  `json:"in_flight,omitempty"`     // 当前在途请求数

	// 额度口径：上游两份计费体系字段不同名也不可换算（见 upstream.entUsagePack）。
	//   CreditUnit="credit"（国内，默认）：看 Credits/CreditsTotal
	//   CreditUnit="usd"（国际按量计费）：看 UsedUSD/LimitUSD —— Credits 恒 0
	// 不填 CreditUnit 时前端按 credit 处理，老数据零回归。
	CreditUnit string  `json:"credit_unit,omitempty"`
	UsedUSD    float64 `json:"used_usd,omitempty"`
	LimitUSD   float64 `json:"limit_usd,omitempty"`

	// 签到域冷却：只挡自动签到轮次，不参与 healthy()/选号。
	// 签到接口拥塞（9074）不代表对话额度不可用，两者不能混。
	CheckinCooling bool      `json:"checkin_cooling,omitempty"`
	CheckinUntil   time.Time `json:"checkin_until,omitempty"`
	CheckinReason  string    `json:"checkin_reason,omitempty"`
}

type entry struct {
	a       *auth.Auth
	credits int64
	// expireCredits 在 expire 那天真正作废的积分（与 expire 同源维护）。
	expireCredits int64
	// creditsTotal 是上游总额（分母），只由余额查询路径写；见 SetCredits。
	creditsTotal int64
	expire       int64
	// 额度口径（见 Status.CreditUnit）：unit 为 upstream.UnitUSD 时用美元那两个字段，
	// credits 恒 0。三个字段同属「余额快照」，只由 SetBalance 一起写。
	unit     string
	usedUSD  float64
	limitUSD float64
	// 签到域冷却，与网关冷却（until/reason）分开：前者只挡签到轮次。
	coolUntil  time.Time
	coolReason string
	disabled   bool
	// paused 暂停选号（人工）：不参与 Pick，但签到/保活/余额刷新/领权益照常。
	// 与 disabled（全下线，调度器也不碰）是两个语义层级，勿合并。
	paused   bool
	reason   string
	until    time.Time
	errCount int // 熔断计数（连败达阈出池）
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
	// usedSeq 上次被选中时的池内单调序号（不持久化）。LRU 兜底用它而非墙钟：
	// 同毫秒内连续选号时 lastUsed 完全相同，墙钟比较退化成恒取第一个候选。
	usedSeq uint64
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
	// paused 也挡选号：它与冷却/禁用同属「不该被 Pick」的状态，
	// 差异只在调度器是否还跑（调度器只看 disabled，不看 paused）。
	if e.disabled || e.paused {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	return true
}

// stateAccount 单账号的持久化快照。
//
// 以前这段是**三处**抄写的匿名 struct（stateFile 里一次、saveLocked 里两次），
// 加字段必然漏一处；收成具名类型后只有这一份定义。
type stateAccount struct {
	Credits       int64 `json:"credits"`
	CreditsTotal  int64 `json:"credits_total,omitempty"`
	Expire        int64 `json:"expire,omitempty"`
	ExpireCredits int64 `json:"expire_credits,omitempty"`
	Disabled      bool  `json:"disabled"`
	// Paused 持久化：重启不该把人工暂停的号放回选号池。
	Paused     bool      `json:"paused,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Until      time.Time `json:"until,omitempty"`
	CoolUntil  time.Time `json:"checkin_until,omitempty"`
	CoolReason string    `json:"checkin_reason,omitempty"`
	// 连败/退避档位要持久化：重启丢掉退避档位等于每次重启都给故障号一次免费重试。
	Breaker  int `json:"breaker_streak,omitempty"`
	Soft     int `json:"soft_streak,omitempty"`
	Degrade  int `json:"degrade_count,omitempty"`
	SessDead int `json:"session_dead_fails,omitempty"`
	// 额度口径与美元余额也要持久化：国际版额度是美元，重启后凭 0 积分什么都恢复不出来，
	// 面板会退回「无额度」。omitempty 让国内号（unit=credit）写出来的文件与以前一致。
	Unit     string  `json:"credit_unit,omitempty"`
	UsedUSD  float64 `json:"used_usd,omitempty"`
	LimitUSD float64 `json:"limit_usd,omitempty"`
}

// stateFile 持久化格式。
type stateFile struct {
	Accounts map[string]stateAccount `json:"accounts"`
}

// Pool 账号池。
type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
	lim     Limits
	// pickSeq 选中序号自增计数器（持锁内自增），为 entry.usedSeq 提供严格全序。
	pickSeq uint64
	// randInt64N 抽签随机源（nil = math/rand/v2）。见 SetRandInt64N。
	randInt64N func(int64) int64
}

// SetRandInt64N 注入抽签随机源：返回 0 即「权重最高者中签」（改造前的取最大语义），
// 供需要确定性选号结果的测试使用（跨包测试用得上）。传 nil 还原默认随机源。
// BumpDeviceSeed 给账号换一套 ug 族设备指纹（种子 +1）并落盘，返回新种子。
//
// 9074（设备维度限流）与 9095（设备已替别的账号签过）都是**设备维度**信号：换一套设备身份
// 就能脱开旧标记；不能靠秒级重试（上游会延长限流窗口）。面板「换指纹」与调度器的自动轮换
// 都走这一处，避免两边各写一遍落盘逻辑。
func (p *Pool) BumpDeviceSeed(uid string) (seed int64, ok bool, err error) {
	a := p.AuthByUID(uid)
	if a == nil {
		return 0, false, nil
	}
	a.SetDeviceSeed(a.DeviceSeedValue() + 1)
	if err := a.SaveAtomic(); err != nil {
		return a.DeviceSeedValue(), true, err
	}
	return a.DeviceSeedValue(), true, nil
}

func (p *Pool) SetRandInt64N(f func(int64) int64) {
	p.mu.Lock()
	p.randInt64N = f
	p.mu.Unlock()
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
	// ExpiringSoon 用 >= 0：配置层把空串解析成 0，注释写明「0 = 禁用分桶」，而 Default()
	// 保证「键缺席」是 168h——所以到这里 0 只可能来自显式禁用。跟着 > 0 走会让「清空该键」
	// 悄悄退回 168h（面板说保存成功、行为一点没变）。调用点只有 limitsOf（启动 + 面板保存）。
	if l.ExpiringSoon >= 0 {
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
	e := p.pickLocked(tried, "")
	if e == nil {
		return nil
	}
	e.inFlight++
	p.markPickedLocked(e)
	return e.a
}

// markPickedLocked 记一次选中（调用方持写锁）：时间戳 + 单调序号。
// 两条都要有：lastUsed 供 minPickGap 与闲置加成，usedSeq 供同毫秒下的 LRU 兜底排序。
func (p *Pool) markPickedLocked(e *entry) {
	e.lastUsed = time.Now()
	p.pickSeq++
	e.usedSeq = p.pickSeq
}

// Peek / PeekExcluding 与 Pick 同策略，但**不占在途名额**：给只读探询用。
// 探询要是也占名额，几次取模型列表就能把账号占到不可用。
func (p *Pool) Peek() *auth.Auth { return p.PeekExcluding(nil) }

// PickRealmExcluding 只在指定地区的账号里挑（占用在途名额，语义同 PickExcluding）。
// realm 空 = 不限，等同 PickExcluding。
func (p *Pool) PickRealmExcluding(realm string, tried map[string]bool) *auth.Auth {
	if realm == "" {
		return p.PickExcluding(tried)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.pickLocked(tried, realm)
	if e == nil {
		return nil
	}
	e.inFlight++
	p.markPickedLocked(e)
	return e.a
}

func (p *Pool) PeekExcluding(tried map[string]bool) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e := p.pickLocked(tried, ""); e != nil {
		return e.a
	}
	return nil
}

// pickLocked 选号策略本体（调用方持锁）：healthy + 未达在途上限 → 快过期优先 → 权重加权随机。
// pickLocked 选号。realm 非空时只在该地区的账号里挑：国际版模型国内号做不了、
// 国内版模型国际号做不了，混着挑等于随机把请求打到一个必然 4001 的号上。
func (p *Pool) pickLocked(tried map[string]bool, realm string) *entry {
	now := time.Now()
	var cands []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
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
	return p.drawLocked(cands, now)
}

// minPickGap 防并发撞号窗口：同一账号在该窗口内不重复被选中（除非候选全部刚被用过）。
// 没有它，同一瞬间涌入的并发请求会各自独立抽签、全部落到权重最高的那个号上——
// 面板上就表现为「只调用一个账号」。持锁串行进入使每个进入者都把 lastUsed 置为 now，
// 于是第 2..N 个进入者看到前面那个号距今 0 < minPickGap，被自然挤向其它号。
var minPickGap = 100 * time.Millisecond

// drawLocked 在候选集内加权随机抽签（调用方持锁）。
//
// 为什么不是「取权重最大」（原实现）：权重最大是确定性的，只要有一个号的积分最高，
// 其它号**永远**不会被选中（实测 4417/4600 两个号，4600 那个吃掉全部流量）；
// 国际版免费号 credits 恒 0，在乘性权重下权重恒 0，更是永久饿死。
// 加权随机把「积分高的号分到更多流量」变成概率而非独占。
func (p *Pool) drawLocked(cands []*entry, now time.Time) *entry {
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	type weighted struct {
		e *entry
		w float64
	}
	ws := make([]weighted, 0, len(cands))
	for _, e := range cands {
		ws = append(ws, weighted{e: e, w: p.weight(e, maxCredits, now)})
	}
	// 权重降序 + uid 兜底：抽签按前缀和取，顺序决定「r=0 时谁中签」，
	// 排序使注入定值随机源的测试得到确定的 argmax 语义（见 pickRandInt64N）。
	sort.SliceStable(ws, func(i, j int) bool {
		if ws[i].w != ws[j].w {
			return ws[i].w > ws[j].w
		}
		return ws[i].e.a.UID < ws[j].e.a.UID
	})

	// 防撞号：优先在「距上次选中超过 minPickGap」的号里抽。
	eligible := make([]weighted, 0, len(ws))
	for _, c := range ws {
		if now.Sub(c.e.lastUsed) >= minPickGap {
			eligible = append(eligible, c)
		}
	}
	// 全部刚被用过 → LRU 兜底：取 usedSeq 最小（最久未被选中）的那个。
	// 用单调序号而非墙钟比较：同毫秒内连续选号时 lastUsed 完全相同，墙钟比较会恒取 ws[0]。
	if len(eligible) == 0 {
		oldest := ws[0].e
		for _, c := range ws[1:] {
			if c.e.usedSeq < oldest.usedSeq {
				oldest = c.e
			}
		}
		return oldest
	}

	const scale = 1_000_000 // 定点放大，整数抽签
	weights := make([]int64, len(eligible))
	var total int64
	for i, c := range eligible {
		weights[i] = int64(c.w * scale)
		total += weights[i]
	}
	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	if total <= 0 {
		return eligible[rnd(int64(len(eligible)))].e
	}
	r := rnd(total)
	var acc int64
	for i, c := range eligible {
		acc += weights[i]
		if r < acc {
			return c.e
		}
	}
	return eligible[len(eligible)-1].e
}

// weight 选号权重（照 workbuddy2api-panel 的 weightOf 形状：**加性**，基线 1.0）。
//
//	1.0        基线——任何 healthy 号都有非零权重。乘性权重（旧实现 credits×(1+闲置%)）
//	           在 credits=0 时恒为 0：国际版免费号没有积分，会被永久饿死。
//	积分/最高分×10  按比例偏好积分多的号；用比例而非绝对值（积分是几百到几千的量级，
//	           绝对值会被量级淹没），除以最高分使积分项落在 [0,10]。
//	闲置加成    久未使用的号抬权，封顶 idle_weight_max；从未用过（含刚重启）给满分——
//	           随机抽签下不存在「重启打乱固定顺序」的问题。
//
// 权重只影响抽中概率，不影响正确性：真正没额度的号会在 1005 时被硬冷却踢出候选。
func (p *Pool) weight(e *entry, maxCredits int64, now time.Time) float64 {
	w := 1.0
	switch {
	case e.unit == "usd" && e.limitUSD > 0:
		// 美元号（国际版 Free plan 等）：按**剩余额度占比**给分。
		//
		// 为什么不能沿用积分那条「占池内最大积分的比例」：单位不可换算，把美元数丢进
		// 积分口径的比较里没有任何意义。而选号本来就在同 realm 内进行（见 pickLocked 的
		// realm 过滤），同组内单位必然一致，所以各自口径按自己的占比归一 = 等价且诚实。
		remain := e.limitUSD - e.usedUSD
		if remain < 0 {
			remain = 0
		}
		w += remain / e.limitUSD * 10
	case maxCredits > 0:
		w += float64(e.credits) / float64(maxCredits) * 10
	}
	if p.lim.IdleWeightPerHour <= 0 {
		return w
	}
	if e.lastUsed.IsZero() {
		return w + p.lim.IdleWeightMax
	}
	bonus := now.Sub(e.lastUsed).Hours() * p.lim.IdleWeightPerHour
	if bonus > p.lim.IdleWeightMax {
		bonus = p.lim.IdleWeightMax
	}
	if bonus < 0 {
		bonus = 0 // lastUsed 在未来（时钟回拨）时钳 0
	}
	return w + bonus
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
	p.markPickedLocked(e)
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
		// 老路径没有逐包信息，该日到期额未知 → 清 0，前端回退到总余额。
		e.expireCredits = 0
	}
	p.saveLocked()
}

// Balance 一个账号的额度快照（两种口径二选一，单位见 Unit）。
//
// 为什么不合成一个数：国内是积分、国际是美元，上游两套字段不同名（credits_limit vs
// basic_usage_limit）且**不可换算**。硬折算会造出一个两边都不准的数字。
type Balance struct {
	Unit   string // "credit"（默认）| "usd"
	Remain int64  // 积分口径
	Total  int64
	// 美元口径（Unit="usd"）。Free plan 实测 used=0.00059，必须留小数。
	UsedUSD  float64
	LimitUSD float64
	// ExpireCredits 与 expire 同日到期的积分（见 Status.ExpireCredits）。
	ExpireCredits int64
}

// SetBalance 写一个账号的额度快照（口径感知版，取代只有积分口径的 SetCreditsExpire）。
//
// unit 为空按 credit 处理：签到路径只算积分，走老逻辑不会把国际号写成 usd。
func (p *Pool) SetBalance(uid string, b Balance, expire int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	unit := b.Unit
	if unit == "" {
		unit = "credit"
	}
	e.unit, e.usedUSD, e.limitUSD = unit, b.UsedUSD, b.LimitUSD
	if unit == "usd" {
		// 美元号：credits 留 0（没有积分这回事），余额看 usedUSD/limitUSD。
		// 不把美元塞进 credits —— 那会让「积分」列显示一个不是积分的数。
		e.credits, e.creditsTotal = 0, 0
	} else {
		e.credits, e.creditsTotal = b.Remain, b.Total
		e.usedUSD, e.limitUSD = 0, 0
	}
	e.expire = expire
	e.expireCredits = b.ExpireCredits
	p.saveLocked()
}

// Cooldown 冷却账号至 now+d（通用入口：计划冷却/签到域之外的显式冷却走它）。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cooldownLocked(uid, d, reason)
}

// cooldownLocked 调用方须持 p.mu（时长可能取自 p.lim，必须在锁内求值）。
func (p *Pool) cooldownLocked(uid string, d time.Duration, reason string) {
	if e, ok := p.byUID[uid]; ok {
		e.setCooldownLocked(time.Now().Add(d), reason)
		e.errCount = 0
	}
	p.saveLocked()
}

// setCooldownLocked 网关冷却只延长、不缩短（模块头承诺的「不叠加：取更长者」，也是
// CooldownCheckin 的做法）。四种失败路径都走它：并发请求下 12h 的 plan 硬冷却曾被随后
// 一次 60s 软冷却改写成 60s，号提前回池被再选中 → 重复打 1005/429，面板原因也被改写。
// 提前解除冷却只有显式路径（ClearCooldown / ReenableIfCredits / 面板按钮）。
func (e *entry) setCooldownLocked(until time.Time, reason string) {
	if until.After(e.until) {
		e.until, e.reason = until, reason
	}
}

// Pause 暂停选号（人工操作）：不挡调度器（签到/保活/余额刷新），只挡 Pick。
// 与 Disable 的区别：Disable 是全下线（调度器也跳过），Pause 只是不上对话流量。
// 两个状态可叠加：disabled+paused 的号既不上流量也不签到。
func (p *Pool) Pause(uid, reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.paused = true
	if reason != "" {
		e.reason = reason
	}
	p.saveLocked()
	return true
}

// Unpause 恢复选号。只清 paused，不动冷却/禁用——该号若有未过冷却仍不会被选。
func (p *Pool) Unpause(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.paused = false
	p.saveLocked()
	return true
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
	now := time.Now()
	until := now.Add(d)
	if !e.coolUntil.IsZero() && now.Before(e.coolUntil) && !until.After(e.coolUntil) {
		// 已在冷却中：更重的判罚（plan 权益不足 12h）可以加长，轻的不许缩短已有的。
		return
	}
	e.coolUntil = until
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
	e.setCooldownLocked(time.Now().Add(d), fmt.Sprintf("breaker x%d", e.breakerStreak))
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
	e.setCooldownLocked(time.Now().Add(d), "degraded")
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
	e.setCooldownLocked(time.Now().Add(d), reason)
	e.errCount = 0
	p.saveLocked()
}

// CooldownPlan 1005 权益不足的硬冷却（plan_credit，固定时长不叠加）。
//
// 时长在**锁内**取：原写法 p.Cooldown(uid, CoolPlan, p.lim.PlanCooldown, …) 的实参在进锁前
// 求值，与 ApplyLimits 的整结构体写入（面板热改配置）构成数据竞争（-race 实测报警）。
func (p *Pool) CooldownPlan(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cooldownLocked(uid, p.lim.PlanCooldown, "plan 权益不足")
}

// NoteSuccess 成功请求重置连败计数与退避档位（成功即认为账号恢复正常）。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		// breakerStreak/softStreak/degradeCount 持久化在 state.json（errCount 只在内存）：
		// 清了持久字段就必须落盘，否则崩在下次落盘前会带着虚高退避档位回来（下次一次 429
		// 跳到高档位）。成功是热路径，所以只在真清了持久字段时才写一次。
		persisted := e.degradeCount != 0 || e.breakerStreak != 0 || e.softStreak != 0
		e.errCount, e.degradeCount, e.breakerStreak, e.softStreak = 0, 0, 0, 0
		if persisted {
			p.saveLocked()
		}
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
		UID:           uid,
		Nickname:      e.a.Nickname,
		Realm:         e.a.Realm(),
		Credits:       e.credits,
		CreditsTotal:  e.creditsTotal,
		Expire:        e.expire,
		ExpireCredits: e.expireCredits,
		CreditUnit:    e.unit,
		UsedUSD:       e.usedUSD,
		LimitUSD:      e.limitUSD,
		Cooling:       !e.until.IsZero() && now.Before(e.until),
		Until:         e.until,
		Reason:        e.reason,
		Disabled:      e.disabled,
		Paused:        e.paused,
		ErrCount:      e.errCount,
		Degraded:      e.degradeCount,
		InFlight:      e.inFlight,

		CheckinCooling: !e.coolUntil.IsZero() && now.Before(e.coolUntil),
		CheckinUntil:   e.coolUntil,
		CheckinReason:  e.coolReason,
	}
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (p *Pool) load() {
	// 读不出来时回落到 state.json.bak（上一代好副本）：坏文件不等于「全池没有账号」。
	raw, err := safefile.Read(p.stateFp, validStateFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return // 首次启动
		}
		// 两边都读不出来 = 未知，不是空。备份现场再重建：上一次保存留下的好文件
		// 不会被下一次保存盖掉。
		bak := p.stateFp + ".corrupt"
		if rerr := os.Rename(p.stateFp, bak); rerr != nil {
			log.Printf("[pool] state %s 读不出来且备份失败(%v)，本轮按空状态启动: %v", p.stateFp, rerr, err)
			return
		}
		log.Printf("[pool] state %s 读不出来（%v），已备份为 %s 并按空状态启动（禁用/冷却/积分需重新累积）", p.stateFp, err, bak)
		return
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return
	}
	for uid, s := range sf.Accounts {
		p.byUID[uid] = &entry{
			a:                &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:          s.Credits,
			creditsTotal:     s.CreditsTotal,
			expire:           s.Expire,
			expireCredits:    s.ExpireCredits,
			disabled:         s.Disabled,
			paused:           s.Paused,
			reason:           s.Reason,
			until:            s.Until,
			coolUntil:        s.CoolUntil,
			coolReason:       s.CoolReason,
			breakerStreak:    s.Breaker,
			softStreak:       s.Soft,
			degradeCount:     s.Degrade,
			sessionDeadFails: s.SessDead,
			unit:             s.Unit,
			usedUSD:          s.UsedUSD,
			limitUSD:         s.LimitUSD,
		}
	}
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{Accounts: map[string]stateAccount{}}
	for uid, e := range p.byUID {
		sf.Accounts[uid] = stateAccount{
			Credits:       e.credits,
			CreditsTotal:  e.creditsTotal,
			Expire:        e.expire,
			ExpireCredits: e.expireCredits,
			Disabled:      e.disabled,
			Paused:        e.paused,
			Reason:        e.reason,
			Until:         e.until,
			CoolUntil:     e.coolUntil,
			CoolReason:    e.coolReason,
			Breaker:       e.breakerStreak,
			Soft:          e.softStreak,
			Degrade:       e.degradeCount,
			SessDead:      e.sessionDeadFails,
			Unit:          e.unit,
			UsedUSD:       e.usedUSD,
			LimitUSD:      e.limitUSD,
		}
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	// safefile.WriteKeep：上一代留成 .bak，临时文件 fsync 后才 rename（掉电不留全零文件）。
	if err := safefile.WriteKeep(p.stateFp, raw, 0o600); err != nil {
		log.Printf("[pool] 状态落盘失败（冷却/禁用标记可能在重启后丢失）: %v", err)
	}
}

// validStateFile 状态文件可用口径：能解析成 stateFile 才算（半截 JSON 不算）。
func validStateFile(b []byte) bool {
	var sf stateFile
	return json.Unmarshal(b, &sf) == nil
}
