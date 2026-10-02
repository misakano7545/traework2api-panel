package server

import "testing"

// 地区前缀协议：`[realm:]model`。前缀是网关路由协议（上游不认），剥完只剩裸名。
func TestResolveModel(t *testing.T) {
	cases := []struct {
		in        string
		wantRealm string
		wantBare  string
	}{
		{"gpt-5.2", "cn", "gpt-5.2"},               // 裸名 → 国内版（老客户端零回归）
		{"cn:glm-5.2", "cn", "glm-5.2"},            // 显式国内前缀
		{"intl:gpt-5.2", "intl", "gpt-5.2"},        // 显式国际前缀
		{"intl:", "intl", ""},                      // 只给前缀 → 空裸名，交给 mapModel 走默认模型
		{"global:gpt-5.2", "cn", "global:gpt-5.2"}, // 别的枚举（workbuddy 叫 global）不当前缀
		{"Intl:gpt-5.2", "cn", "Intl:gpt-5.2"},     // 大小写敏感
		{"a:b:c", "cn", "a:b:c"},                   // 前段不是枚举 → 整串当裸名（只认第一个冒号）
		{"intl:a:b", "intl", "a:b"},                // 前缀剥掉后，后续冒号留在裸名里
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.wantRealm || bare != c.wantBare {
			t.Errorf("resolveModel(%q) = (%q, %q), want (%q, %q)", c.in, realm, bare, c.wantRealm, c.wantBare)
		}
	}
}
