package scheduler

import (
	"testing"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
)

// 国际号必须被排除在签到队列外：它没有签到接口（国际域 EpCheckin* 全 404），
// 放进去的代价是每小时一条假报错日志 + 一次白发的 404，最要命的是面板「签到全部」
// 会把国际号算进 errs → 整个操作回报失败，哪怕国内号全都签成了。
func TestByUrgencySkipsIntlAccounts(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "trae.cn", RefreshToken: "rt-cn", AccessToken: "at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "intl1", Domain: "trae.ai", RefreshToken: "rt-intl", AccessToken: "at", ExpiresAt: 9999999999})
	s := newTestScheduler(f, p, srv)

	got := s.byUrgency()
	for _, uid := range got {
		if uid == "intl1" {
			t.Fatalf("国际号不该进签到队列：%v", got)
		}
	}
	if len(got) != 1 || got[0] != "cn1" {
		t.Fatalf("只该剩国内号，实得 %v", got)
	}
}
