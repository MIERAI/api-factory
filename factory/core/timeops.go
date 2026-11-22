package core

// 时间运算的参考实现(add / trunc)。独立于 SQL 后端: 不调用数据库,月的加法、时区的换算都在这里自己算,
// 由元件认证和 Postgres 逐值比较(每个月末 + 1 个月、闰年、负数、时区的日界线前后)。
//
// 语义(和 Postgres 一致):
//   - 月: 年月相加,日超过那个月的天数 → 那个月的最后一天(1/31 + 1 个月 = 2/28,闰年 2/29)。
//     Go 的 time.AddDate 不是这样(1/31 + 1 个月 = 3/3),所以不用它。
//   - 时间的 day / month: 换算到领域的时区的墙上时间,按日历加,再换算回时刻(timestamptz AT TIME ZONE)。
//   - 时间的 minute / hour: 绝对时长。
//   - trunc: 时间先换算到领域的时区,截到那一天 / 那个月的 0 点,结果是那个时刻。

import (
	"fmt"
	"math"
	"sync"
	"time"
)

var zones sync.Map // 名字 → *time.Location

func loadZone(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	if l, ok := zones.Load(name); ok {
		return l.(*time.Location), nil
	}
	l, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	zones.Store(name, l)
	return l, nil
}

func isLeap(y int) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }

func daysIn(y int, m time.Month) int {
	switch m {
	case time.February:
		if isLeap(y) {
			return 29
		}
		return 28
	case time.April, time.June, time.September, time.November:
		return 30
	}
	return 31
}

// addMonths: (y, m, d) + n 个月,日超过那个月的天数时取最后一天。
func addMonths(y int, m time.Month, d int, n int64) (int, time.Month, int) {
	total := int64(y)*12 + int64(m-1) + n
	ny := total / 12
	nm := total % 12
	if nm < 0 { // 向下取整的除法
		nm += 12
		ny--
	}
	month := time.Month(nm + 1)
	if dim := daysIn(int(ny), month); d > dim {
		d = dim
	}
	return int(ny), month, d
}

// addTime: x(日期 = UTC 0 点的 time.Time;时间)加 n 个单位。
func addTime(x time.Time, n int64, unit string, kind Kind, zone string) (time.Time, error) {
	if n > math.MaxInt32 || n < math.MinInt32 { // Postgres: make_interval 的参数是 int4
		return time.Time{}, fmt.Errorf("add: %d 超出整数(int4)的范围", n)
	}
	if kind == Date {
		y, m, d := x.Date()
		switch unit {
		case Day:
			return time.Date(y, m, d+int(n), 0, 0, 0, 0, time.UTC), nil
		case Month:
			ny, nm, nd := addMonths(y, m, d, n)
			return time.Date(ny, nm, nd, 0, 0, 0, 0, time.UTC), nil
		}
		return time.Time{}, fmt.Errorf("add: 日期不能加 %s", unit)
	}
	switch unit {
	case Minute:
		return x.Add(time.Duration(n) * time.Minute).UTC(), nil
	case Hour:
		return x.Add(time.Duration(n) * time.Hour).UTC(), nil
	}
	loc, err := loadZone(zone)
	if err != nil {
		return time.Time{}, err
	}
	w := x.In(loc)
	y, m, d := w.Date()
	hh, mm, ss := w.Clock()
	ns := w.Nanosecond()
	switch unit {
	case Day:
		return localToInstant(loc, y, m, d+int(n), hh, mm, ss, ns), nil
	case Month:
		ny, nm, nd := addMonths(y, m, d, n)
		return localToInstant(loc, ny, nm, nd, hh, mm, ss, ns), nil
	}
	return time.Time{}, fmt.Errorf("add: 单位 %q 不对", unit)
}

// truncTime: x 截到 unit(day / month)的开始。
func truncTime(x time.Time, unit string, kind Kind, zone string) (time.Time, error) {
	loc := time.UTC
	if kind == Timestamp {
		l, err := loadZone(zone)
		if err != nil {
			return time.Time{}, err
		}
		loc = l
	}
	y, m, d := x.In(loc).Date()
	switch unit {
	case Day:
	case Month:
		d = 1
	default:
		return time.Time{}, fmt.Errorf("trunc: 单位 %q 不对", unit)
	}
	return localToInstant(loc, y, m, d, 0, 0, 0, 0), nil
}

// localToInstant: 时区 loc 的墙上时间 → 时刻,夏令时的切换按 Postgres 的规则(timestamp AT TIME ZONE):
// 不存在的时刻(往前跳的那一小时里)用切换前的偏移;有两个的时刻(往回跳)用切换后的偏移。
// Go 的 time.Date 对这两种情况"不保证"选哪个(实测往回跳时选切换前的),所以自己算。
func localToInstant(loc *time.Location, y int, m time.Month, d, hh, mm, ss, ns int) time.Time {
	wall := time.Date(y, m, d, hh, mm, ss, ns, time.UTC) // 墙上时间当作 UTC 的读数
	offsetAt := func(t time.Time) int { _, o := t.In(loc).Zone(); return o }
	before := offsetAt(wall.Add(-48 * time.Hour)) // 假设 ±2 天里最多一次切换(现实的时区都是这样)
	after := offsetAt(wall.Add(48 * time.Hour))
	at := func(o int) (time.Time, bool) {
		t := wall.Add(-time.Duration(o) * time.Second)
		return t.UTC(), offsetAt(t) == o
	}
	ta, okA := at(after)
	tb, okB := at(before)
	switch {
	case okA: // 只有切换后的成立,或者两个都成立(有两个的时刻: 用切换后的)
		return ta
	case okB:
		return tb
	}
	return tb // 不存在的时刻: 用切换前的偏移
}

// YMDDate: YYYYMMDD 的整数 → 日期(UTC 零点)。不是合法的日期(年 1000~9999、月 1~12、日 1~那个月的天数)→ nil。
// loose(差分测试的错误实现): 年月日拆开后交给 time.Date(不合法的日 / 月按日历进位)。
func YMDDate(n int64, loose bool) any {
	y, m, d := n/10000, n/100%100, n%100
	if loose {
		if n <= 0 {
			return nil
		}
		return time.Date(int(y), time.Month(m), int(d), 0, 0, 0, 0, time.UTC)
	}
	if n < 10000101 || n > 99991231 || m < 1 || m > 12 || d < 1 {
		return nil
	}
	t := time.Date(int(y), time.Month(m), int(d), 0, 0, 0, 0, time.UTC)
	if t.Day() != int(d) { // 那个月没有这一天(time.Date 进位到了下个月)
		return nil
	}
	return t
}

// YMDInt: 日期 → YYYYMMDD 的整数。
func YMDInt(t time.Time) int64 {
	return int64(t.Year())*10000 + int64(t.Month())*100 + int64(t.Day())
}
