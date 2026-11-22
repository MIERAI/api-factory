package design

import (
	"strings"
	"testing"

	"apifactory/factory/core"
)

// 计算的写法 → 核心代数(core.Format 的文本),以及读回来(core.ParseExpr)还是同一个表达式。
func TestParseDerived(t *testing.T) {
	inputs := map[string]core.Kind{"n": core.Int32, "big": core.Int64, "d": core.Date, "at": core.Timestamp, "s": core.Text}
	in := func(name string) (core.Kind, bool) { k, ok := inputs[name]; return k, ok }
	for _, c := range []struct {
		src, want string
		now       bool
	}{
		{"现在 - n 分钟", "add(now(), -n, minute)", true},
		{"现在-5分钟", "add(now(), -5, minute)", true},
		{"现在 + 2 小时 - big 天", "add(add(now(), 2, hour), -big, day)", true},
		{"月初(d - 1 个月)", "trunc(add(d, -1, month), month)", false},
		{"月初(d) - 1 天", "add(trunc(d, month), -1, day)", false},
		{"日初(现在)", "trunc(now(), day)", true},
		{"(at + 1 月)", "add(at, 1, month)", false},
	} {
		x, _, now, err := parseDerived(c.src, in)
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		got, err := core.Format(x)
		if err != nil || got != c.want || now != c.now {
			t.Errorf("%s: %q now=%v %v,期待 %q now=%v", c.src, got, now, err, c.want, c.now)
			continue
		}
		back, err := core.ParseExpr(got, func(id string) (*core.Expr, bool) { _, ok := inputs[id]; return core.Param(id), ok })
		if err != nil {
			t.Errorf("%s: 读不回来: %v", got, err)
			continue
		}
		if again, _ := core.Format(back); again != got {
			t.Errorf("%s: 读回来是 %s", got, again)
		}
	}
	for _, c := range []struct{ src, err string }{
		{"现在 - x 分钟", "不是输入"},
		{"现在 - s 分钟", "必须是整数"},
		{"n - 1 天", "不是日期 / 时间"},
		{"现在 - 1 周", "单位"},
		{"月初 现在", "后面要写"},
		{"现在 现在", "多余"},
		{"月初(现在", "缺少 )"},
	} {
		if _, _, _, err := parseDerived(c.src, in); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: 应该报错 %q,实际 %v", c.src, c.err, err)
		}
	}
}
