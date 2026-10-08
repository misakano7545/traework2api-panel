package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"traework2api/internal/auth"
)

func jsonI64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{200, `{"code":1005,"message":"plan limit","extra":{"plan":2}}`, ErrPlanLimit},
		{200, `{"code":1005,"msg":"权益不足"}`, ErrPlanLimit},
		{429, ``, ErrSoftRate},
		{401, `{"code":1001,"msg":"login required"}`, ErrSessionDead},
		{401, ``, ErrSessionDead},
		{404, ``, ErrNotFound},
		{500, `boom`, ErrServer},
		{503, `unavailable`, ErrServer},
		{400, `{"code":11101,"msg":"bad param"}`, ErrClient},
		{200, `{"checked_in":false}`, ErrNone},
		// 以下几条是「参考实现对拍」查出来的差距（2026-10-08）：HTTP 状态与 SSE 流内
		// 是同一套上游语义的两种到达方式，同一个码不许落进不同的池状态。
		{400, `{"code":4001,"msg":"We're sorry, the param is invalid"}`, ErrNone},
		{403, `{"code":4008}`, ErrSoftRate},
		{413, `too large`, ErrNone},
		{400, `{"msg":"prompt is too long for this model"}`, ErrNone},
		// 上面两条「输入过大」是调用方问题，罚号没有意义（同 4026 那条的教训）。
		{400, `{"code":4026,"message":"context length has exceeded the maximum limit"}`, ErrNone},
		// 4008 的额度文案不许被「输入过大」的宽松匹配抢走（含 "exceeded"，但不含任何
		// too-large 标记）——否则一个限流号会拿到免罚。
		{403, `{"code":4008,"msg":"Your requests have exceeded the quota"}`, ErrSoftRate},
		// 传输层信号优先于 body 里的业务码：500 里带 quota 文案仍按服务端故障处理。
		{500, `{"code":4008,"msg":"quota exceeded"}`, ErrServer},
		// 认不出的业务码 + 4xx：按状态归 Client，别落到「认不出就罚号」的流内兜底。
		{403, `{"code":99999}`, ErrClient},
		// Code 大写 + 字符串数字（换证接口那种信封）也要认得出。
		{400, `{"Code":"4001"}`, ErrNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testClient(fn rtFunc) *Client {
	return &Client{
		HTTP:      &http.Client{Transport: fn},
		AgentHost: "https://agent.example",
		UgHost:    "https://ug.example",
		OAuthHost: "https://oauth.example",
		ClientID:  ClientID,
	}
}

func TestRefreshTokenExchange(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, EpExchange) {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			return nil, errors.New("missing content-type")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"ClientID":"en1oxy7wnw8j9n"`)) || !bytes.Contains(body, []byte(`"RefreshToken":"oldrt"`)) {
			return nil, errors.New("bad body: " + string(body))
		}
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786805537,"TokenExpireDuration":1209600}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1, ApiHost: "https://oauth.example"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
	if a.ExpiresAt != 1786805537 {
		t.Errorf("expiresAt=%d", a.ExpiresAt)
	}
}

// TestRefreshTokenExchangeMilliseconds 覆盖上游 TokenExpireAt 返回毫秒的场景：
// 必须归一化为 Unix 秒后再写 auth.ExpiresAt。
func TestRefreshTokenExchangeMilliseconds(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786847930141,"TokenExpireDuration":1209600}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1, ApiHost: "https://oauth.example"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.ExpiresAt != 1786847930 {
		t.Errorf("expiresAt=%d want 1786847930 (毫秒转秒)", a.ExpiresAt)
	}
}

func TestRefreshTokenIfNeededSkipsFresh(t *testing.T) {
	calls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786847930141}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, ApiHost: "https://oauth.example"}
	refreshed, err := c.RefreshTokenIfNeeded(a, 24*3600*1e9)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed {
		t.Error("fresh token should not refresh")
	}
	if calls != 0 {
		t.Errorf("ExchangeToken should not be called, calls=%d", calls)
	}
	if a.AccessToken != "at" {
		t.Error("token should remain unchanged")
	}
}

func TestRefreshTokenIfNeededRefreshesExpired(t *testing.T) {
	calls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786847930141}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1, ApiHost: "https://oauth.example"}
	refreshed, err := c.RefreshTokenIfNeeded(a, 24*3600*1e9)
	if err != nil {
		t.Fatal(err)
	}
	if !refreshed || calls != 1 {
		t.Errorf("expired token should refresh once, refreshed=%v calls=%d", refreshed, calls)
	}
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
}

func TestRefreshTokenUsesAuthApiHost(t *testing.T) {
	var gotHost string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotHost = r.URL.Scheme + "://" + r.URL.Host
		return jsonResp(200, `{"Result":{"Token":"newat"}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1, ApiHost: "https://custom.example"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatal(err)
	}
	if gotHost != "https://custom.example" {
		t.Errorf("host=%s want auth.apiHost", gotHost)
	}
}

func TestChatStreamSendsHeadersAndRewritesBody(t *testing.T) {
	var gotAuth, gotUID, gotAppID, gotIdeVer string
	var gotBody []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-Uid")
		gotAppID = r.Header.Get("X-App-Id")
		gotIdeVer = r.Header.Get("X-Ide-Version")
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("event:done\ndata:{\"finish_reason\":\"stop\"}\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", MachineID: "m1", DeviceID: "d1"}
	rc, status, respBody, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	if respBody != nil {
		t.Errorf("200 response should carry nil body, got %q", respBody)
	}
	rc.Close()
	if gotAuth != "Cloud-IDE-JWT at" || gotUID != "u1" {
		t.Errorf("headers: auth=%q uid=%q", gotAuth, gotUID)
	}
	if gotAppID != AppID || gotIdeVer != IdeVersion {
		t.Errorf("app headers: appid=%q idever=%q", gotAppID, gotIdeVer)
	}
	if !bytes.Contains(gotBody, []byte(`"stream":true`)) || !bytes.Contains(gotBody, []byte(`"function":"solo_work_lite"`)) {
		t.Errorf("body not rewritten: %s", gotBody)
	}
}

func TestChatStreamUsesDedicatedStreamClient(t *testing.T) {
	// StreamHTTP 优先于 HTTP 被 ChatStream 使用（无总超时的长 SSE 流客户端）。
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("event:done\ndata:{\"finish_reason\":\"stop\"}\n\n")),
		}, nil
	})
	c.StreamHTTP = &http.Client{Transport: c.HTTP.Transport} // 无 Timeout
	rc, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{"model":"glm-5.2","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	if c.StreamHTTP.Timeout != 0 {
		t.Errorf("stream client should have no total timeout, got %v", c.StreamHTTP.Timeout)
	}
}

func TestChatStreamHTTPError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(429, `rate limited`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	_, status, respBody, err := c.ChatStream(a, []byte(`{}`))
	if status != 429 {
		t.Errorf("status=%d", status)
	}
	if err != nil {
		t.Fatalf("429 should come via status, err=%v", err)
	}
	if Classify(status, string(respBody)) != ErrSoftRate {
		t.Errorf("not classified soft rate: %q", respBody)
	}
}

func TestUserEntUsageAggregation(t *testing.T) {
	future := time.Now().Unix() + 3600
	past := time.Now().Unix() - 3600
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, EpEntUsage) {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT at" {
			return nil, errors.New("missing auth header")
		}
		return jsonResp(200, `{"is_credits_billing":true,"user_entitlement_pack_list":[
			{"entitlement_base_info":{"quota":{"credits_limit":2000}},"usage":{"credits_amount":150.9},"expire_time":`+jsonI64(future+7200)+`},
			{"entitlement_base_info":{"quota":{"credits_limit":500}},"usage":{"credits_amount":20},"expire_time":`+jsonI64(future)+`},
			{"entitlement_base_info":{"quota":{"credits_limit":0}},"usage":{"credits_amount":10},"expire_time":`+jsonI64(past)+`},
			{"entitlement_base_info":{"quota":{"credits_limit":100}},"usage":{"credits_amount":0},"expire_time":`+jsonI64(past)+`}
		]}`), nil
	})
	remain, total, expire, err := c.UserEntUsage(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("ent usage: %v", err)
	}
	// 1850 + 480 + 100；0 上限的包不计。150.9 截断为 150，与 cmd/credit 一致。
	if remain != 2430 {
		t.Errorf("remain=%d want 2430", remain)
	}
	// 总额 = 各包 credits_limit 之和（0 上限的包不计），做「剩余/总额」的分母。
	if total != 2600 {
		t.Errorf("total=%d want 2600", total)
	}
	// 最早到期取 expire_time > now 的最小值；已过期的包不参与。
	if expire != future {
		t.Errorf("expire=%d want %d（已过期的包不参与紧迫度）", expire, future)
	}
}

func TestCheckinClaimRejectsBusinessError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":9074,"message":"当前参与用户太多，请稍后再试"}`), nil
	})
	err := c.CheckinClaim(&auth.Auth{AccessToken: "at"})
	if err == nil || !strings.Contains(err.Error(), "当前参与用户太多") {
		t.Fatalf("err=%v", err)
	}
}

func TestCheckinStatusAndClaim(t *testing.T) {
	var path string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		path = r.URL.Path
		if r.Header.Get("X-User-Region") != "CN" {
			return nil, errors.New("missing X-User-Region")
		}
		return jsonResp(200, `{"checked_in":false,"credits":150,"extra_credits":50,"enable":true}`), nil
	})
	checkedIn, credits, enable, err := c.CheckinStatus(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatal(err)
	}
	// credits 是「本次可领」= credits + extra_credits（免费档 150+50）。
	if checkedIn || !enable || credits != 200 {
		t.Errorf("status: checked=%v enable=%v credits=%d", checkedIn, enable, credits)
	}
	if path != EpCheckinStatus {
		t.Errorf("path=%s", path)
	}
}

// 官方模型口径：只有「用户可见 + chat_completion」留下。
// 上游全量表实测 39（版本码 20260716）/ 42（20260811）条，另外三类必须被挡掉：
// invisible 子代理、custom_model_* 槽位、summary。可见集合随版本码漂移，故不做型号名单。
func TestPickOfficialModels(t *testing.T) {
	const payload = `{"config_info_list":[
		{"config_name":"glm-5.2","is_invisible_to_user":false,"usage":"chat_completion",
		 "display_config":{"display_name":"GLM-5.2","model_capability":"reasoning_model"},
		 "context_window_tokens":{"dev":200000},
		 "model_detail_list":[{"max_tokens":32000,"model_extra_config":"{\"Thinking\":{\"Type\":\"enabled\"}}"}]},
		{"config_name":"Doubao-Seed-Evolving","is_invisible_to_user":false,"usage":"chat_completion",
		 "display_config":{"model_capability":"reasoning_model"},
		 "reasoning_effort_config":{"support_thinking":false},
		 "context_window_tokens":{"dev":256000},"model_detail_list":[{"max_tokens":32000}]},
		{"config_name":"kimi-k3","is_invisible_to_user":false,"usage":"chat_completion",
		 "display_config":{"display_name":"Kimi-K3","model_capability":"reasoning_model"},
		 "context_window_tokens":{"dev":200000},"model_detail_list":[{"max_tokens":32000}]},
		{"config_name":"browser_use_subagent","is_invisible_to_user":true,"usage":"chat_completion"},
		{"config_name":"glm-5-turbo","is_invisible_to_user":true,"usage":"chat_completion",
		 "display_config":{"display_name":"GLM-5-Turbo","model_capability":"reasoning_model"}},
		{"config_name":"custom_model_kimi","is_invisible_to_user":false,"usage":"custom_model"},
		{"config_name":"summary","is_invisible_to_user":false,"usage":"summary"},
		{"config_name":"","is_invisible_to_user":false,"usage":"chat_completion"}
	]}`
	var resp struct {
		ConfigInfoList []paramConfig `json:"config_info_list"`
	}
	if err := json.Unmarshal([]byte(payload), &resp); err != nil {
		t.Fatal(err)
	}
	got := pickOfficialModels(resp.ConfigInfoList, &auth.Auth{Domain: "trae.cn"}, Function)
	if len(got) != 3 {
		t.Fatalf("官方模型数=%d want 3: %+v", len(got), got)
	}
	if got[0].ID != "glm-5.2" || got[1].ID != "Doubao-Seed-Evolving" || got[2].ID != "kimi-k3" {
		t.Errorf("顺序/内容错: %+v", got)
	}
	if got[0].Name != "GLM-5.2" || got[0].ContextWindow != 200000 || got[0].MaxTokens != 32000 {
		t.Errorf("字段未填充: %+v", got[0])
	}

	// 思考相关三项按上游原样搬运（面板「能力 / 思考」列直接用）。
	if got[0].Capability != "reasoning_model" || got[0].Thinking != "enabled" || got[0].Effort != "" {
		t.Errorf("glm-5.2 思考字段: %+v", got[0])
	}
	if got[1].Capability != "reasoning_model" || got[1].Thinking != "" ||
		got[1].Effort != `{"support_thinking":false}` {
		t.Errorf("Doubao 思考字段: %+v", got[1])
	}
	// 解析不出来就当"上游没给"，不猜、不报错。
	if t2 := thinkingType(`{坏 JSON`); t2 != "" {
		t.Errorf("坏 JSON 应返回空: %q", t2)
	}
	if t3 := thinkingType(`{"Thinking":{"Type":"disabled"}}`); t3 != "disabled" {
		t.Errorf("thinkingType=%q", t3)
	}

	// 全被挡掉时必须返回空，让上层回退静态表，而不是回一张空清单。
	if only := pickOfficialModels([]paramConfig{{ConfigName: "summary", Usage: "summary"}}, nil, Function); len(only) != 0 {
		t.Errorf("应返回空: %+v", only)
	}

	// 国际版（realm=intl）：官方模型全被上游标成 is_invisible_to_user=true 却照样能调，
	// 按「有显示名」放行（glm-5-turbo），没显示名的内部子代理（browser_use_subagent）
	// 与 custom_model_* / summary 仍要挡掉。
	intl := &auth.Auth{Domain: "trae.ai"}
	gotIntl := pickOfficialModels(resp.ConfigInfoList, intl, Function)
	ids := make([]string, 0, len(gotIntl))
	for _, m := range gotIntl {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "glm-5.2,Doubao-Seed-Evolving,kimi-k3,glm-5-turbo" {
		t.Fatalf("国际版模型表=%v want [glm-5.2 Doubao-Seed-Evolving kimi-k3 glm-5-turbo]", ids)
	}
}

// 通道冻结：这条中继转发的是 **TRAE Work**（原 TRAE SOLO）的对话通道，不是 TRAE IDE。
//
// 为什么断言字面值而不是常量：常量会被一起改掉，测试就跟着"通过"了。这里钉的是
// **实际上线的值**——function / AppID / 主机 / 版本码 任意一项漂到 IDE 口径，模型表、
// 门控（k3 那次就是版本码决定的）、计费归属都会跟着变，必须在这条测试上先响。
func TestWorkChannelIdentityFrozen(t *testing.T) {
	const (
		wantFunction  = "solo_work_lite"
		wantAgentHost = "https://trae-api-cn.mchost.guru"
		wantUgHost    = "https://api.trae.cn"
		wantChatPath  = "/api/agent/v3/llm_utils_chat"
		wantModelPath = "/api/ide/v1/get_detail_param"
		wantAppID     = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
		wantClientID  = "en1oxy7wnw8j9n"
		wantVerCode   = "20260811"
	)
	if Function != wantFunction {
		t.Errorf("function=%q want %q（换掉就是另一条产品线）", Function, wantFunction)
	}
	if AgentHost != wantAgentHost || UgHost != wantUgHost {
		t.Errorf("hosts: agent=%q ug=%q", AgentHost, UgHost)
	}
	if EpChat != wantChatPath || EpModels != wantModelPath {
		t.Errorf("paths: chat=%q models=%q", EpChat, EpModels)
	}
	if AppID != wantAppID || ClientID != wantClientID {
		t.Errorf("appid=%q clientid=%q（都应是 SOLO 的）", AppID, ClientID)
	}
	if IdeVersionCode != wantVerCode {
		t.Errorf("版本码=%q：它是上游的放量开关，往下调 k3 等模型会重新被门控", IdeVersionCode)
	}

	// 真发一次，抓出站头与请求体。
	var gotAppID, gotVerCode, gotVerType, gotTraffic string
	var gotBody []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotAppID = r.Header.Get("X-App-Id")
		gotVerCode = r.Header.Get("X-Ide-Version-Code")
		gotVerType = r.Header.Get("X-Ide-Version-Type")
		gotTraffic = r.Header.Get("Request-Traffic-Type")
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("event:done\ndata:{\"finish_reason\":\"stop\"}\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", MachineID: "m1", DeviceID: "d1"}
	rc, _, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if gotAppID != wantAppID || gotVerCode != wantVerCode || gotVerType != "stable" || gotTraffic != "prod" {
		t.Errorf("出站头: appid=%q verCode=%q type=%q traffic=%q", gotAppID, gotVerCode, gotVerType, gotTraffic)
	}
	if !bytes.Contains(gotBody, []byte(`"function":"solo_work_lite"`)) {
		t.Errorf("请求体 function 不是 Work 通道: %s", gotBody)
	}

	// 运行期覆盖（面板 upstream.user_agent / client_version）只能改 UA 与版本号字符串
	// **不能**改产品线：版本码与 AppID 必须纹丝不动。
	defer SetIdentity("", "")
	SetIdentity("TraeClient/TTNet", "9.9.9")
	var hdr http.Header
	c2 := testClient(func(r *http.Request) (*http.Response, error) {
		hdr = r.Header.Clone()
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("event:done\ndata:{\"finish_reason\":\"stop\"}\n\n")),
		}, nil
	})
	rc2, _, _, err := c2.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	rc2.Close()
	if hdr.Get("X-Ide-Version-Code") != wantVerCode || hdr.Get("X-App-Id") != wantAppID {
		t.Errorf("运行期覆盖动了产品线关键头: verCode=%q appid=%q",
			hdr.Get("X-Ide-Version-Code"), hdr.Get("X-App-Id"))
	}
	if hdr.Get("X-Ide-Version") != "9.9.9" || hdr.Get("User-Agent") != "TraeClient/TTNet" {
		t.Errorf("该生效的覆盖没生效: ver=%q ua=%q", hdr.Get("X-Ide-Version"), hdr.Get("User-Agent"))
	}
}
