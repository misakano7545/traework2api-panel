//go:build audit

// audit_delta_upstream_test.go 只读缺陷审计（本次上游同步增量 6188638..436b0df 专用）。
//
// 与 audit_upstream_test.go 分家：那份钉的是**改动前就存在**的缺陷（错误分类、SSE、
// remain 不夹 0、nil auth…），这份只钉**这次新写进来的代码**：domains.go（agent 域名
// 自动跟进）、client.go 的 Max 判据与美元额度口径、以及新加的 agent_host 热改写侧。
//
//	跑法：go test -tags audit -count=1 -run TestAudit ./internal/upstream/
//	竞态：go test -tags audit -race -count=1 -run TestAudit ./internal/upstream/
//
// 每条断言的都是**当前（错误/可疑）行为**，注释里写「修好之后该改成什么」。
// 为了让它继续绿而放宽断言 = 把证据抹掉。不写真网络：全部内存 RoundTripper + testdata 固件。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"traework2api/internal/auth"
)

// ── 通用假 RoundTripper ────────────────────────────────────────────────────

// audRT 按 host/aid 给响应，并记录每次调用与「请求到达时 ctx 已死」这件事。
//
//	bootHosts 里的 host 视作 TNC boot：按 query 里的 aid 返回对应固件；
//	hangAid   里的 aid 挂到 ctx 结束才返回（模拟「boot 主机不可达/极慢」）；
//	codes     host → 固定状态码（探活用）；其余 host 一律 404。
type audRT struct {
	bootHosts []string
	boot      map[string][]byte // aid → 固件
	hangAid   map[string]bool   // aid → 挂到 ctx 结束
	codes     map[string]int    // host → 状态码

	mu      sync.Mutex
	calls   []string
	ctxDead map[string]error // host → 请求到达时 ctx 的错误（已死的证据）
}

func (a *audRT) aid(req *http.Request) string {
	q := req.URL.RawQuery
	i := strings.Index(q, "aid=")
	if i < 0 {
		return ""
	}
	s := q[i+len("aid="):]
	if j := strings.IndexAny(s, "&"); j >= 0 {
		s = s[:j]
	}
	return s
}

func (a *audRT) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	ctxErr := req.Context().Err()

	a.mu.Lock()
	a.calls = append(a.calls, req.Method+" "+host+req.URL.Path+"?"+req.URL.RawQuery)
	if ctxErr != nil {
		if a.ctxDead == nil {
			a.ctxDead = map[string]error{}
		}
		a.ctxDead[host] = ctxErr
	}
	a.mu.Unlock()

	// 真 transport 会先看 ctx：这里照做，否则「预算耗尽」会被假 transport 悄悄掩盖。
	if ctxErr != nil {
		return nil, ctxErr
	}
	if a.hangAid != nil && a.hangAid[a.aid(req)] {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	mk := func(code int, body []byte) *http.Response {
		return &http.Response{
			StatusCode: code,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    req,
		}
	}
	for _, b := range a.bootHosts {
		if b == host {
			if body, ok := a.boot[a.aid(req)]; ok {
				return mk(http.StatusOK, body), nil
			}
			continue
		}
	}
	if code, ok := a.codes[host]; ok {
		return mk(code, []byte("audit")), nil
	}
	return mk(http.StatusNotFound, []byte("404 page not found")), nil
}

func (a *audRT) calledSubstr(s string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.calls {
		if strings.Contains(c, s) {
			return true
		}
	}
	return false
}

func (a *audRT) deadAt(host string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ctxDead[host]
}

func audFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("读固件 %s: %v", name, err)
	}
	return raw
}

// audDiscoveryClient 造一个只走内存 transport 的 Client（不碰真网络）。
func audDiscoveryClient(rt *audRT) *Client {
	c := New()
	c.discoveryCli = &http.Client{Transport: rt}
	return c
}

func audIntlAuth() *auth.Auth {
	return &auth.Auth{UID: "1000000000000000000", Domain: "trae.ai",
		ApiHost: "https://api-sg-central.trae.ai", AccessToken: "at-audit"}
}

// ── 1. 探活 fail-open：只有 404 判死，5xx / 3xx / 任何 200 都算活 ──────────────

// domains.go:196-199 probeAgentBase 的判据是「不是 404 就算活」。
// 后果不是判活一个死主机，而是 DiscoverHosts（domains.go:220-228）**break 在第一个
// 「活」的候选上** → 一个 502/边缘 HTML 的坏分片会挤掉后面真正健康的候选，
// 并且整条 realm 的基址被切到坏主机上（回退常量这条退路不会触发，因为「探到活了」）。
func TestAuditDeltaProbeFailOpenOn5xxAnd200(t *testing.T) {
	rt := &audRT{
		bootHosts: []string{"boot.test"},
		boot: map[string][]byte{
			"787976": audFixture(t, "get_domains_cn.json"),
			"677332": audFixture(t, "get_domains_intl.json"),
		},
		codes: map[string]int{
			"api5-normal.mchost.guru":   http.StatusBadGateway,         // 首选分片：网关坏了
			"api3-normal.mchost.guru":   http.StatusUnauthorized,       // 次选分片：健康（401=路由在）
			"parked.example":            http.StatusOK,                 // 停车场/边缘 200 HTML
			"api22-normal-alisg.mchost": http.StatusServiceUnavailable, // 国际首选：503
		},
	}
	c := audDiscoveryClient(rt)

	// 5xx 与「200 非 JSON」都必须判死：判活会把 realm 基址切到坏主机上。
	for _, tc := range []struct {
		host, why string
	}{
		{"https://api5-normal.mchost.guru", "502"},
		{"https://parked.example", "200 非 JSON"},
		{"https://api22-normal-alisg.mchost", "503"},
	} {
		if ok, why := c.probeAgentBase(context.Background(), tc.host); ok {
			t.Errorf("%s 现在判活（why=%q）：坏候选会顶掉健康候选", tc.why, why)
		} else {
			t.Logf("probe(%s) → ok=false why=%q", tc.host, why)
		}
	}

	old := tncBootHosts
	tncBootHosts = map[string][]string{auth.RealmCN: {"https://boot.test"}, auth.RealmIntl: {"https://boot.test"}}
	t.Cleanup(func() { tncBootHosts = old })

	if err := c.DiscoverHosts(context.Background()); err != nil {
		t.Fatalf("DiscoverHosts 不该失败（两区表都在固件里）: %v", err)
	}
	// 502 的首选被判死后要继续往下探，选中 401 的 api3-normal。
	if got := c.agentBaseFor(nil); got != "https://api3-normal.mchost.guru" {
		t.Fatalf("国服基址=%q（期望跳过 502 的 api5、选中 api3-normal）", got)
	}
	if !rt.calledSubstr("api3-normal.mchost.guru") {
		t.Fatalf("api3-normal 没被探过：遍历仍在首个候选上 break")
	}
	t.Logf("国服生效基址 = %q（502 被跳过，401 的 api3-normal 被选中）；调用序列 = %v",
		c.agentBaseFor(nil), rt.calls)
}

// ── 2. 发现预算：12s 总预算被两个 realm 串行共用，国服先跑 ────────────────────

// domains.go:205 起一个 12s 的 WithTimeout 给**两个 realm 串行**用，realm 顺序固定在
// domains.go:210（国服先）。单 realm 最坏 3 个 boot × 8s（domainHTTPTimeout）= 24s，
// 探活 4 个候选 × 8s = 32s —— 都大于 12s。
//
// 本测试用「父 ctx 给更短的预算」复现同一结构（预算被前一半耗尽）：国服 boot 挂到
// ctx 结束 → 国际那一半发出的请求到达 transport 时 ctx 已经死了 → 国际永远发现不出来，
// 而同一份固件、同一个 transport，换一个不被耗尽的预算立刻就能发现。
func TestAuditDeltaDiscoveryBudgetStarvesLaterRealm(t *testing.T) {
	rt := &audRT{
		bootHosts: []string{"boot.test"},
		boot: map[string][]byte{
			"787976": audFixture(t, "get_domains_cn.json"),
			"677332": audFixture(t, "get_domains_intl.json"),
		},
		hangAid: map[string]bool{"787976": true}, // 国服 boot 主机不可达/极慢
		codes: map[string]int{
			"api5-normal.mchost.guru":           http.StatusUnauthorized,
			"api22-normal-alisg.mchost.guru":    http.StatusUnauthorized,
			"api22-normal-useast1a.mchost.guru": http.StatusUnauthorized,
			"api16-normal-alisg.mchost.guru":    http.StatusUnauthorized,
			"api16-normal-useast1a.mchost.guru": http.StatusUnauthorized,
		},
	}
	old := tncBootHosts
	tncBootHosts = map[string][]string{auth.RealmCN: {"https://boot.test"}, auth.RealmIntl: {"https://boot.test"}}
	t.Cleanup(func() { tncBootHosts = old })

	c := audDiscoveryClient(rt)

	// 第一轮：预算被「国服先跑」吃掉。
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := c.DiscoverHosts(ctx)
	if err == nil {
		t.Fatal("国服那一半挂住时应当返回错误（国际那半照常发现）")
	}
	t.Logf("预算耗尽后 err = %v", err)

	// 两区并行后：国服吃满预算也不该连累国际那一半 —— 它该在同一个预算里被发现。
	if got := c.effectiveAgentBase(auth.RealmIntl); got != "https://api22-normal-alisg.mchost.guru" {
		t.Fatalf("国际基址=%q（期望 api22-normal-alisg：国服挂住不该饿死国际）", got)
	}
	// 关键证据：国际那一半的表请求有没有被拖到 ctx 死掉之后才发出。
	if err := rt.deadAt("boot.test"); err != nil {
		t.Logf("证据：国际的 boot 请求到达 transport 时 ctx 已死：%v", err)
	} else if rt.calledSubstr("aid=677332") {
		t.Logf("证据：国际的 boot 请求发出过，但两区共用同一个 12s 预算")
	}
	if got := rt.deadAt("boot.test"); got == nil && !rt.calledSubstr("aid=677332") {
		t.Logf("证据：国际那一半在这个预算里连请求都没发出（calls=%v）", rt.calls)
	}
	t.Logf("第一轮：国际生效基址仍 = %q（常量）", c.effectiveAgentBase(auth.RealmIntl))

	// 第二轮：同一份固件、同一个 transport，只把「国服挂住」拿掉。
	rt.hangAid = nil
	if err := c.DiscoverHosts(context.Background()); err != nil {
		t.Fatalf("拿掉慢国服后 DiscoverHosts 仍失败: %v", err)
	}
	if got := c.effectiveAgentBase(auth.RealmIntl); got != "https://api22-normal-alisg.mchost.guru" {
		t.Fatalf("行为已变：国际基址=%q（期望 api22-normal-alisg 被发现）", got)
	}
	t.Logf("第二轮（不被饿死）：国际基址 = %q，国服 = %q",
		c.effectiveAgentBase(auth.RealmIntl), c.effectiveAgentBase(auth.RealmCN))
}

// ── 3. Max 判据只可能来自批量视图：单 function 视图声明的东西被丢掉 ────────────

// client.go:475 MaxAvailable 要 ContextMax > ContextWindow；而 ContextMax **只有**
// mergeCatalog（client.go:808）从批量视图填。两条后果都在新代码里：
//
//	a) client.go:556-566 在单 function 视图里也给变体名，注释承诺「只在老视图里出现的
//	   模型也要能开 Max」——但 mergeCatalog 对 cat 里没有的条目直接 continue（client.go:796），
//	   ContextMax 保持 0，MaxAvailable 恒 false，那句承诺落不了地（DevModelName 也无消费方）。
//	b) pickOfficialModels 只搬了 ContextWindowTokens.Dev，**没搬 .Max**；批量端点一坏
//	   （FetchModels 里 err 被吞掉：client.go:905），单视图里明明声明了 max 也照样没有 Max。
func TestAuditDeltaMaxNeedsBatchCatalog(t *testing.T) {
	// (a) 老视图独有的模型。
	var cfg paramConfig
	single := `{"config_name":"old-only","usage":"chat_completion",
	  "display_config":{"display_name":"OldOnly"},
	  "context_window_tokens":{"dev":116000,"max":1000000},
	  "model_detail_list":[{"model_name":"old-only__dev","max_tokens":8192},
	                       {"model_name":"old-only__max","max_tokens":8192}]}`
	if err := json.Unmarshal([]byte(single), &cfg); err != nil {
		t.Fatalf("构造 paramConfig 失败: %v", err)
	}
	got := pickOfficialModels([]paramConfig{cfg}, nil, Function)
	if len(got) != 1 {
		t.Fatalf("单视图挑出 %d 条，期望 1", len(got))
	}
	if got[0].MaxModelName != "old-only__max" || got[0].DevModelName != "old-only__dev" {
		t.Fatalf("变体名没读出来: dev=%q max=%q", got[0].DevModelName, got[0].MaxModelName)
	}
	merged := mergeCatalog(got, map[string]catalogMeta{}, nil)
	// 单视图自己声明的 max 必须带到 ContextMax（批量端点坏掉时只有这条路）。
	if !merged[0].MaxAvailable() {
		t.Fatalf("(a) 老视图独有条目开不了 Max：ContextMax=%d 名=%q（上游声明了 max=1000000）",
			merged[0].ContextMax, merged[0].MaxModelName)
	}
	t.Logf("(a) 单视图声明 max=%d、__max 名=%q → ContextMax=%d MaxAvailable=%v",
		1000000, merged[0].MaxModelName, merged[0].ContextMax, merged[0].MaxAvailable())

	// (b) 批量端点 500：FetchModels 照样返回模型表，只是 Max 静默消失。
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, EpModelsBatch) {
			return jsonResp(http.StatusInternalServerError, "batch down"), nil
		}
		return jsonResp(http.StatusOK, `{"config_info_list":[`+single+`]}`), nil
	})
	infos, err := c.FetchModels(&auth.Auth{AccessToken: "at-audit"})
	if err != nil {
		t.Fatalf("FetchModels 不该失败（批量端点已回退单视图）: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("模型表 = %d 条，期望 1", len(infos))
	}
	if infos[0].ContextWindow != 116000 {
		t.Fatalf("ContextWindow=%d 期望 116000", infos[0].ContextWindow)
	}
	if !infos[0].MaxAvailable() {
		t.Fatalf("(b) 批量端点 500 后 Max 也没了：context_max=%d 名=%q（上游声明了 max=1000000）",
			infos[0].ContextMax, infos[0].MaxModelName)
	}
	if infos[0].ContextMax != 1000000 {
		t.Fatalf("(b) ContextMax=%d 期望 1000000（单视图的 max 口径）", infos[0].ContextMax)
	}
	t.Logf("(b) 批量端点 500 → 模型仍在（%d 条，context=%d/%d）且 Max 仍可用（单视图口径）",
		len(infos), infos[0].ContextWindow, infos[0].ContextMax)
}

// ── 4. agent_host 热改：新加的写侧与请求读侧无锁并发（-race 报竞态）────────────

// 本次增量在入口新加了 upstream.agent_host / agent_host_intl 两个配置项与热改写回
// （cmd/server/main.go:61-72，缺省 false 之前的版本里 cmd/server **一次都没引用过**
// AgentHost，字段只由 upstream.New() 写过，所以那时没有并发写）。
//
// 写侧在面板保存配置的请求里同步执行（cmd/server/main.go:237 的 SaveConfig → applyUpstream），
// 读侧是每个请求的 agentBaseFor（client.go:255 → domains.go:342 直接读 c.AgentHost）与
// 新加的发现 goroutine（domains.go:216/231/240）。两边都没有锁：这是本次增量新引入的数据竞争。
// 修好后本条测试在 -race 下应当无报告。
func TestAuditDeltaAgentHostHotReloadRace(t *testing.T) {
	c := New()
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() { // 写侧：镜像 cmd/server/main.go 的 applyUpstream（热改走 SetAgentHosts，读侧要加锁）
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			if i%2 == 0 {
				c.SetAgentHosts("https://cn-a.test", "https://intl-a.test")
			} else {
				c.SetAgentHosts("https://cn-b.test", "https://intl-b.test")
			}
		}
	}()

	read := func(fn func()) {
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			fn()
		}
	}
	wg.Add(3)
	go read(func() { _ = c.agentBaseFor(nil) })           // 请求路径（国服）
	go read(func() { _ = c.agentBaseFor(audIntlAuth()) }) // 请求路径（国际）
	go read(func() { _, _, _, _ = c.AgentHosts() })       // 面板展示
	_ = stop

	wg.Wait()
	// 热改与请求读并发，读侧必须只看到两个合法值之一（撕裂读 = 读到半个 URL）。
	// 断言打在 -race 上：这里安静通过 + 无 DATA RACE 报告才算真修好。
	if got := c.agentBaseFor(nil); !strings.HasPrefix(got, "https://cn-") {
		t.Fatalf("读到的基址是垃圾值 %q（撕裂读）", got)
	}
	t.Log("写侧（applyUpstream → SetAgentHosts，持 hostMu）与读侧 agentBaseFor/AgentHosts 并发" +
		" —— 期望 -race 无报告")
}

// ── 5. 美元口径：赠送额度（bonus_*）字段解析了但从未参与计算 ─────────────────

// client.go:1045/1054 新加了 BonusUsageLimit / BonusUsageAmount 两个字段，
// 但 UserEntUsageDetail（client.go:1159-1172）与 BalancePackages（client.go:1271-1288）
// 只读 basic_usage_limit / basic_usage_amount：既不算进额度，也不算进已用；
// 且 limit<=0 的包被整个 skip（client.go:1162/1274）——只有赠送额度的包直接消失。
// 两份真固件里 bonus 都是 0，所以真实影响未证实；字段已声明却无人消费是确定的。
func TestAuditDeltaBonusQuotaIgnored(t *testing.T) {
	body := `{"is_dollar_usage_billing":true,"user_entitlement_pack_list":[
	  {"display_desc":"Free plan","entitlement_base_info":{"quota":{"basic_usage_limit":1,"bonus_usage_limit":5}},
	   "usage":{"basic_usage_amount":0.5,"bonus_usage_amount":0.25}},
	  {"display_desc":"Bonus only","entitlement_base_info":{"quota":{"bonus_usage_limit":3}},
	   "usage":{"bonus_usage_amount":1.0}}]}`
	c := entUsageClient(t, body, EpEntUsageIntl)
	u, err := c.UserEntUsageDetail(audIntlAuth())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	// basic(1+0) + bonus(5+3) = 9；used = basic(0.5+0) + bonus(0.25+1.0) = 1.75。
	if u.LimitUSD != 9 || u.UsedUSD != 1.75 {
		t.Fatalf("bonus 没算进额度：limit=%v used=%v（期望 9 / 1.75）", u.LimitUSD, u.UsedUSD)
	}
	t.Logf("上游给了 bonus_limit=5+3、bonus_used=0.25+1.0；面板看到 limit=%v used=%v（赠送已计入）",
		u.LimitUSD, u.UsedUSD)

	packs, pu, err := c.BalancePackages(audIntlAuth())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	// 「Bonus only」现在有面额了，不再被当作「订阅权益那类」跳过；逐包汇总必须与上面同口径。
	if len(packs) != 2 {
		t.Fatalf("包列表=%d %v（期望 2 个：Bonus only 有面额了）", len(packs), packs)
	}
	if pu.LimitUSD != 9 || pu.UsedUSD != 1.75 {
		t.Fatalf("逐包汇总与 UserEntUsageDetail 不同口径: limit=%v used=%v（期望 9 / 1.75）", pu.LimitUSD, pu.UsedUSD)
	}
	t.Logf("逐包视角：%d 个包，汇总 limit=%v used=%v（两处口径一致）", len(packs), pu.LimitUSD, pu.UsedUSD)
}

// ── 6. 到期时间断言是对齐「当下的 now」：新套件里的绝对时刻会自己过期 ───────────

// client.go:1152/1240 起的到期时间都带 `if e > now` 门槛，所以 Expire 是 **now 的函数**。
// 但同期新加的 ent_usage_unit_test.go 把国际号的 Expire 钉成绝对时刻 1793491199
// （= 固件 end_time，2026-11-01）：那天一过，expireQuota/expireAny 都不再被填，
// Expire 变 0，那条断言会自己变红（不是回归，是夹具/断言的时间耦合）。
func TestAuditDeltaIntlExpireGatedByNow(t *testing.T) {
	body := string(audFixture(t, "ent_usage-intl.json"))
	var doc struct {
		Packs []struct {
			Base struct {
				EndTime int64 `json:"end_time"`
			} `json:"entitlement_base_info"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("固件解析失败: %v", err)
	}
	end := doc.Packs[0].Base.EndTime
	t.Logf("固件 end_time=%d（%s）—— 断言的绝对时刻", end, time.Unix(end, 0).UTC().Format(time.RFC3339))

	c := entUsageClient(t, body, EpEntUsageIntl)
	u, err := c.UserEntUsageDetail(audIntlAuth())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u.Expire != end && end > time.Now().Unix() {
		t.Fatalf("行为已变：今天（%s）Expire=%d 期望 %d", time.Now().Format(time.RFC3339), u.Expire, end)
	}
	if end <= time.Now().Unix() && u.Expire != 0 {
		t.Fatalf("夹具已过期（%s），Expire 应为 0，实得 %d", time.Unix(end, 0).UTC().Format(time.RFC3339), u.Expire)
	}

	// 同一份报文，只把 end_time 换成过去 → Expire 立刻变 0（同一段代码、只是 now 不同）。
	past := strings.Replace(body, `"end_time":1793491199`, `"end_time":100`, -1)
	past = strings.Replace(past, `"expire_time":0`, `"expire_time":0`, 1)
	c2 := entUsageClient(t, past, EpEntUsageIntl)
	u2, err := c2.UserEntUsageDetail(audIntlAuth())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u2.Expire != 0 {
		t.Fatalf("行为已变：过期包仍返回到期时间 %d", u2.Expire)
	}
	t.Logf("end_time 改成过去 → Expire=%d（三条到期路径都被 e > now 挡住）；"+
		"所以「Expire == 1793491199」只在 2026-11-01 之前成立", u2.Expire)
}
