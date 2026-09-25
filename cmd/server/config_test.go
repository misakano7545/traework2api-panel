package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDefault(t *testing.T) {
	c := Default()
	if c.Listen != ":7864" {
		t.Errorf("listen=%s", c.Listen)
	}
	if c.DefaultModel != "glm-5.2" {
		t.Errorf("default_model=%s", c.DefaultModel)
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.PlanCreditDur.Hours() != 12 {
		t.Errorf("plan_credit=%v", c.PlanCreditDur)
	}
	if c.SoftRateDur.Seconds() != 60 {
		t.Errorf("soft_rate=%v", c.SoftRateDur)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999","default_model":"kimi-k2.7-code","schedule":{"checkin_hours":[7]},"pool":{"breaker_threshold":5}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	// 未在 JSON 里出现的键保留默认（开关缺省 true）；出现的键被覆盖。
	if c.Listen != ":9999" || c.DefaultModel != "kimi-k2.7-code" ||
		!slices.Equal(c.Schedule.CheckinHours, []int{7}) || c.Pool.BreakerThreshold != 5 ||
		!c.Schedule.CheckinEnabled {
		t.Errorf("c=%+v", c)
	}
	// APIKey 不读 json
	if c.APIKey != "" {
		t.Errorf("api_key should not come from json, got %q", c.APIKey)
	}
}

func TestLoadMissingFileFallsBackToDefaults(t *testing.T) {
	c, err := Load("/nonexistent/config.json")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":7864" {
		t.Errorf("listen=%s", c.Listen)
	}
}

// 密钥的单一来源是 config.json：环境变量不再参与（面板改的就是文件里那一项）。
func TestAPIKeyOnlyFromFile(t *testing.T) {
	t.Setenv("TW2A_API_KEY", "env-key")
	fp := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(fp, []byte(`{"api_key":"file-key","listen":":7864"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKey != "file-key" {
		t.Errorf("api_key=%q（要取文件里的，env 不覆盖）", c.APIKey)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("TW2A_LISTEN", ":7777")
	t.Setenv("TW2A_DEFAULT_MODEL", "qwen-3.7-plus")
	t.Setenv("TW2A_SOFT_RATE", "30s")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":7777" || c.DefaultModel != "qwen-3.7-plus" {
		t.Errorf("c=%+v", c)
	}
	if c.SoftRateDur.Seconds() != 30 {
		t.Errorf("soft_rate=%v", c.SoftRateDur)
	}
}

func TestParseBodyTakesSubmittedKeyAndMarksRestart(t *testing.T) {
	base := Default()
	base.APIKey = "secret-key"
	next, err := ParseBody(base, []byte(`{"api_key":"panel-key","listen":":9999","default_model":"kimi-k2.6","schedule":{"checkin_hours":[7]},"upstream":{"timeout_seconds":30}}`))
	if err != nil {
		t.Fatal(err)
	}
	if next.APIKey != "panel-key" || next.DefaultModel != "kimi-k2.6" ||
		!slices.Equal(next.Schedule.CheckinHours, []int{7}) || next.Upstream.TimeoutSeconds != 30 {
		t.Fatalf("%+v", next)
	}
	if got := strings.Join(RestartFields(base, next), ","); got != "listen,schedule.checkin_hours" {
		t.Fatalf("重启项=%q", got)
	}
	if _, err := ParseBody(base, []byte(`{"schedule":{"checkin_hours":[99]}}`)); err == nil {
		t.Fatal("hour 99 should fail")
	}
	if _, err := ParseBody(base, []byte(`{"upstream":{"timeout_seconds":0}}`)); err == nil {
		t.Fatal("timeout 0 should fail")
	}
	fp := filepath.Join(t.TempDir(), "config.json")
	if err := SaveFile(fp, next); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(fp)
	// 面板填的密钥要落盘：文件是唯一来源，不落盘等于刷新后配置和实际不一致。
	if !strings.Contains(string(raw), "panel-key") {
		t.Fatalf("面板填的 api_key 没写进文件: %s", raw)
	}
	fi, _ := os.Stat(fp)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm %o", fi.Mode().Perm())
	}
}

func TestBadDuration(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"plan_credit":"not-a-duration"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad duration")
	}
}

// 排程时点没有运行时 setter，改了必须报重启——否则面板谎称已生效。
func TestRestartFieldsReportKeepaliveHours(t *testing.T) {
	base := Default()
	next, err := ParseBody(base, []byte(`{"schedule":{"keepalive_hours":[3,15]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(RestartFields(base, next), ","); got != "schedule.keepalive_hours" {
		t.Fatalf("got %q", got)
	}
	// 同值不报，避免每次保存都挂一条假重启项。
	same, err := ParseBody(base, []byte(`{"schedule":{"keepalive_hours":[3]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := RestartFields(base, same); len(got) != 0 {
		t.Fatalf("same value reported restart: %v", got)
	}
}

// 首次运行写出的配置：0600、随机 sk- 密钥（默认监听 0.0.0.0，空 key 等于裸暴露）、
// 结构与 wb 对齐。env 会覆盖 api_key，测试里必须清掉，否则断言的是环境而不是代码。
func TestWriteDefault(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "sub", "config.json")
	key, err := WriteDefault(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "sk-") || len(key) < 20 {
		t.Fatalf("生成的密钥不像 sk-：%q", key)
	}
	fi, err := os.Stat(fp)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("权限=%v，期望 0600", fi.Mode().Perm())
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKey != key {
		t.Fatalf("文件里的密钥 %q != 返回的 %q", c.APIKey, key)
	}
	if c.Pool.BreakerThreshold != 3 || len(c.Schedule.CheckinHours) != 1 || !c.SessionSticky.Enabled {
		t.Fatalf("默认值不对: %+v", c)
	}
	// 已存在时 O_EXCL 拒绝覆盖（绝不改写用户配置），此时不该返回密钥。
	if again, err := WriteDefault(fp); err == nil {
		t.Fatal("已存在时不该覆盖")
	} else if again != "" {
		t.Fatalf("拒绝覆盖时不该返回密钥: %q", again)
	}
	// 两次生成的密钥必须不同（crypto/rand，不是固定值）。
	if err := os.Remove(fp); err != nil {
		t.Fatal(err)
	}
	if key2, err := WriteDefault(fp); err != nil || key2 == key {
		t.Fatalf("重新生成的密钥重复或失败: %q %v", key2, err)
	}
}

// 未搬过来的 wb 键（活动任务/global/upstash 等）被当未知字段忽略，不报错、不生效。
func TestUnportedWorkbuddyKeysIgnored(t *testing.T) {
	c, err := Load(writeTempConfig(t, `{
	  "listen": ":7864",
	  "schedule": {"travel_hours":[9],"blackcat_enabled":true,"checkin_hours":[9,21]},
	  "global": {"enabled": true, "chat_base": "https://x"},
	  "upstash": {"url": "rediss://x", "token": "t"},
	  "features": {"sanitize_blacklist_fingerprints": true},
	  "pool": {"max_in_flight_global": 2, "cost_explore_interval": "30m", "breaker_threshold": 4}
	}`))
	if err != nil {
		t.Fatalf("未知键不该导致加载失败: %v", err)
	}
	if len(c.Schedule.CheckinHours) != 2 || c.Pool.BreakerThreshold != 4 {
		t.Fatalf("已支持的键应生效: %+v", c)
	}
	// 排程开关缺省 true（键缺席保留默认）。
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Fatalf("开关缺省应为 true: %+v", c.Schedule)
	}
}

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	fp := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return fp
}
