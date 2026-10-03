package server

import "testing"

// TestAccountLabel 请求记录里的账号显示名。只写裸 uid 在表里没法扫（同批账号 uid 前缀
// 相同），昵称为空时不能拼出 "(uid8)" 这种半截标签。
func TestAccountLabel(t *testing.T) {
	cases := []struct{ uid, nick, want string }{
		{"253358232317424", "MisakaNo", "MisakaNo(25335823)"},
		{"3753047934371050", "用户62666340370", "用户62666340370(37530479)"},
		{"7658504876536546322", "", "7658504876536546322"},
		{"123", "短uid", "短uid(123)"},
		{" u1 ", "  nick  ", "nick(u1)"},
	}
	for _, c := range cases {
		if got := accountLabel(c.uid, c.nick); got != c.want {
			t.Errorf("accountLabel(%q, %q) = %q，期望 %q", c.uid, c.nick, got, c.want)
		}
	}
}
