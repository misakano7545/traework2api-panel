package session

import (
	"testing"
	"time"
)

const bodyA = `{"model":"m","messages":[{"role":"system","content":"S"},{"role":"user","content":"第一条"}]}`
const bodyB = `{"model":"m","messages":[{"role":"system","content":"S"},{"role":"user","content":"另一条"}]}`

// 显式会话键优先；没有显式键时用内容派生，且带 user 维度时不派生。
func TestExtractKey(t *testing.T) {
	if got := ExtractKey([]byte(`{"conversation_id":"c1","messages":[]}`)); got != "c1" {
		t.Fatalf("显式键=%q", got)
	}
	if got := ExtractKey([]byte(`{"metadata":{"conversation_id":"c2"},"messages":[]}`)); got != "c2" {
		t.Fatalf("metadata 键=%q", got)
	}
	if got := ExtractKey([]byte(`{"prompt_cache_key":"p1"}`)); got != "p1" {
		t.Fatalf("prompt_cache_key=%q", got)
	}
	a1, a2 := ExtractKey([]byte(bodyA)), ExtractKey([]byte(bodyA))
	if a1 == "" || a1 != a2 {
		t.Fatalf("派生键应稳定: %q %q", a1, a2)
	}
	if b := ExtractKey([]byte(bodyB)); b == a1 {
		t.Fatal("不同会话不该派生同键")
	}
	if got := ExtractKey([]byte(`{"user_id":"u","messages":[{"role":"user","content":"x"}]}`)); got != "" {
		t.Fatalf("带 user 维度不该派生，got %q", got)
	}
	// 多轮追加历史后，键仍稳定（只看 system + 首条 user）。
	multi := `{"model":"m","messages":[{"role":"system","content":"S"},{"role":"user","content":"第一条"},{"role":"assistant","content":"答"},{"role":"user","content":"追问"}]}`
	if got := ExtractKey([]byte(multi)); got != a1 {
		t.Fatalf("多轮后键漂移: %q vs %q", got, a1)
	}
}

// 绑定可解析、TTL 到期即失效、Unbind 生效。
func TestBindAndTTL(t *testing.T) {
	r := New(Config{Enabled: true, TTL: time.Hour, GCInterval: time.Hour})
	defer r.Stop()
	r.Bind("k", "u1")
	if uid, ok := r.Resolve("k"); !ok || uid != "u1" {
		t.Fatalf("应命中 u1: %q %v", uid, ok)
	}
	if !r.Unbind("k") {
		t.Fatal("Unbind 应报告原本有绑定")
	}
	if _, ok := r.Resolve("k"); ok {
		t.Fatal("解绑后不该命中")
	}

	// TTL 极短 → 过期后不命中，GC 清掉。
	r.Apply(Config{Enabled: true, TTL: time.Millisecond})
	r.Bind("k2", "u2")
	time.Sleep(5 * time.Millisecond)
	if _, ok := r.Resolve("k2"); ok {
		t.Fatal("过期绑定不该命中")
	}
	if n := r.GC(); n != 1 {
		t.Fatalf("GC 应清掉 1 条，got %d", n)
	}

	// 关闭开关后不命中（存量绑定保留，重新打开即可用）。
	r.Apply(Config{Enabled: false, TTL: time.Hour})
	r.Bind("k3", "u3")
	if _, ok := r.Resolve("k3"); ok {
		t.Fatal("关闭时不该命中")
	}
}
