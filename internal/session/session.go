// Package session 会话粘性：同一会话的多轮请求粘到同一个账号。
//
// 为什么需要：上游对同一账号 + 同一 prompt 前缀有缓存，随机换号会打散缓存，延迟和成本都变差；
// 客户端（CLI/编辑器）在一条会话里连续发多轮，粘住一个号才符合它的预期。
//
// 键从哪来（ExtractKey）：
//  1. 显式会话键优先：metadata.conversation_id / conversationId、顶层 conversation_id /
//     conversationId、prompt_cache_key（OpenAI 系的前缀缓存键，语义就是"同一会话"）。
//  2. 否则从内容派生：SHA-256(system 文本 + 首条 user 文本) 的前 16 字节。
//     不用全部消息：多轮历史每轮追加，全量哈希每轮都变，粘性会完全失效。
//  3. 带 user 维度标识（metadata.user_id / user_id）的请求**不派生**：user 粒度过粗，
//     一个用户的所有并行对话会被钉到同一账号，还不如普通轮换。
//
// 过期：TTL 到期即视为未绑定（Resolve 判定 + GC 清理），不做续期外的任何状态迁移。
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// entry 一次绑定。
type entry struct {
	uid  string
	last time.Time
}

// Config 运行期参数（可由面板热改）。
type Config struct {
	Enabled    bool
	TTL        time.Duration
	GCInterval time.Duration
}

// Router 会话 → 账号 的粘性表。
type Router struct {
	mu       sync.Mutex
	cfg      Config
	bindings map[string]entry

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// New 构建并启动 GC（Enabled 为 false 时 Resolve 恒不命中，GC 仍跑，开销可忽略）。
func New(cfg Config) *Router {
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Minute
	}
	if cfg.GCInterval <= 0 {
		cfg.GCInterval = 5 * time.Minute
	}
	r := &Router{
		cfg:      cfg,
		bindings: map[string]entry{},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go r.gcLoop()
	return r
}

// Apply 热改开关与 TTL（GC 周期改动需重启，见 RestartFields）。
func (r *Router) Apply(cfg Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg.Enabled = cfg.Enabled
	if cfg.TTL > 0 {
		r.cfg.TTL = cfg.TTL
	}
}

// Enabled 当前是否启用粘性。
func (r *Router) Enabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg.Enabled
}

// Stop 停止 GC。
func (r *Router) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

func (r *Router) gcLoop() {
	defer close(r.done)
	t := time.NewTicker(r.cfg.GCInterval)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			r.GC()
		}
	}
}

// GC 清理过期绑定，返回清理条数。
func (r *Router) GC() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	n := 0
	for k, e := range r.bindings {
		if now.Sub(e.last) > r.cfg.TTL {
			delete(r.bindings, k)
			n++
		}
	}
	return n
}

// Resolve 查会话绑定的账号（未启用 / 无绑定 / 已过期 → ok=false）。
func (r *Router) Resolve(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.cfg.Enabled {
		return "", false
	}
	e, ok := r.bindings[key]
	if !ok || time.Since(e.last) > r.cfg.TTL {
		return "", false
	}
	e.last = time.Now() // 命中即续期：活跃会话不该被 GC 掉
	r.bindings[key] = e
	return e.uid, true
}

// Bind 绑定会话到一个账号（覆盖旧绑定）。
func (r *Router) Bind(key, uid string) {
	if key == "" || uid == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bindings[key] = entry{uid: uid, last: time.Now()}
}

// Unbind 解绑（返回是否原本有绑定）。
func (r *Router) Unbind(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.bindings[key]
	delete(r.bindings, key)
	return ok
}

// Count 当前绑定条数（面板展示用）。
func (r *Router) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bindings)
}

// derivedKeyPrefix 派生键前缀，与显式 id 的命名空间隔离（显式 id 优先返回）。
const derivedKeyPrefix = "d-"

// ExtractKey 从请求体提取会话键；提取不到返回空串（调用方退回普通轮换）。
func ExtractKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}
		if v := strOrEmpty(meta["conversationId"]); v != "" {
			return v
		}
	}
	if v := strOrEmpty(obj["conversation_id"]); v != "" {
		return v
	}
	if v := strOrEmpty(obj["conversationId"]); v != "" {
		return v
	}
	if v := strOrEmpty(obj["prompt_cache_key"]); v != "" {
		return v
	}
	if hasUserIDKey(obj) {
		return ""
	}
	return deriveKey(obj)
}

// hasUserIDKey 报告请求是否带 user 维度标识（只用于派生回退闸）。
func hasUserIDKey(obj map[string]any) bool {
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if strOrEmpty(meta["user_id"]) != "" {
			return true
		}
	}
	return strOrEmpty(obj["user_id"]) != ""
}

// deriveKey 从内容派生稳定会话键：SHA-256(system 文本 + 首条 user 文本) 前 16 字节。
// 取不到任何用户文本（纯图片等）→ 空串，退回普通轮换。
func deriveKey(obj map[string]any) string {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return ""
	}
	systemText, firstUserText := "", ""
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch strOrEmpty(msg["role"]) {
		case "system":
			if systemText == "" {
				systemText = messageText(msg["content"])
			}
		case "user":
			if firstUserText == "" {
				firstUserText = messageText(msg["content"])
			}
		}
		if firstUserText != "" {
			break
		}
	}
	if firstUserText == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(systemText + "\x00" + firstUserText))
	return derivedKeyPrefix + hex.EncodeToString(sum[:16])
}

// messageText 取消息文本：content 是字符串直接返回；是分块数组则拼接其中的 text 块。
func messageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := p["text"].(string); ok {
				b.WriteString(t)
			}
		}
		return b.String()
	}
	return ""
}

func strOrEmpty(v any) string {
	s, _ := v.(string)
	return s
}
