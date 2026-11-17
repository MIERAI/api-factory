package kinds

import (
	"fmt"
	"time"

	"apifactory/factory/core"
)

var diffBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func init() {
	// 日期: API 里 "YYYY-MM-DD"
	Register(&Type{Name: "日期", Core: core.Date, SQL: []string{"date"}, Proto: "string", Value: "date", Go: "time.Time",
		Pattern: `^\d{4}-\d{2}-\d{2}$`,
		Boundaries: []Boundary{
			{"日期格式不对(2026/01/01)", "2026/01/01", false},
			{"日期格式正确(2026-01-01)", "2026-01-01", true},
		},
		Parse: func(s string) (any, error) {
			if _, err := time.Parse("2006-01-02", s); err != nil {
				return nil, fmt.Errorf("日期 %q 要写成 2026-11-01 的形式", s)
			}
			return s, nil
		},
		Domain:  []any{diffBase.AddDate(0, 0, -1), diffBase, diffBase.AddDate(0, 0, 1)},
		Missing: diffBase.AddDate(5, 0, 0)})
	// 时间: 时刻(timestamptz),API 里 google.protobuf.Timestamp。例子的写法按领域的时区解释(设计编译器处理)
	Register(&Type{Name: "时间", Core: core.Timestamp, SQL: []string{"timestamptz", "timestamp"}, Proto: "google.protobuf.Timestamp", Value: "time", Go: "time.Time",
		Domain:  []any{diffBase.Add(-time.Hour), diffBase, diffBase.Add(time.Hour), diffBase.Add(2 * time.Hour)},
		Missing: diffBase.AddDate(5, 0, 0)})
}
