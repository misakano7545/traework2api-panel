package server

import "testing"

// 别名层：客户端自带的模型名（Claude Code / Cursor / Cline 写死的那套）要能落到真模型上，
// 且必须按地区落——国际账号上没有 glm-5.2，国内账号上没有 gpt-5.2。
func TestAliasModel(t *testing.T) {
	cases := []struct {
		realm, in, want string
	}{
		{"cn", "claude-sonnet-4-5", "glm-5.2"},
		{"cn", "claude-3-7-sonnet-20250219", "glm-5.2"}, // 带日期后缀的一族走前缀
		{"cn", "gpt-4o", "glm-5.2"},
		{"intl", "claude-opus-4-7", "gpt-5.2"},
		{"intl", "gpt-4o-mini", "gpt-5.2"},
		{"intl", "gpt-5.1", "gpt-5.2"},
		{"cn", "Gpt-4o", "glm-5.2"}, // normalizeModelName 会首字母大写，别名表仍要命中
	}
	for _, c := range cases {
		got, ok := aliasModel(c.realm, c.in)
		if !ok || got != c.want {
			t.Errorf("aliasModel(%s, %q) = %q,%v want %q", c.realm, c.in, got, ok, c.want)
		}
	}
	// 网页端「TraeWork Auto Model」在反代里就是 auto：各地区的旗舰（地区不能串）。
	if got, ok := aliasModel("intl", "traework-auto-model"); ok {
		t.Errorf("auto 不该走别名表 → %q", got)
	}
	for realm, want := range map[string]string{"cn": "glm-5.2", "intl": "gpt-5.2"} {
		if strongModel(realm) != want {
			t.Errorf("strongModel(%s) = %q want %q", realm, strongModel(realm), want)
		}
	}

	// 不是别名的名字不能凭空给目标（未知模型仍要 400，别吞成默认模型）。
	for _, s := range []string{"glm-5.2", "llama-3-70b", "some-random-model", "claude"} {
		if got, ok := aliasModel("cn", s); ok && s != "claude" {
			t.Errorf("aliasModel(cn, %q) 不该命中 → %q", s, got)
		}
	}
}
