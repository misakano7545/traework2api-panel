// 固定容量日志环。main 把标准日志镜像进来；超出后丢掉最旧的行。
package panel

import (
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	ChChat = "chat"
	ChTask = "task"
	ChSys  = "sys"
)

// LogEntry 一条日志。不保存请求体、回调链接或 token。
type LogEntry struct {
	TS   time.Time `json:"ts"`
	Ch   string    `json:"ch"`
	Text string    `json:"text"`
}

var (
	tsPrefixRe = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)
	secretRE   = regexp.MustCompile(`(?i)((?:bearer|cloud-ide-jwt)\s+|(?:refresh|access)_?token(?:=|"\s*:\s*"))[^&\s"]+`)
	jsonSecret = regexp.MustCompile(`(?i)"(?:accessToken|refreshToken|token|RefreshToken)"\s*:\s*"[^"]*"`)
	callbackRE = regexp.MustCompile(`https?://\S*authorize\?\S+`)
	apiKeyRE   = regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`)
)

func scrub(line string) string {
	line = callbackRE.ReplaceAllString(line, "http://127.0.0.1/authorize?********")
	line = jsonSecret.ReplaceAllString(line, `"token":"********"`)
	line = apiKeyRE.ReplaceAllString(line, "sk-********")
	return secretRE.ReplaceAllString(line, "${1}********")
}

func classifyLine(line string) string {
	switch {
	case strings.HasPrefix(line, "chat_stream") || strings.HasPrefix(line, "| #"):
		return ChChat
	case strings.HasPrefix(line, "checkin") || strings.HasPrefix(line, "refresh") || strings.HasPrefix(line, "panel:"):
		return ChTask
	default:
		return ChSys
	}
}

// Ring 日志环形缓冲。
type Ring struct {
	mu      sync.Mutex
	entries []LogEntry
	cap     int
}

// NewRing 构建容量为 capacity 的日志环（非正值回退 400）。
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = 400
	}
	return &Ring{cap: capacity}
}

// Write 按行入环，并抹掉 token / 回调。
func (r *Ring) Write(p []byte) (int, error) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\r\n"), "\n") {
		if line == "" {
			continue
		}
		text := scrub(tsPrefixRe.ReplaceAllString(line, ""))
		r.entries = append(r.entries, LogEntry{TS: now, Ch: classifyLine(text), Text: text})
		if overflow := len(r.entries) - r.cap; overflow > 0 {
			r.entries = r.entries[overflow:]
		}
	}
	return len(p), nil
}

// Snapshot 按写入顺序返回拷贝。
func (r *Ring) Snapshot() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogEntry, len(r.entries))
	copy(out, r.entries)
	return out
}
