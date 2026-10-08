package upstream

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"traework2api/internal/auth"
)

// 对拍探针：把参考实现（shadyrispy/cpa-multi-plugins 的 plugins/trae，AGPL 之外
// 的独立实现）生成的**答案卷**喂进本仓实现，逐条打印一致 / 不一致。
//
// 答案卷不进仓（那是别人的产物，口径还只覆盖它自己那套网关），只在自己手上时跑：
//
//	TW2A_VECTORS=/path/to/trae/vectors go test ./internal/upstream -run TestVectorsCrossCheck -v
//
// 2026-10-08 对拍结论（答案卷 v0.12.95-10-gb1a3b6a）：
//   - headers：chat 路 18 个头**键集逐键一致**（当时只有版本号/版本码不同，我们按自己的
//     探针钉 20260811）→ 已固化成 TestSOLOHeadersKeySet；
//   - payload：除我们有意的分歧（不发 config_name、max_tokens 上限、未知字段透传、
//     原样保留 reasoning_effort）外，形状规则全对；
//   - aggregate：除我们有意的分歧（空流报错好换号、流内错误按 ErrKind 上报）外全对；
//   - classify：4 条不一致 —— 403/4008、400/4001、413「too large」、400「prompt is too
//     long」参考实现归成 plan_limit / model_unavailable / input_too_large（调用方问题），
//     我们归 ErrClient → NoteDegrade（降权）。**这是待决项**：给调用方的超长请求降权，
//     与 4026 那条「上下文超长不许罚号」是同一类错误，见 context_overflow_test.go。
func TestVectorsCrossCheck(t *testing.T) {
	dir := os.Getenv("TW2A_VECTORS")
	if dir == "" {
		t.Skip("TW2A_VECTORS 未设置（指向 trae-vectors.json 所在目录）")
	}
	raw, err := os.ReadFile(strings.TrimRight(dir, "/") + "/trae-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vec map[string]any
	if err := json.Unmarshal(raw, &vec); err != nil {
		t.Fatal(err)
	}

	t.Run("classify", func(t *testing.T) {
		rows, _ := vec["classify"].([]any)
		for _, r := range rows {
			m := r.(map[string]any)
			status := int(m["status"].(float64))
			body, want := m["body"].(string), m["kind"].(string)
			if got := errKindName(Classify(status, body)); got != want {
				t.Errorf("status=%d body=%q 参考=%s 本仓=%s", status, body, want, got)
			}
		}
	})

	t.Run("headers", func(t *testing.T) {
		for _, key := range []string{"headers", "ugHeaders"} {
			rows, _ := vec[key].([]any)
			for _, r := range rows {
				m := r.(map[string]any)
				want, _ := m["output"].(map[string]any)
				req, _ := http.NewRequest(http.MethodPost, "https://x/y", nil)
				// ug 族的令牌用答案卷里的 "at"：那是它自报的值，换个令牌比出来的是探针自造
				// 的假差异。设备号两侧口径不同（本仓按 uid 派生，参考实现取登录设备号），
				// 比对时 X-Device-Id 会显形 —— 那是设计差异，不是缺陷。
				tok := "at"
				if key == "headers" {
					tok = "JWT-ABC"
				}
				a := &auth.Auth{AccessToken: tok, DeviceID: "d-1", MachineID: "m-1", UID: "u-1"}
				if key == "headers" {
					SOLOHeaders(req, a, m["stream"] == true)
				} else {
					UgHeaders(req, a)
				}
				got := map[string]string{}
				for k, v := range req.Header {
					got[k] = v[0]
				}
				var missing, extra, diff []string
				for k, v := range want {
					w, ok := got[k]
					switch {
					case !ok:
						missing = append(missing, k)
					case normVec(w) != normVec(v.(string)):
						diff = append(diff, k+" ref="+v.(string)+" ours="+w)
					}
				}
				for k := range got {
					if _, ok := want[k]; !ok {
						extra = append(extra, k)
					}
				}
				sort.Strings(missing)
				sort.Strings(extra)
				if len(missing)+len(extra)+len(diff) > 0 {
					t.Errorf("%s/%v 缺=%v 多=%v 值不同=%v", key, m["name"], missing, extra, diff)
				}
			}
		}
	})

	t.Run("payload", func(t *testing.T) {
		rows, _ := vec["payload"].([]any)
		for _, r := range rows {
			m := r.(map[string]any)
			var want, got map[string]any
			if json.Unmarshal([]byte(m["output"].(string)), &want) != nil {
				continue // 非法 JSON 那例：参考的期望本身就是原文，无从比对
			}
			if json.Unmarshal(PrepareBody([]byte(m["input"].(string))), &got) != nil {
				t.Errorf("payload %v：本仓输出不是合法 JSON", m["name"])
				continue
			}
			var onlyRef, onlyOurs, valDiff []string
			for k, wv := range want {
				gv, ok := got[k]
				if !ok {
					onlyRef = append(onlyRef, k)
					continue
				}
				if wb, _ := json.Marshal(wv); string(wb) != string(mustMarshal(gv)) {
					valDiff = append(valDiff, k)
				}
			}
			for k := range got {
				if _, ok := want[k]; !ok {
					onlyOurs = append(onlyOurs, k)
				}
			}
			sort.Strings(onlyRef)
			sort.Strings(onlyOurs)
			sort.Strings(valDiff)
			if len(onlyRef)+len(onlyOurs)+len(valDiff) > 0 {
				t.Errorf("payload %v 仅参考有=%v 仅本仓有=%v 值不同=%v", m["name"], onlyRef, onlyOurs, valDiff)
			}
		}
	})

	t.Run("aggregate", func(t *testing.T) {
		rows, _ := vec["aggregate"].([]any)
		for _, r := range rows {
			m := r.(map[string]any)
			got, err := Aggregate(strings.NewReader(m["input"].(string)))
			if err != nil {
				t.Errorf("aggregate %v：本仓报错 %v（参考产出 %s）", m["name"], err, m["output"])
				continue
			}
			gb, _ := json.Marshal(got)
			if normVec(string(gb)) != normVec(m["output"].(string)) {
				t.Errorf("aggregate %v\n本仓=%s\n参考=%s", m["name"], gb, m["output"])
			}
		}
	})
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// vecNorm 归一化逐次运行不同的 id/created/session 与空白，只比形状。
var vecNormRe = regexp.MustCompile(`"(id|created|session_id|prompt_completion_id)":("?)[^",}]*("?)`)

func normVec(s string) string {
	return strings.ReplaceAll(vecNormRe.ReplaceAllString(s, `"$1":X`), " ", "")
}

func errKindName(k ErrKind) string {
	switch k {
	case ErrNone:
		return "none"
	case ErrSessionDead:
		return "session_dead"
	case ErrPlanLimit:
		return "plan_limit"
	case ErrNotFound:
		return "not_found"
	case ErrSoftRate:
		return "soft_rate"
	case ErrServer:
		return "server"
	default:
		return "client"
	}
}
