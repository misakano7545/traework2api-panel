package safefile

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := Write(path, b, 0o600); err != nil {
		t.Fatalf("Write(%s): %v", path, err)
	}
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return b
}

// 第二次写入后 .bak 是上一份内容，第三次之后仍是上一次的（不是最早的）。
func TestWriteKeepKeepsPrevious(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	write(t, p, []byte(`{"v":1}`))
	if err := WriteKeep(p, []byte(`{"v":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(read(t, Bak(p)))); got != `{"v":1}` {
		t.Fatalf(".bak = %s", got)
	}
	if err := WriteKeep(p, []byte(`{"v":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(read(t, Bak(p)))); got != `{"v":2}` {
		t.Fatalf(".bak 不是上一份: %s", got)
	}
	if got := strings.TrimSpace(string(read(t, p))); got != `{"v":3}` {
		t.Fatalf("正文 = %s", got)
	}
}

// 掉电现场：文件长度正确但内容全 0 —— 读回 .bak，原文件不动。
func TestReadFallsBackToBak(t *testing.T) {
	p := filepath.Join(t.TempDir(), "logins.json")
	good := []byte(`{"a":"token"}`)
	if err := WriteKeep(p, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteKeep(p, []byte(`{"a":"newer"}`), 0o600); err != nil { // .bak = good
		t.Fatal(err)
	}
	zeroed := make([]byte, len(read(t, p)))
	if err := os.WriteFile(p, zeroed, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Read(p, nil)
	if err != nil || string(got) != string(good) {
		t.Fatalf("Read = %s, %v", got, err)
	}
	if string(read(t, p)) == string(good) {
		t.Fatal("Read 不该改原文件")
	}
}

// 文件不在是有人特意删的：不复活 .bak，错误要能被 errors.Is(fs.ErrNotExist) 认出。
func TestReadMissingIsNotExist(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := WriteKeep(p, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(p, nil); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
}

// 两边都坏：报错说清楚路径和坏法，不返回空内容。
func TestReadBothBad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(p, make([]byte, 64), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Read(p, nil)
	if err == nil || !strings.Contains(err.Error(), "全为 0") {
		t.Fatalf("err = %v", err)
	}
}

// 覆盖一个已损坏的文件：坏内容另存 .bad-*，.bak 不被污染。
func TestWriteKeepFromCorruptFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "usage.json")
	write(t, p, []byte(`{"good":1}`))
	if err := WriteKeep(p, []byte(`{"good":2}`), 0o600); err != nil { // .bak = {"good":1}
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"trunc`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteKeep(p, []byte(`{"good":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(read(t, Bak(p)))); got != `{"good":1}` {
		t.Fatalf(".bak 被坏内容污染: %s", got)
	}
	bad, err := filepath.Glob(p + ".bad-*")
	if err != nil || len(bad) != 1 {
		t.Fatalf(".bad 副本 %v, %v", bad, err)
	}
	if got := string(read(t, bad[0])); got != `{"trunc` {
		t.Fatalf(".bad 内容 = %s", got)
	}
}

// 自定义校验：JSON 合法但语义不合格的文件也要走 .bak。
func TestReadCustomValidator(t *testing.T) {
	p := filepath.Join(t.TempDir(), "workbuddy-x.json")
	if err := WriteKeep(p, []byte(`{"auth":{"accessToken":"t1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteKeep(p, []byte(`{"auth":{"accessToken":"t2"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := func(b []byte) bool {
		var v struct {
			Auth struct {
				AccessToken string `json:"accessToken"`
			} `json:"auth"`
		}
		return json.Unmarshal(b, &v) == nil && v.Auth.AccessToken != ""
	}
	// 语义不合格（accessToken 空）的正文：JSON 合法，但必须回退 .bak。
	if err := os.WriteFile(p, []byte(`{"auth":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Read(p, valid)
	if err != nil || !strings.Contains(string(got), "t1") {
		t.Fatalf("Read = %s, %v", got, err)
	}
}
