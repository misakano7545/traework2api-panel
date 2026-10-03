package usage

import (
	"math"
	"testing"
	"time"
)

// 公式与单价表：断言只挑「算错就会把积分报错一个量级」的那几件事。
func TestEstimateCredit(t *testing.T) {
	// 刊例 × 40：glm-5.2 输入 8 元/M → 320 积分/M，再乘实测会员补贴 0.675 → 216。
	if v, ok := EstimateCredit("glm-5.2", 1_000_000, 0, 0); !ok || math.Abs(v-216) > 1e-9 {
		t.Fatalf("glm-5.2 输入 1M → %v (ok=%v)，期望 216", v, ok)
	}
	// 输出价与输入价不同（28 元/M → 756 积分/M），不能拿输入价代替。
	if v, _ := EstimateCredit("glm-5.2", 0, 1_000_000, 0); math.Abs(v-756) > 1e-9 {
		t.Fatalf("输出 1M → %v，期望 756", v)
	}
	// 缓存命中 token 走缓存价（2 元/M → 54），且**要先从输入里扣掉**，不能双算。
	if v, _ := EstimateCredit("glm-5.2", 1_000_000, 0, 1_000_000); math.Abs(v-54) > 1e-9 {
		t.Fatalf("全部命中缓存 → %v，期望 54（只按缓存价，不叠加输入价）", v)
	}
	// measuredOverride 优先于刊例 × 折扣。
	if v, _ := EstimateCredit("deepseek-v4.1-flash", 1_000_000, 0, 0); math.Abs(v-28) > 1e-9 {
		t.Fatalf("v4.1-flash 实测覆盖价 → %v，期望 28", v)
	}
	// 缓存 token 超输入（上游脏数据）要夹住，不能算出负数。
	if v, ok := EstimateCredit("glm-5.2", 1000, 0, 5000); !ok || v < 0 {
		t.Fatalf("缓存 > 输入应夹到输入量，实得 %v", v)
	}
	// 大小写/空白不敏感（模型 id 从上游来，大小写不一定）。
	if a, _ := EstimateCredit("GLM-5.2 ", 1000, 10, 0); a == 0 {
		t.Fatal("大小写/空白应被归一化")
	}
	// 域前缀 cn: 要剥掉：同一条请求在网关里有裸名与带前缀两种入账形态，不归一就会出现
	// 「同一模型一半请求有估算、一半没有」。intl: 不剥——国际版计费口径不同，宁可估不出。
	if v, ok := EstimateCredit("cn:glm-5.2", 1_000_000, 0, 0); !ok || math.Abs(v-216) > 1e-9 {
		t.Fatalf("cn: 前缀应与裸名同价 → %v (ok=%v)，期望 216", v, ok)
	}
	if _, ok := EstimateCredit("intl:glm-5.2", 1_000_000, 0, 0); ok {
		t.Fatal("intl: 前缀不该套用人民币价目表")
	}
	// 未收录模型必须返回 false：宁可不显示，也不拿错单价误导。
	if _, ok := EstimateCredit("some-unknown-model", 1e6, 0, 0); ok {
		t.Fatal("未收录模型不该给出估算")
	}
}

// 聚合层：积分按桶推算，账号/模型/总计三个维度都要有值且一致（同源累加）。
func TestSnapshotCarriesEstimatedCredits(t *testing.T) {
	r := New("")
	r.Add(time.Now(), "u1", "glm-5.2", Delta{
		PromptTokens: 1_000_000, HasPromptTokens: true,
		CompletionTokens: 0, HasCompletion: true,
		TotalTokens: 1_000_000, HasTotal: true,
	}, true)
	snap := r.Snapshot(0, nil)

	if math.Abs(snap.Totals.Credits-216) > 1e-6 {
		t.Fatalf("totals 积分 = %v，期望 216", snap.Totals.Credits)
	}
	if len(snap.ByModel) != 1 || math.Abs(snap.ByModel[0].Credits-216) > 1e-6 {
		t.Fatalf("by_model 积分 = %+v，期望 216", snap.ByModel)
	}
	if len(snap.ByAccount) != 1 || math.Abs(snap.ByAccount[0].Credits-216) > 1e-6 {
		t.Fatalf("by_account 积分 = %+v，期望 216", snap.ByAccount)
	}
}

// 未收录模型的桶不进积分合计（不拿别的模型的价凑数），但 token 仍然照记。
func TestSnapshotSkipsUnpricedModelCredits(t *testing.T) {
	r := New("")
	r.Add(time.Now(), "u1", "mystery-model", Delta{
		PromptTokens: 1_000_000, HasPromptTokens: true,
		TotalTokens: 1_000_000, HasTotal: true,
	}, true)
	snap := r.Snapshot(0, nil)
	if snap.Totals.Credits != 0 {
		t.Fatalf("未收录模型不该产生积分：%v", snap.Totals.Credits)
	}
	if snap.Totals.PromptTokens != 1_000_000 {
		t.Fatalf("token 仍要照记：%v", snap.Totals.PromptTokens)
	}
}
