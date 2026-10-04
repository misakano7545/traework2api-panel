package upstream

import (
	"net/http"
	"testing"

	"traework2api/internal/auth"
)

// TestEntUsageEmptyPackListIsError 空包列表是接口异常（未登录/风控/结构变了），不是
// 「剩余 0 分」：当成 0 分会把好号的积分静默清零并影响选号权重。
func TestEntUsageEmptyPackListIsError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"is_credits_billing":true,"user_entitlement_pack_list":[]}`), nil
	})
	remain, total, expire, err := c.UserEntUsage(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatalf("空包列表应报错，实得 remain=%d total=%d expire=%d", remain, total, expire)
	}
	if remain != 0 || total != 0 || expire != 0 {
		t.Errorf("报错时不该回半截数字：%d/%d/%d", remain, total, expire)
	}
	// 非空则照常解析（别把守卫写成「有包也报错」）。
	c2 := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":500}},"usage":{"credits_amount":120}}]}`), nil
	})
	if remain, total, _, err := c2.UserEntUsage(&auth.Auth{AccessToken: "at"}); err != nil || remain != 380 || total != 500 {
		t.Fatalf("正常包列表解析错：remain=%d total=%d err=%v", remain, total, err)
	}
}
