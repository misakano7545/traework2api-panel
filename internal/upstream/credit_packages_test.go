package upstream

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"traework2api/internal/auth"
)

// TestCreditPackagesParse 逐包构成解析：面额/已用/剩余/到期/发放，以及
// 「没有 credits_limit 的包（订阅权益）要被跳过」这条——它参与聚合会凭空多出余额。
func TestCreditPackagesParse(t *testing.T) {
	body := `{
	  "is_credits_billing": true,
	  "user_entitlement_pack_list": [
	    {"display_desc":"老用户福利","group_name":"用户福利","group_type":4,
	     "expire_time":1792968453,"source_id":"366218350082",
	     "entitlement_base_info":{"start_time":1790290053,"entitlement_id":"366218350082",
	       "quota":{"credits_limit":4000}},
	     "usage":{"credits_amount":183.7476}},
	    {"display_desc":"免费","expire_time":1793462399,
	     "entitlement_base_info":{"quota":{"enable_solo_coder":true}},
	     "usage":{}},
	    {"display_desc":"签到奖励","group_name":"每日签到","group_type":1,
	     "expire_time":1793629162,
	     "entitlement_base_info":{"start_time":1790950762,"quota":{"credits_limit":100}},
	     "usage":{"credits_amount":100}}
	  ]
	}`
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, EpEntUsage) {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		return jsonResp(200, body), nil
	})
	packs, remain, size, err := c.CreditPackages(&auth.Auth{UID: "u1"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(packs) != 2 {
		t.Fatalf("无面额的订阅包应被跳过，剩 2 个，实得 %d: %+v", len(packs), packs)
	}
	if packs[0].Name != "老用户福利" || packs[0].Group != "用户福利" || packs[0].Size != 4000 {
		t.Errorf("pack0=%+v", packs[0])
	}
	if packs[0].Used != 183 || packs[0].Remain != 3817 {
		t.Errorf("面额-已用 换算错: used=%d remain=%d", packs[0].Used, packs[0].Remain)
	}
	if packs[0].Expire != 1792968453 || packs[0].Start != 1790290053 {
		t.Errorf("时间字段丢了: %+v", packs[0])
	}
	if packs[1].Remain != 0 {
		t.Errorf("已用完的包剩余应钳到 0: %+v", packs[1])
	}
	if remain != 3817 || size != 4100 {
		t.Errorf("聚合 remain=%d size=%d，期望 3817/4100", remain, size)
	}
}

// TestCreditPackagesIntlUsesV1Path 国际版走 v1 路径 + 账号自己的计费网关（ApiHost），
// 并清空 `credits_limit=0` 的免费套餐（返回空列表而不是报错）。
// 这也是「国际版没显示出积分和到期日期」的回归网：旧实现对国际版直接返回 ErrNoIntlUG。
func TestCreditPackagesIntlUsesV1Path(t *testing.T) {
	body := `{"user_entitlement_pack_list":[
	  {"display_desc":"Free plan","expire_time":0,
	   "entitlement_base_info":{"start_time":1790812800,"end_time":1793491199,
	     "quota":{"credits_limit":0}},"usage":{}},
	  {"display_desc":"Pro plan","expire_time":0,
	   "entitlement_base_info":{"start_time":1790812800,"end_time":1793491199,
	     "quota":{"credits_limit":2000}},"usage":{"credits_amount":50}}
	]}`
	var gotPath, gotHost string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath, gotHost = r.URL.Path, r.URL.Host
		return jsonResp(200, body), nil
	})
	packs, remain, size, err := c.CreditPackages(&auth.Auth{UID: "u1", Domain: "trae.ai", ApiHost: "https://api-sg-central.trae.ai"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if gotPath != EpEntUsageIntl {
		t.Errorf("国际版应走 v1 路径，实得 %s", gotPath)
	}
	if gotHost != "api-sg-central.trae.ai" {
		t.Errorf("国际版应走账号的 ApiHost，实得 %s", gotHost)
	}
	if len(packs) != 1 || packs[0].Name != "Pro plan" {
		t.Fatalf("免费套餐应被跳过：%+v", packs)
	}
	if packs[0].Expire != 1793491199 {
		t.Errorf("国际版 expire_time=0，到期应回落 entitlement_base_info.end_time，实得 %d", packs[0].Expire)
	}
	if remain != 1950 || size != 2000 {
		t.Errorf("remain=%d size=%d，期望 1950/2000", remain, size)
	}
}

// UserEntUsage 的国际版同口径：到期时间同样回落 end_time（否则面板「到期」列恒空）。
func TestUserEntUsageIntlExpiryFallback(t *testing.T) {
	future := time.Now().Add(30 * 24 * time.Hour).Unix()
	body := `{"user_entitlement_pack_list":[
	  {"display_desc":"Pro plan","expire_time":0,
	   "entitlement_base_info":{"end_time":` + strconv.FormatInt(future, 10) + `,
	     "quota":{"credits_limit":500}},"usage":{"credits_amount":100}}
	]}`
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != EpEntUsageIntl {
			return nil, errors.New("国际版应走 v1 路径，实得 " + r.URL.Path)
		}
		return jsonResp(200, body), nil
	})
	remain, total, expire, err := c.UserEntUsage(&auth.Auth{UID: "u1", Domain: "trae.ai"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if remain != 400 || total != 500 {
		t.Errorf("remain=%d total=%d，期望 400/500", remain, total)
	}
	if expire != future {
		t.Errorf("到期时间应回落 end_time：got %d want %d", expire, future)
	}
}
