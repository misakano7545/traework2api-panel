package upstream

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"traework2api/internal/auth"
)

// TestCheckinStatusRejectsErrorEnvelope 上游的错误信封（典型是限流 9074）不带
// checked_in/enable：不校验就会被读成 enable=false，调度器当成「上游把这个号的签到关了」
// 跳过（skipped，不冷却不报警），限流被伪装成正常状态。
func TestCheckinStatusRejectsErrorEnvelope(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":9074,"message":"当前参与用户太多，请稍后再试"}`), nil
	})
	checkedIn, _, enable, err := c.CheckinStatus(&auth.Auth{AccessToken: "at"})
	var be *BusinessError
	if !errors.As(err, &be) || be.Code != 9074 {
		t.Fatalf("限流信封应报 BusinessError(9074)，实得 checked=%v enable=%v err=%v", checkedIn, enable, err)
	}
	if !strings.Contains(err.Error(), "当前参与用户太多") {
		t.Errorf("错误信息应带上游原文：%v", err)
	}
	if enable {
		t.Error("出错时 enable 不该为 true")
	}
}

// TestCheckinStatusRejectsSuccessFalse 有的响应不带 code、只用 success=false 表示失败。
func TestCheckinStatusRejectsSuccessFalse(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"success":false,"message":"签到功能维护中"}`), nil
	})
	if _, _, _, err := c.CheckinStatus(&auth.Auth{AccessToken: "at"}); err == nil {
		t.Fatal("success=false 必须报错，否则会被当成 enable=false 静默跳过")
	}
}

// TestCheckinClaimAlreadyIsNotFailure 9095「今日已签到」是幂等回执：与 status 的竞态会走到
// 这里，当成失败会让面板把「已经签上了」显示成红字失败。
func TestCheckinClaimAlreadyIsNotFailure(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":9095,"message":"今日已签到"}`), nil
	})
	err := c.CheckinClaim(&auth.Auth{AccessToken: "at"})
	if !errors.Is(err, ErrCheckinAlready) {
		t.Fatalf("9095 应回 ErrCheckinAlready，实得 %v", err)
	}
}

// TestCheckinClaimSuccessFalseIsFailure code=0 但 success=false 仍是失败（不能只看 code）。
func TestCheckinClaimSuccessFalseIsFailure(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"success":false,"msg":"稍后再试"}`), nil
	})
	err := c.CheckinClaim(&auth.Auth{AccessToken: "at"})
	if err == nil || errors.Is(err, ErrCheckinAlready) {
		t.Fatalf("success=false 应报失败，实得 %v", err)
	}
	if !strings.Contains(err.Error(), "稍后再试") {
		t.Errorf("错误信息应带 msg 原文：%v", err)
	}
}
