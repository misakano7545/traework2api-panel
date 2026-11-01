package upstream

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"traework2api/internal/auth"
)

// ent_usage_unit_test.go 额度口径的回归网。
//
// 夹具是**真实报文**（2026-10-15 用真 token 抓，只把 uid/订单号这类 12 位以上数字
// 掩成 1000…0，计费字段原样保留）：
//
//	testdata/ent_usage-cn.json    国内账号：is_dollar_usage_billing=false、走 credits_limit
//	testdata/ent_usage-intl.json  国际账号：is_dollar_usage_billing=true、走 basic_usage_limit
//
// 这两个文件就是这次改动的全部依据：国际版不是「免费无限」，是 Free plan 给了
// basic_usage_limit=1 即 $1 额度，已用 basic_usage_amount=0.00059。

func entUsageFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("读夹具失败: %v", err)
	}
	return string(raw)
}

// entUsageClient 打桩：额度接口返回给定报文，并校验走的是哪条路径。
// 直接返回 *Client（不是接口），调用方只用到上面那几个方法。
func entUsageClient(t *testing.T, body string, wantPath string) *Client {
	t.Helper()
	return testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != wantPath {
			return nil, errors.New("额度接口路径不对: " + r.URL.Path)
		}
		return jsonResp(200, body), nil
	})
}

// TestEntUsageSummaryFallback packs 一个面额都不带时（免费包只有布尔开关），用顶级
// usage_summary 兜底——实测它 {total_amount, consumed_amount} 与 Σpacks 逐位吻合，是同一口径的另一条路。
func TestEntUsageSummaryFallback(t *testing.T) {
	// 18 个免费包、全无 credits_limit；summary 才是唯一有数的口径。
	body := `{"is_dollar_usage_billing":false,
	  "usage_summary":{"total_amount":6000,"consumed_amount":434.19},
	  "user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"is_free":true}},"usage":{"credits_amount":0}}]}`
	c := entUsageClient(t, body, EpEntUsage)
	u, err := c.UserEntUsageDetail(&auth.Auth{UID: "u1"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u.Total != 6000 || u.Remain != 5566 {
		t.Errorf("兜底口径 remain/total = %d/%d，期望 5566/6000", u.Remain, u.Total)
	}
	// 有面额时不许被 summary 覆盖（宁可少算也不要两种口径混着来）。
	withPack := `{"is_dollar_usage_billing":false,
	  "usage_summary":{"total_amount":6000,"consumed_amount":434.19},
	  "user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":100}},"usage":{"credits_amount":10}}]}`
	u2, err := entUsageClient(t, withPack, EpEntUsage).UserEntUsageDetail(&auth.Auth{UID: "u1"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u2.Total != 100 || u2.Remain != 90 {
		t.Errorf("有面额时不该走兜底: remain/total = %d/%d，期望 90/100", u2.Remain, u2.Total)
	}
}

// TestEntUsageUnitFromRealFixtures 两个地区的真实报文都要被正确判口径。
func TestEntUsageUnitFromRealFixtures(t *testing.T) {
	t.Run("国内=积分口径", func(t *testing.T) {
		c := entUsageClient(t, entUsageFixture(t, "ent_usage-cn.json"), EpEntUsage)
		u, err := c.UserEntUsageDetail(&auth.Auth{UID: "u1"})
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if u.Unit != UnitCredit {
			t.Fatalf("国内版 Unit=%q，期望 %q", u.Unit, UnitCredit)
		}
		// 夹具实测：16 个有面额的包、总面额 5900、剩余 5466（另一条「免费」包无面额要跳过）
		if u.Total != 5900 || u.Remain != 5466 {
			t.Errorf("积分口径 remain/total = %d/%d，期望 5466/5900", u.Remain, u.Total)
		}
		if u.LimitUSD != 0 || u.UsedUSD != 0 {
			t.Errorf("国内报文不该填美元字段: limit=%v used=%v", u.LimitUSD, u.UsedUSD)
		}
	})

	t.Run("国际=美元口径", func(t *testing.T) {
		body := entUsageFixture(t, "ent_usage-intl.json")
		// 夹具是上游原始紧凑 JSON（无空格），所以只查键在不在，值交给下面的解析断言。
		if !strings.Contains(body, `"basic_usage_limit"`) {
			t.Fatal("夹具变了：没找到 basic_usage_limit（$1 免费额度）")
		}
		if !strings.Contains(body, `"is_dollar_usage_billing":true`) {
			t.Fatal("夹具变了：国际报文应带 is_dollar_usage_billing=true")
		}
		// 国际版本地夹具里的 api host 无所谓，路径必须是 v1
		c := entUsageClient(t, body, EpEntUsageIntl)
		a := &auth.Auth{UID: "u1", Domain: "trae.ai", ApiHost: "https://api-sg-central.trae.ai"}
		u, err := c.UserEntUsageDetail(a)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if u.Unit != UnitUSD {
			t.Fatalf("国际版 Unit=%q，期望 %q（is_dollar_usage_billing=true）", u.Unit, UnitUSD)
		}
		if u.LimitUSD != 1 {
			t.Errorf("额度 LimitUSD=%v，期望 1（$1 免费额度）", u.LimitUSD)
		}
		if u.UsedUSD != 0.00059 {
			t.Errorf("已用 UsedUSD=%v，期望 0.00059", u.UsedUSD)
		}
		if u.Total != 0 || u.Remain != 0 {
			t.Errorf("美元报文不该填积分字段: remain=%d total=%d", u.Remain, u.Total)
		}
		// 小数不能被截断成 0 —— 这是「面板显示成一分没用」的根因
		if u.UsedUSD == 0 {
			t.Error("已用金额被截断成 0：美元口径必须保留小数")
		}
		if got := u.RemainUSD(); got < 0.99 || got > 1 {
			t.Errorf("剩余美元=%v，期望约 0.99941", got)
		}
	})
}

// TestIntlFixtureUnreadableAsCredits 这是本次改动的**根因回归网**。
//
// 同一份国际报文，老接口（只读 credits_limit / credits_amount）拿到的是 0/0 ——
// 面板据此显示「0 积分 / 无额度套餐」，看着像接口坏了或号废了；
// 而上游真实状态是 $1 额度里用了 $0.00059。
func TestIntlFixtureUnreadableAsCredits(t *testing.T) {
	body := entUsageFixture(t, "ent_usage-intl.json")
	c := entUsageClient(t, body, EpEntUsageIntl)
	a := &auth.Auth{UID: "u1", Domain: "trae.ai", ApiHost: "https://api-sg-central.trae.ai"}

	remain, total, _, err := c.UserEntUsage(a)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if remain != 0 || total != 0 {
		t.Fatalf("这条是「老口径读不到东西」的实证：期望 0/0，实得 %d/%d", remain, total)
	}
	// 老口径的包列表也必须为空（credits_limit=0 被跳过），这就是「无积分套餐」的来源。
	packs, _, _, err := c.CreditPackages(a)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(packs) != 0 {
		t.Fatalf("积分口径应看不到任何包，实得 %d 个", len(packs))
	}

	// 换成口径感知的接口：同样一份报文，必须拿得到美元额度与那个包。
	u, err := c.UserEntUsageDetail(a)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u.Unit != UnitUSD || u.LimitUSD != 1 {
		t.Fatalf("口径感知接口应给出 usd/$1，实得 unit=%q limit=%v", u.Unit, u.LimitUSD)
	}
	usdPacks, pu, err := c.BalancePackages(a)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(usdPacks) != 1 {
		t.Fatalf("美元口径应看到 1 个包（Free plan），实得 %d 个", len(usdPacks))
	}
	p := usdPacks[0]
	if p.Unit != UnitUSD || p.Name != "Free plan" {
		t.Errorf("包口径/名字不对: unit=%q name=%q", p.Unit, p.Name)
	}
	if p.SizeUSD != 1 || p.UsedUSD != 0.00059 {
		t.Errorf("包金额不对: limit=%v used=%v", p.SizeUSD, p.UsedUSD)
	}
	if p.RemainUSD <= 0.99 || p.RemainUSD > 1 {
		t.Errorf("包剩余不对: %v", p.RemainUSD)
	}
	if pu.LimitUSD != 1 || pu.UsedUSD != 0.00059 {
		t.Errorf("包接口的汇总与明细不一致: limit=%v used=%v", pu.LimitUSD, pu.UsedUSD)
	}
	// 到期时间两口径都要有（国际版 expire_time=0，靠 end_time 回落）。但两者契约不同：
	//   u.Expire 带 `e > now` 门槛——过期就不报（0）；
	//   p.Expire 是包的原始截止时刻，过期后仍原样给出（面板据 diff<=0 渲染「已到期」，
	//            credit_packages_test.go 也钉着它等于夹具 end_time）。
	// 所以只有 u 期望值随 now 变，写成两者同一个 now 函数会让断言在夹具到期那天自己变红。
	const fixtureEnd = 1793491199
	wantExpire := int64(0)
	if fixtureEnd > time.Now().Unix() {
		wantExpire = fixtureEnd
	}
	if u.Expire != wantExpire {
		t.Errorf("汇总到期应回落 end_time=%d（未过期）/ 0（已过期），实得 %d", wantExpire, u.Expire)
	}
	if p.Expire != fixtureEnd {
		t.Errorf("包的到期应恒等于 end_time=%d（过期也原样报，面板渲染「已到期」），实得 %d", fixtureEnd, p.Expire)
	}
}

// TestBalancePackagesCNUnchanged 国内口径与改动前逐字一致（零回归）。
func TestBalancePackagesCNUnchanged(t *testing.T) {
	body := entUsageFixture(t, "ent_usage-cn.json")
	c := entUsageClient(t, body, EpEntUsage)
	a := &auth.Auth{UID: "u1"}

	packs, u, err := c.BalancePackages(a)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(packs) != 16 {
		t.Fatalf("国内夹具应有 16 个有面额的包（第 17 条「免费」无面额要跳过），实得 %d", len(packs))
	}
	for _, p := range packs {
		if p.Unit != UnitCredit {
			t.Fatalf("国内包口径应为 credit，实得 %q（%s）", p.Unit, p.Name)
		}
		if p.SizeUSD != 0 || p.UsedUSD != 0 || p.RemainUSD != 0 {
			t.Fatalf("国内包不该填美元字段: %+v", p)
		}
	}
	// 老接口结果不变
	oldPacks, remain, size, err := c.CreditPackages(a)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(oldPacks) != len(packs) || remain != 5466 || size != 5900 {
		t.Errorf("老接口结果变了: 包数 %d→%d, remain/size=%d/%d", len(packs), len(oldPacks), remain, size)
	}
	if u.Remain != remain || u.Total != size {
		t.Errorf("新老接口对国内号的汇总必须一致: %d/%d vs %d/%d", u.Remain, u.Total, remain, size)
	}
}

// TestEntUsageUnitDefaultsToCredit 没有 is_dollar_usage_billing 标记的报文按积分处理。
//
// 这是**故意**的保守取向：国内版报文里根本没有 basic_usage_limit 这个键（实测为缺失），
// 「缺键」与「值为 0」在 JSON 里分不开，所以只能靠顶层标记判口径；
// 标记缺失时按积分走，老数据/老上游零回归。
func TestEntUsageUnitDefaultsToCredit(t *testing.T) {
	body := `{"user_entitlement_pack_list":[
	  {"display_desc":"老用户福利","entitlement_base_info":{"quota":{"credits_limit":100}},
	   "usage":{"credits_amount":40}},
	  {"display_desc":"Free plan","entitlement_base_info":{"quota":{"basic_usage_limit":1}},
	   "usage":{"basic_usage_amount":0.5}}
	]}`
	c := entUsageClient(t, body, EpEntUsage)
	u, err := c.UserEntUsageDetail(&auth.Auth{UID: "u1"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u.Unit != UnitCredit {
		t.Fatalf("无标记时应按积分处理，实得 %q", u.Unit)
	}
	if u.Remain != 60 || u.Total != 100 {
		t.Errorf("remain/total=%d/%d，期望 60/100", u.Remain, u.Total)
	}
	// basic_usage_limit 在积分口径下必须被忽略，不能被当成积分混进来
	if u.LimitUSD != 0 {
		t.Errorf("积分口径不该读美元字段: %v", u.LimitUSD)
	}
}
