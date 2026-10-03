package scheduler

import (
	"testing"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
)

// TestCheckinUIDSkipsIntl 国际号签到入口的兜底守卫。
//
// 签到有三个入口（面板单号按钮直接调 CheckinUID、批量/补签扫描经 byUrgency、启动补跑），
// 而国际域没有签到接口（EpCheckin* 全 404）。守卫必须在 CheckinUID 里——只挡 byUrgency
// 的那一版，面板单号按钮会回 502「国际版没有签到/积分接口」，面板「签到全部」也会把它
// 算进失败。这条网钉三件事：不报错、状态是 skipped（不是 failed）、**根本没去打签到接口**。
func TestCheckinUIDSkipsIntl(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500, resourceUsed: 100}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	// ApiHost 指向假上游：国际号的积分走自家域（ugBaseFor → ApiHost / UgHostIntl），
	// 不设就会打到真域名——单测里那次网络调用会被 err!=nil 静默吞掉，断言变成空转。
	p.Add(&auth.Auth{UID: "intl1", Domain: "trae.ai", ApiHost: srv.URL, RefreshToken: "rt-intl", AccessToken: "at", ExpiresAt: 9999999999})
	s := newTestScheduler(f, p, srv)

	res, err := s.CheckinUID("intl1")
	if err != nil {
		t.Fatalf("国际号签到不该报错（按跳过处理）：%v", err)
	}
	if res.Status != "skipped" {
		t.Fatalf("国际号状态应为 skipped，实得 %q（failed 会让面板显示成失败）", res.Status)
	}
	if f.checkinCalls.Load() != 0 || f.claimCalls.Load() != 0 {
		t.Fatalf("不该去打签到接口：status=%d claim=%d", f.checkinCalls.Load(), f.claimCalls.Load())
	}
	// 顺带刷余额：这一轮不白跑（国际号有 v1 积分接口）。
	if st, ok := p.Status("intl1"); !ok || st.Credits == 0 {
		t.Fatalf("国际号应仍刷新余额与到期时间，实得 %+v (ok=%v)", st, ok)
	}
}

// TestCheckinUIDIntlDisabledStillConflicts 禁用优先于「国际号跳过」：禁用账号仍报 ErrDisabled
// （面板据此回 409，而不是拿一个 skipped 结果掩盖已禁用状态）。
func TestCheckinUIDIntlDisabledStillConflicts(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "intl2", Domain: "trae.ai", RefreshToken: "rt-intl", AccessToken: "at", ExpiresAt: 9999999999})
	p.Disable("intl2", "test")
	s := newTestScheduler(f, p, srv)

	if _, err := s.CheckinUID("intl2"); err != ErrDisabled {
		t.Fatalf("禁用账号应回 ErrDisabled，实得 %v", err)
	}
}
