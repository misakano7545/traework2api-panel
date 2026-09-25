// Package scheduler 定时任务：每日签到 + token 保活 + 周期余额刷新。
// 签到成功后重新查积分，积分 > 0 的冷却账号自动解冻（只在这一条路径上解冻，见
// RunBalanceRefresh 的说明）。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"slices"
	"sort"
	"sync"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
	"traework2api/internal/upstream"
)

// Config 调度器依赖。
//
// 时点集合与开关在 New 时定下（面板改排程要重启，见 RestartFields）：
// 排程是低频且"改错代价大"的配置，做成热生效反而容易让人误判当前生效值。
type Config struct {
	Pool     *pool.Pool
	Upstream *upstream.Client

	CheckinHours   []int // 每日签到时点，默认 [9]
	KeepaliveHours []int // token 保活时点，默认 [3]

	CheckinEnabled   bool // 默认 true
	KeepaliveEnabled bool // 默认 true

	// BalanceRefreshInterval > 0 时按它周期刷余额（0 = 不启动）。
	BalanceRefreshInterval time.Duration
}

// 面板签到可辨识的账号状态。
var (
	ErrNotFound = errors.New("account not found")
	ErrDisabled = errors.New("account disabled")
)

// Scheduler 调度器。
type Scheduler struct {
	cfg Config
	mu  sync.RWMutex
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{3}
	}
	return &Scheduler{cfg: cfg}
}

// allHours 当前启用的全部触发时点（两个任务都关掉时为空 → 主循环无事可做）。
func (s *Scheduler) allHours() []int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []int
	if s.cfg.CheckinEnabled {
		out = append(out, s.cfg.CheckinHours...)
	}
	if s.cfg.KeepaliveEnabled {
		out = append(out, s.cfg.KeepaliveHours...)
	}
	return out
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// Run 主循环，阻塞直到 ctx 取消；余额刷新另起一条 ticker（周期与整点无关）。
func (s *Scheduler) Run(ctx context.Context) {
	s.mu.RLock()
	balance := s.cfg.BalanceRefreshInterval
	s.mu.RUnlock()
	if balance > 0 {
		go s.runBalanceLoop(ctx, balance)
	}
	hours := s.allHours()
	if len(hours) == 0 {
		// 两个任务都关了：没有时点要等，直接躺到退出（别空转 nextFire）。
		<-ctx.Done()
		return
	}
	for {
		timer := time.NewTimer(time.Until(nextFire(time.Now(), hours)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			h := time.Now().Hour()
			s.mu.RLock()
			doCheckin := s.cfg.CheckinEnabled && contains(s.cfg.CheckinHours, h)
			doKeepalive := s.cfg.KeepaliveEnabled && contains(s.cfg.KeepaliveHours, h)
			s.mu.RUnlock()
			if doCheckin {
				s.RunCheckinNow()
			}
			if doKeepalive {
				s.RunRefreshNow()
			}
		}
	}
}

// runBalanceLoop 周期刷新余额；interval 由配置决定（<=0 时调用方不启动）。
func (s *Scheduler) runBalanceLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RunBalanceRefresh()
		}
	}
}

// RunBalanceRefresh 刷新全部账号的积分与到期时间；返回查询失败的账号数。
//
// 与签到的区别：只读余额，不刷 token、**不解冻**。"余额 > 0 就解冻"是签到那一步的语义
// （用户手动签到就是为了解冻），套到周期刷新上会让熔断/降权/计划冷却每 5 分钟被清一次，
// 等于这些机制不存在。
func (s *Scheduler) RunBalanceRefresh() int {
	failed := 0
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		remain, total, expire, err := s.cfg.Upstream.UserEntUsage(a)
		if err != nil {
			failed++
			continue
		}
		s.cfg.Pool.SetCreditsExpire(st.UID, remain, total, expire)
	}
	if failed > 0 {
		log.Printf("balance refresh: %d 个账号查询失败", failed)
	}
	return failed
}

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// RunCheckinNow 立即对所有账号执行签到 + 积分刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
//
// 顺序按积分到期紧迫度，不是池内顺序：上游对每日签发存在限频约束，
// 把快过期的账号排前面，限频窗口用尽时有效积分留存最大。
func (s *Scheduler) RunCheckinNow() []error {
	var errs []error
	for _, uid := range s.byUrgency() {
		if err := s.CheckinUID(uid); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", uid, err))
		}
	}
	return errs
}

// byUrgency 探一轮余额，按最早到期时间升序返回待签 uid；无到期信息的排最后。
// 探失败不剔除账号（仍要去签，失败原因由 CheckinUID 报出来），只是没有紧迫度信息。
// ponytail: 每账号多一次只读探询换准确排序；号多了改成复用上一轮落盘的 expire。
func (s *Scheduler) byUrgency() []string {
	type row struct {
		uid    string
		expire int64
	}
	rows := make([]row, 0)
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		// 签到域冷却中的账号跳过本轮：上游拥塞时反复重试只会更堵。
		if s.cfg.Pool.CheckinCooling(st.UID) {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		expire := int64(0)
		if remain, total, exp, err := s.cfg.Upstream.UserEntUsage(a); err == nil {
			expire = exp
			s.cfg.Pool.SetCreditsExpire(st.UID, remain, total, exp)
		}
		rows = append(rows, row{uid: st.UID, expire: expire})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].expire, rows[j].expire
		if a == 0 {
			return false // 无到期信息排最后
		}
		if b == 0 {
			return true
		}
		return a < b
	})
	uids := make([]string, 0, len(rows))
	for _, r := range rows {
		uids = append(uids, r.uid)
	}
	return uids
}

// CheckinUID 签到并按剩余积分解冻单个账号。禁用返回 ErrDisabled。
func (s *Scheduler) CheckinUID(uid string) error {
	st, ok := s.cfg.Pool.Status(uid)
	if !ok {
		return ErrNotFound
	}
	if st.Disabled {
		return ErrDisabled
	}
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil || a.RefreshTokenValue() == "" {
		return fmt.Errorf("no refresh token")
	}
	checkedIn, _, enable, err := s.cfg.Upstream.CheckinStatus(a)
	var checkinErr error
	if err != nil {
		checkinErr = err
		log.Printf("checkin status %s: %v", uid, err)
	} else if !checkedIn && enable {
		if err := s.claimWithRetry(a); err != nil {
			checkinErr = err
			log.Printf("checkin claim %s: %v", uid, err)
		} else {
			log.Printf("checkin %s: ok", uid)
		}
	} else if checkedIn {
		log.Printf("checkin %s: already checked in", uid)
	}
	remain, total, expire, err := s.cfg.Upstream.UserEntUsage(a)
	if err != nil {
		log.Printf("ent-usage %s: %v", uid, err)
		return err
	}
	// 先落余额与到期时间（签到后的新值），再谈解冻/冷却。
	s.cfg.Pool.SetCreditsExpire(uid, remain, total, expire)
	// ReenableIfCredits 会在 remain>0 时清网关冷却，即「签到解冻」（改动前即如此）。
	// 它不碰签到域冷却，所以下面两条路径都不会把签到冷却冲掉。
	s.cfg.Pool.ReenableIfCredits(uid, remain)
	if checkinErr == nil {
		s.cfg.Pool.ClearCheckinCooldown(uid)
		return nil
	}
	// 签到失败：记一次签到域冷却，避免下一轮对拥塞的上游反复重试
	// （9074「当前参与用户太多」是上游拥塞，重试只会更堵）。
	// 只挡签到轮次，不进网关选号——对话额度跟签到接口拥塞无关。
	if d, ok := checkinCooldown(checkinErr); ok {
		s.cfg.Pool.CooldownCheckin(uid, d, checkinErr.Error())
	}
	return checkinErr
}

// checkinCooldown 按签到失败类型决定冷却时长。
// 业务失败（HTTP 200 + 非 0 业务码）不会因重试而改变，短冷却只为不再连打上游；
// plan 权益不足要等很久，给长冷却。
func checkinCooldown(err error) (time.Duration, bool) {
	var be *upstream.BusinessError
	if errors.As(err, &be) {
		if be.Code == 1005 {
			return 12 * time.Hour, true
		}
		return 5 * time.Minute, true
	}
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.Kind == upstream.ErrSoftRate {
		return time.Minute, true
	}
	return 0, false
}

// claimWithRetry 只在网络层失败时重试一次。
// 业务失败与 HTTP 错误不会因重试而改变，重试只会成倍增加上游请求、触发风控。
func (s *Scheduler) claimWithRetry(a *auth.Auth) error {
	err := s.cfg.Upstream.CheckinClaim(a)
	if err == nil || !isNetworkError(err) {
		return err
	}
	time.Sleep(time.Second)
	return s.cfg.Upstream.CheckinClaim(a)
}

// isNetworkError 判断是否传输层失败：http.Client 会把传输错误包成 *url.Error，
// 而 HTTP 4xx/5xx 与业务失败走的是 *upstream.Error / *upstream.BusinessError。
func isNetworkError(err error) bool {
	var uerr *url.Error
	return errors.As(err, &uerr)
}

// CatchUp 启动补跑：已过当天最早的签到时点就补签一次。
// 排程定在 9 点而进程 10 点才起来是常态，不补跑等于当天整天不签。
//
// 不记台账：CheckinUID 先探上游状态，已签到不会重复 claim，
// 代价是进程当天反复重启时会多几次只读探询。
func (s *Scheduler) CatchUp() {
	s.mu.RLock()
	enabled := s.cfg.CheckinEnabled
	hours := append([]int(nil), s.cfg.CheckinHours...)
	s.mu.RUnlock()
	if !enabled || len(hours) == 0 {
		return
	}
	earliest := slices.Min(hours)
	if h := time.Now().Hour(); h < earliest {
		return
	}
	log.Printf("checkin catch-up: 已过 %d 点签到小时，补跑一轮", earliest)
	if errs := s.RunCheckinNow(); len(errs) != 0 {
		log.Printf("checkin catch-up: %d 个账号失败", len(errs))
	}
}

// RunRefreshNow 立即对所有账号刷新 token（保活：不看剩余有效期，每天无条件刷一遍）。
//
// 为什么不按 RefreshSkew 只刷快过期的：access token 活 14 天，按「过期前 24h」刷等于
// refresh token 链十天半个月没人碰；上游一旦让它过期就只能重登（掉线）。
// session 失效不立刻杀号：连续 SessionDeadThreshold 次才禁用，成功即清零。
func (s *Scheduler) RunRefreshNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("refresh %s: %v", st.UID, err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("refresh %s: 连续 %d 次 session 失效 — 禁用", st.UID, pool.SessionDeadThreshold())
				}
			}
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID)
		if err := a.SaveAtomic(); err != nil {
			log.Printf("refresh %s save: %v", st.UID, err)
		}
	}
}
