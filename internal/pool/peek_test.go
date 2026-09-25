package pool

import (
	"testing"

	"traework2api/internal/auth"
)

// Peek 不占在途名额（只读探询用），Pick 占；两者是唯一区别。
func TestPeekDoesNotLease(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.ApplyLimits(Limits{MaxInFlight: 1})

	if p.Peek() == nil {
		t.Fatal("Peek 应能选中")
	}
	if st, _ := p.Status("u1"); st.InFlight != 0 {
		t.Fatalf("Peek 不该占名额: %+v", st)
	}
	// 探询多次仍可选中（名额没被吃掉）。
	for i := 0; i < 3; i++ {
		if p.Peek() == nil {
			t.Fatal("多次 Peek 后仍应可选")
		}
	}
	if p.PickExcluding(nil) == nil {
		t.Fatal("Pick 应能选中")
	}
	if st, _ := p.Status("u1"); st.InFlight != 1 {
		t.Fatalf("Pick 应占 1 个名额: %+v", st)
	}
	if p.Peek() != nil {
		t.Fatal("名额占满后 Peek 也不该返回（策略一致）")
	}
	p.Release("u1")
	if p.Peek() == nil {
		t.Fatal("Release 后应恢复")
	}
}
