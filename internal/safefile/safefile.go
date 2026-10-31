// Package safefile 原子、掉电安全地替换状态文件，并在文件损坏时回退到上一份好副本。
//
// 为什么 tmp + rename 不够：rename 只保证读者看到的是一个完整文件，不保证内容
// 已经落盘。掉电时可能留下长度正确、内容全 0 的文件（ext4/XFS 上 rename 的日志
// 先提交、数据块后刷盘）。所以临时文件先 fsync 再 rename，并把替换前的有效内容
// 留成 <path>.bak —— 坏文件被读成「空」之后，下一次写入就会把它彻底抹掉，而这
// 份 .bak 是唯一的退路。
//
// 规则同 magpie 的 internal/steady + internal/lastgood（其 issue #1505：掉电留下
// 全 0 的 logins.json，读成「没有账号」，随即被下一次写入变成定局）：
//   - 文件不在是有人特意删的，不去 .bak 里复活它；
//   - 内容坏但 .bak 好 → 用 .bak，并打 WARN；
//   - 两边都坏 → 报错说清楚路径和坏法，绝不悄悄当成空。
package safefile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Bak 返回 path 的上一份好副本路径。
func Bak(path string) string { return path + ".bak" }

// Write 用 b 原子替换 path：同目录临时文件写完并 fsync 后才 rename，
// 任何一步失败都删掉临时文件、path 保持原样。
func Write(path string, b []byte, perm fs.FileMode) error { return replace(path, b, perm) }

// WriteKeep 同 Write，并在替换前把 path 现有的有效内容留成 <path>.bak。
// 现有内容本身已损坏时另存 <path>.bad-<时间>，不拿坏内容污染 .bak。
func WriteKeep(path string, b []byte, perm fs.FileMode) error {
	if err := keep(path, perm); err != nil {
		return err
	}
	return replace(path, b, perm)
}

// Read 读 path；内容通不过 valid（nil 表示 json.Valid）时回退到 <path>.bak。
// path 不存在返回 fs.ErrNotExist（调用方据此区分「没配置」与「配置坏了」），
// 此时不看 .bak。
func Read(path string, valid func([]byte) bool) ([]byte, error) {
	if valid == nil {
		valid = json.Valid
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if valid(b) {
		return b, nil
	}
	why := describe(b)
	if bak, bakErr := os.ReadFile(Bak(path)); bakErr == nil && valid(bak) {
		log.Printf("WARN: safefile: %s %s，已回退到 %s（原文件保持不动，下次写入前会另存 .bad）", path, why, Bak(path))
		return bak, nil
	}
	return nil, fmt.Errorf("%s %s，且没有可用的 %s", path, why, filepath.Base(Bak(path)))
}

// keep 把 path 现有的有效内容留成 .bak（与 .bak 相同则跳过）。
func keep(path string, perm fs.FileMode) error {
	prev, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !json.Valid(prev) {
		bad := path + ".bad-" + time.Now().Format("20060102-150405")
		if err := replace(bad, prev, perm); err != nil {
			return err
		}
		log.Printf("WARN: safefile: %s %s，已另存 %s", path, describe(prev), bad)
		return nil
	}
	if old, err := os.ReadFile(Bak(path)); err == nil && bytes.Equal(old, prev) {
		return nil
	}
	return replace(Bak(path), prev, perm)
}

// replace 写临时文件 → fsync → rename。
func replace(path string, b []byte, perm fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	tmp = ""
	return nil
}

// describe 说清楚坏在哪，供日志和报错使用。
func describe(b []byte) string {
	switch {
	case len(b) == 0:
		return "是空文件"
	case len(bytes.Trim(b, "\x00")) == 0:
		return strconv.Itoa(len(b)) + " 字节全为 0"
	case len(bytes.TrimSpace(b)) == 0:
		return "只有空白字符"
	}
	return "不是合法 JSON"
}
