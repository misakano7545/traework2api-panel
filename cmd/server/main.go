// main.go traework2api 入口：加载配置 → 构建 pool → 起 HTTP 服务。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/panel"
	"traework2api/internal/pool"
	"traework2api/internal/scheduler"
	"traework2api/internal/server"
	"traework2api/internal/session"
	"traework2api/internal/upstream"
	"traework2api/internal/usage"
)

// limitsOf 把配置翻成池参数（唯一转换点：启动与热改走同一个函数，不会两处漂移）。
func limitsOf(c *Config) pool.Limits {
	return pool.Limits{
		MaxInFlight:        c.Pool.MaxInFlight,
		BreakerThreshold:   c.Pool.BreakerThreshold,
		BreakerCooldown:    c.BreakerCooldownDur,
		BreakerCooldownMax: c.BreakerCooldownMaxD,
		DegradeThreshold:   c.Pool.DegradeThreshold,
		DegradeCooldown:    c.DegradeCooldownDur,
		DegradeCooldownMax: c.DegradeCooldownMaxD,
		IdleWeightPerHour:  c.Pool.IdleWeightPerHour,
		IdleWeightMax:      c.Pool.IdleWeightMax,
		ExpiringSoon:       c.ExpiringSoonDur,
		PlanCooldown:       c.PlanCreditDur,
		SoftCooldown:       c.SoftRateDur,
		SoftCooldownMax:    c.SoftRateMaxDur,
	}
}

// applyUpstream 出站身份与超时（启动与热改共用）。
func applyUpstream(up *upstream.Client, c *Config) {
	up.SetTimeouts(
		time.Duration(c.Upstream.TimeoutSeconds)*time.Second,
		time.Duration(c.Upstream.HeaderTimeoutSeconds)*time.Second,
		time.Duration(c.Upstream.IdleTimeoutSeconds)*time.Second,
	)
	upstream.SetIdentity(c.Upstream.UserAgent, c.Upstream.ClientVersion)
}

// usagePathFor 由 state 文件路径推出用量台账路径：同目录、文件名 usage.json。
// 这样 config 里改 state_file 时用量数据跟着走，不用多加一个配置项。
func usagePathFor(stateFile string) string { return stateSibling(stateFile, "usage.json") }

// stateSibling 返回与 state 文件同目录的指定文件名路径（相对路径场景回落当前目录）。
func stateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	logs := panel.NewRing(400)
	log.SetOutput(io.MultiWriter(os.Stderr, logs))

	// 首次运行自动落一份推荐配置（后面改配置就有文件可编辑/可版本化）。
	if *cfgPath != "" {
		if _, statErr := os.Stat(*cfgPath); os.IsNotExist(statErr) {
			key, err := WriteDefault(*cfgPath)
			if err != nil {
				log.Printf("生成默认配置 %s 失败（继续用内置默认值）: %v", *cfgPath, err)
			} else {
				log.Printf("首次运行：已生成默认配置 %s（已随机生成 api_key）", *cfgPath)
				// 密钥只写 stderr，不进日志环：面板的日志视图里不该出现可用于登录的密钥
				// （环里的脱敏规则是兜底，不是许可）。journalctl / 终端里能看到。
				fmt.Fprintf(os.Stderr, "api_key = %s   （写在 %s 的 api_key 里，也可在面板里改）\n", key, *cfgPath)
			}
		}
	}
	cfg, err := Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	bound := cfg.Clone()
	var cfgMu sync.Mutex

	p := pool.New(cfg.StateFile)
	p.ApplyLimits(limitsOf(cfg))
	p.SyncToDir(auths) // 对齐：剔除 state.json 中已删除 auth 文件的幽灵账号

	// 用量台账：进程内累计 + 防抖原子落盘，面板「用量」视图读它。
	usagePath := usagePathFor(cfg.StateFile)
	rec := usage.New(usagePath)
	rec.Start()
	defer rec.Stop()
	log.Printf("[usage] 用量台账 %s（%s）", usagePath, rec.Describe())

	up := upstream.New()
	applyUpstream(up, cfg)

	// 会话粘性：同一会话的多轮请求粘同一账号（上游 prompt 缓存命中）。
	sess := session.New(session.Config{
		Enabled:    cfg.SessionSticky.Enabled,
		TTL:        cfg.SessionTTL,
		GCInterval: cfg.SessionGCInterval,
	})
	defer sess.Stop()

	sch := scheduler.New(scheduler.Config{
		Pool:                   p,
		Upstream:               up,
		RefreshSkew:            24 * time.Hour,
		CheckinHours:           cfg.Schedule.CheckinHours,
		KeepaliveHours:         cfg.Schedule.KeepaliveHours,
		CheckinEnabled:         cfg.Schedule.CheckinEnabled,
		KeepaliveEnabled:       cfg.Schedule.KeepaliveEnabled,
		BalanceRefreshInterval: cfg.BalanceRefreshInt,
	})

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		DefaultModel: cfg.DefaultModel,
		PromptMode:   cfg.Prompt.Mode,
		PromptText:   cfg.PromptText,
		Session:      sess,
		Usage:        rec,
	})
	var pn *panel.Panel // 先声明：SaveConfig 闭包里要调 pn.ApplyAPIKey，赋值在这之后
	pn = panel.New(panel.Config{
		Pool:       p,
		Upstream:   up,
		Scheduler:  sch,
		AuthDir:    cfg.AuthDir,
		APIKey:     cfg.APIKey,
		Version:    "traework2api",
		Logs:       logs,
		Models:     h.Models,
		Usage:      rec,
		ConfigPath: *cfgPath,
		LoadConfig: func() (any, error) {
			cfgMu.Lock()
			defer cfgMu.Unlock()
			// 如实回显整份配置（含 api_key）：面板显示的就是 config.json 的内容。
			return cfg.Clone(), nil
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			cfgMu.Lock()
			defer cfgMu.Unlock()
			next, err := ParseBody(cfg, raw)
			if err != nil {
				return nil, err
			}
			path := *cfgPath
			if path == "" {
				path = "config.json"
			}
			if err := SaveFile(path, next); err != nil {
				return nil, err
			}
			*cfg = *next
			// 热生效：池参数、运行期设置、出站身份/超时、会话粘性开关。
			// 排程（时点/开关/余额周期）不走这里，如实报"需重启"（RestartFields）。
			p.ApplyLimits(limitsOf(next))
			h.ApplyRuntime(server.Runtime{
				DefaultModel: next.DefaultModel,
				PromptMode:   next.Prompt.Mode,
				PromptText:   next.PromptText,
				APIKey:       next.APIKey,
			})
			pn.ApplyAPIKey(next.APIKey) // 面板自己也认新密钥，否则保存完就把自己挡在门外
			applyUpstream(up, next)
			sess.Apply(session.Config{Enabled: next.SessionSticky.Enabled, TTL: next.SessionTTL})
			return RestartFields(bound, next), nil
		},
	})
	h.MountPanel(pn)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)
	// 启动补跑：排程定在 9 点而进程 10 点才起来是常态，不补跑等于当天整天不签。
	go sch.CatchUp()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	host := cfg.Listen
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	log.Printf("traework2api listening on %s (api_key=%v) panel http://%s/panel/", cfg.Listen, cfg.APIKey != "", host)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}
