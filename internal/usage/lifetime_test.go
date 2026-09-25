package usage

import (
	"testing"
	"time"
)

// 账号池三列要的是累计口径：窗口外的老桶照样进 Life/LastOK，窗口内的聚合不受影响。
func TestSnapshotLifetimeVsWindow(t *testing.T) {
	r := New("")
	now := time.Now()
	old := now.Add(-100 * time.Hour) // 窗口（72h）外

	r.Add(old, "u1", "m", delta(1, 1, 2, 0), true)  // 老成功
	r.Add(now, "u1", "m", delta(1, 1, 2, 0), true)  // 新成功
	r.Add(now, "u1", "m", delta(0, 0, 0, 0), false) // 失败
	r.Add(old, "u2", "m", delta(1, 1, 2, 0), true)  // 只在窗口外成功过
	r.Add(now, "u2", "m", delta(0, 0, 0, 0), false) // 只有失败

	rows := map[string]KeyedAgg{}
	for _, a := range r.Snapshot(72, nil).ByAccount {
		rows[a.Key] = a
	}

	u1 := rows["u1"]
	if u1.Requests != 2 || u1.Errors != 1 { // 窗口内：2 次（1 成 1 败）
		t.Fatalf("窗口口径 u1 = %+v", u1.Agg)
	}
	if u1.Life == nil || u1.Life.Requests != 3 || u1.Life.Errors != 1 {
		t.Fatalf("累计口径 u1 = %+v（要含窗口外的那次）", u1.Life)
	}
	if u1.LastOK.Unix() != now.Unix() {
		t.Fatalf("u1 最近成功 = %v，期望窗口内那次", u1.LastOK)
	}

	// u2 窗口内只有失败，但累计里有一次老成功：最近成功必须还是那个老时间。
	u2 := rows["u2"]
	if u2.Life == nil || u2.Life.Requests != 2 || u2.Life.Errors != 1 {
		t.Fatalf("累计口径 u2 = %+v", u2.Life)
	}
	if u2.LastOK.Unix() != old.Unix() {
		t.Fatalf("u2 最近成功 = %v，期望窗口外那次 %v", u2.LastOK, old)
	}
}

// 折叠成日桶不能把「最后一次成功」弄丢（否则坏号会显示成更早成功过）。
func TestRollupKeepsLastOK(t *testing.T) {
	r := New("")
	now := time.Now()
	old := now.Add(-200 * 24 * time.Hour)
	r.Add(old, "u1", "m", delta(1, 1, 2, 0), true)
	r.Rollup(now)

	a := r.Snapshot(0, nil).ByAccount[0]
	if a.LastOK.Unix() != old.Unix() {
		t.Fatalf("折叠后最近成功 = %v，期望 %v", a.LastOK, old)
	}
	if a.Life == nil || a.Life.Requests != 1 {
		t.Fatalf("折叠后累计 = %+v", a.Life)
	}
	// 失败桶不该写 OKAt（否则「最近成功」会把失败当成功）。
	r.Add(now, "u2", "m", delta(0, 0, 0, 0), false)
	for _, x := range r.Snapshot(0, nil).ByAccount {
		if x.Key == "u2" && !x.LastOK.IsZero() {
			t.Fatalf("只失败过的账号不该有最近成功: %v", x.LastOK)
		}
	}
}
