package accept

import (
	"math/rand"
	"testing"
	"time"

	"apifactory/factory/core"
)

func TestUniqueColumnOf(t *testing.T) {
	g := func(cs ...string) map[string]bool {
		m := map[string]bool{}
		for _, c := range cs {
			m[c] = true
		}
		return m
	}
	cases := []struct {
		cols        []string
		group, ties map[string]bool
		want        string
	}{
		{[]string{"id"}, nil, nil, "id"},                                                         // 单列
		{[]string{"order_id", "line_no"}, nil, nil, "line_no"},                                   // 没有分组列: 最后一列
		{[]string{"order_id", "line_no"}, g("order_id"), nil, "line_no"},                         // 分组列在前: 不变
		{[]string{"sales_date", "store_code"}, g("store_code"), nil, "sales_date"},               // 分组列在最后: 跳过它
		{[]string{"a", "b", "c"}, g("b", "c"), nil, "a"},                                         // 从后往前第一个非分组列
		{[]string{"sales_date", "store_code"}, g("sales_date", "store_code"), nil, "store_code"}, // 都是分组列
		// 主键 (store_code, product_code, proc_date)、product_code 等于、排序 [proc_date, store_code]:
		// proc_date 要平局 → store_code 取唯一值
		{[]string{"store_code", "product_code", "proc_date"}, g("product_code"), g("proc_date"), "store_code"},
		{[]string{"a", "b"}, g("b"), g("a"), "a"}, // 都不满足: 退回到"不是分组列"
	}
	for _, c := range cases {
		if got := uniqueColumnOf(c.cols, c.group, c.ties); got != c.want {
			t.Errorf("uniqueColumnOf(%v, %v, %v) = %q, want %q", c.cols, c.group, c.ties, got, c.want)
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
