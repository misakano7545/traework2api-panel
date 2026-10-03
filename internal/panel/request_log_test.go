package panel

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"traework2api/internal/pool"
	"traework2api/internal/reqlog"
)

// TestRequestLogsFilterAndFallback 请求记录的筛选与回落口径。
//
// 两条不变量：
//  1. 筛选在服务端做（带 outcome/model/client_ip/from/to 时只回命中的条目）；
//  2. 「筛完没有命中」是一个真实结果，**不能**回落成内存里的全部最近事件。
//     回归网：原先用 len(entries)>0 判断「这次是从归档取的」，归档关闭时恒为 false ——
//     一旦带上区间筛选而区间内没有记录，就会把筛选条件之外、时段之外的请求全显示出来。
func TestRequestLogsFilterAndFallback(t *testing.T) {
	rec := reqlog.New(reqlog.Config{})
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)
	rec.Record(reqlog.Event{
		Time: base, RequestID: "r1", Path: "/v1/chat/completions", Model: "cn:glm-5.2",
		Account: "u1(xxxxxxxx)", Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess,
		DurationMs: 1200, PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120,
		Credit: 1.5, HasCredit: true, ClientIP: "1.1.1.1", UserAgent: "curl/8",
	})
	rec.Record(reqlog.Event{
		Time: base.Add(time.Hour), RequestID: "r2", Path: "/v1/responses", Model: "cn:kimi-k3",
		Account: "u2(yyyyyyyy)", Status: 502, Outcome: reqlog.OutcomeHTTPError,
		ClientIP: "2.2.2.2", UserAgent: "python-requests/2",
	})
	rec.Record(reqlog.Event{
		Time: base.Add(2 * time.Hour), RequestID: "r3", Path: "/v1/messages", Model: "cn:glm-5.2",
		Account: "u1(xxxxxxxx)", Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess,
		ClientIP: "3.3.3.3",
	})

	p := New(Config{Pool: pool.New(""), APIKey: "test-key", Version: "test", RequestLog: rec})

	type resp struct {
		Entries  []reqlog.Event `json:"entries"`
		Limit    int            `json:"limit"`
		Archived bool           `json:"archived"`
		Metrics  struct {
			Completed int `json:"completed"`
		} `json:"metrics"`
	}
	get := func(query string) resp {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/panel/api/request_logs"+query, nil)
		r.Header.Set("Authorization", "Bearer test-key")
		p.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("GET %s → %d %s", query, w.Code, w.Body.String())
		}
		var out resp
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("GET %s 解析失败: %v", query, err)
		}
		return out
	}
	ids := func(rs []reqlog.Event) string {
		s := ""
		for _, e := range rs {
			s += e.RequestID + ","
		}
		return s
	}

	if got := get(""); len(got.Entries) != 3 || got.Archived {
		t.Errorf("无筛选应回 3 条且 archived=false，实得 %d 条 archived=%v (%s)", len(got.Entries), got.Archived, ids(got.Entries))
	}
	if got := get("?outcome=http_error"); ids(got.Entries) != "r2," {
		t.Errorf("outcome 筛选应只留 r2，实得 %s", ids(got.Entries))
	}
	// 内存里的最近事件是倒序（最近在前），与归档读盘同序 —— 期望串也按倒序写。
	if got := get("?model=glm"); ids(got.Entries) != "r3,r1," {
		t.Errorf("model 包含匹配应留 r3,r1（倒序），实得 %s", ids(got.Entries))
	}
	if got := get("?client_ip=2.2.2.2"); ids(got.Entries) != "r2," {
		t.Errorf("client_ip 筛选应只留 r2，实得 %s", ids(got.Entries))
	}
	if got := get("?account=u2"); ids(got.Entries) != "r2," {
		t.Errorf("account 筛选应只留 r2，实得 %s", ids(got.Entries))
	}
	// 区间：12:30–13:30 只覆盖 r2（13:00；r3 在 14:00 之外）。
	from := base.Add(30 * time.Minute).Unix()
	to := base.Add(90 * time.Minute).Unix()
	if got := get("?from=" + itoa(from) + "&to=" + itoa(to)); ids(got.Entries) != "r2," {
		t.Errorf("区间筛选应只留 r2，实得 %s", ids(got.Entries))
	}
	// 关键回归：区间落在未来（筛完一条不剩）→ 回空，而不是内存里的全部 3 条。
	future := base.Add(240 * time.Hour).Unix()
	if got := get("?from=" + itoa(future)); len(got.Entries) != 0 {
		t.Errorf("区间内无记录应回 0 条（不是回落成全部），实得 %d 条: %s", len(got.Entries), ids(got.Entries))
	}
	// limit 生效（取最近 N 条：内存里是倒序，最近的两条 = r3,r2）。
	if got := get("?limit=2"); len(got.Entries) != 2 {
		t.Errorf("limit=2 应回 2 条，实得 %d 条", len(got.Entries))
	}
	if got := get("?limit=2"); got.Limit != 2 {
		t.Errorf("limit 回显应为 2，实得 %d", got.Limit)
	}
	// 指标与条目在同一响应里（面板一次请求拿两样，不必再打 request_metrics）。
	if got := get(""); got.Metrics.Completed != 3 {
		t.Errorf("metrics.completed 应为 3，实得 %d", got.Metrics.Completed)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
