package accept

import (
	"math/rand"
	"testing"
	"time"

	"apifactory/factory/core"
)

func TestUniqueColumnOf(t *testing.T) {
	cases := []struct {
		cols  []string
		group map[string]bool
		want  string
	}{
		{[]string{"id"}, nil, "id"},                                                                                   // 单列
		{[]string{"order_id", "line_no"}, nil, "line_no"},                                                             // 没有分组列: 最后一列
		{[]string{"order_id", "line_no"}, map[string]bool{"order_id": true}, "line_no"},                               // 分组列在前: 不变
		{[]string{"sales_date", "store_code"}, map[string]bool{"store_code": true}, "sales_date"},                     // 分组列在最后: 跳过它
		{[]string{"a", "b", "c"}, map[string]bool{"b": true, "c": true}, "a"},                                         // 从后往前第一个非分组列
		{[]string{"sales_date", "store_code"}, map[string]bool{"sales_date": true, "store_code": true}, "store_code"}, // 都是分组列
	}
	for _, c := range cases {
		if got := uniqueColumnOf(c.cols, c.group); got != c.want {
			t.Errorf("uniqueColumnOf(%v, %v) = %q, want %q", c.cols, c.group, got, c.want)
		}
	}
}

// 唯一列的值: 日期 / 时刻 / 十进制也要两两不同(以前从值域的几个值里随机取 → 主键冲突,数据被拒绝)
func TestScalarUnique(t *testing.T) {
	e := &diffEnv{rnd: rand.New(rand.NewSource(1))}
	for _, k := range []core.Kind{core.Date, core.Timestamp, core.Decimal, core.Int64, core.Text} {
		seen := map[string]bool{}
		for i := 0; i < 1000; i++ {
			v := core.Canonical(e.scalar(k, true, i))
			if seen[v] {
				t.Fatalf("%s: value %s repeated at i=%d", k, v, i)
			}
			seen[v] = true
		}
	}
	// 日期: 围绕值域中间的日期交替展开(大于 / 小于的筛选切掉约一半)
	d0 := e.scalar(core.Date, true, 0).(time.Time)
	if d1, d2 := e.scalar(core.Date, true, 1).(time.Time), e.scalar(core.Date, true, 2).(time.Time); !d1.Before(d0) || !d2.After(d0) {
		t.Errorf("dates not alternating around the center: %v %v %v", d0, d1, d2)
	}
}
