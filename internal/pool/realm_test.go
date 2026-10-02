package pool

import (
	"testing"

	"traework2api/internal/auth"
)

// 分池选号：国际版模型只能由国际号做，反之亦然。
func TestPickRealmExcluding(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "trae.cn"})
	p.Add(&auth.Auth{UID: "intl1", Domain: "trae.ai", ApiHost: "https://api-sg-central.trae.ai"})

	if a := p.PickRealmExcluding(auth.RealmIntl, nil); a == nil || a.Realm() != auth.RealmIntl {
		t.Fatalf("intl 池选号拿到 %v", a)
	}
	if a := p.PickRealmExcluding(auth.RealmCN, nil); a == nil || a.Realm() != auth.RealmCN {
		t.Fatalf("cn 池选号拿到 %v", a)
	}
	if a := p.PickRealmExcluding("", nil); a == nil {
		t.Fatal("不限地区时不该选不到号")
	}
	// 该地区一个号都没有 → nil（调用方据此返回 503，而不是硬塞一个别的地区的号）。
	q := New("")
	q.Add(&auth.Auth{UID: "cn1", Domain: "trae.cn"})
	if a := q.PickRealmExcluding(auth.RealmIntl, nil); a != nil {
		t.Fatalf("没有国际号却选中了 %v", a.UID)
	}
}
