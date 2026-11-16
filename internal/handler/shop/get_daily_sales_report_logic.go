// Logic for specs/shop/get-daily-sales-report/spec.yaml — implemented by AI.

package shop

import (
	"time"

	shopv1 "apifactory/gen/factory/shop/v1"
)

// dailySalesDateLayout: 响应里日期的格式(日本时间 YYYY-MM-DD)。
const dailySalesDateLayout = "2006-01-02"

// getDailySalesReportLogic 实现 GetDailySalesReport 的业务逻辑(式样书 behavior 节)。
// 纯函数: 不访问 DB/网络/文件,不用 context,不调用 time.Now,不用全局可变状态。
func getDailySalesReportLogic(req *shopv1.GetDailySalesReportRequest, rows []getDailySalesReportRow) (*shopv1.GetDailySalesReportResponse, error) {
	// 固定 UTC+9,不读系统时区(behavior: 日期的划分)。局部变量,避免全局状态。
	jst := time.FixedZone("JST", 9*3600)

	start := req.GetPeriodStart().AsTime()
	end := req.GetPeriodEnd().AsTime()

	// 从 period_start 所在的日本日期,到 period_end 前一刻所在的日本日期,每一天都返回。
	s := start.In(jst)
	last := end.Add(-time.Nanosecond).In(jst)
	firstDay := time.Date(s.Year(), s.Month(), s.Day(), 0, 0, 0, 0, jst)
	lastDay := time.Date(last.Year(), last.Month(), last.Day(), 0, 0, 0, 0, jst)

	days := []*shopv1.DailySales{}
	index := map[string]*shopv1.DailySales{}
	for i := 0; ; i++ {
		d := time.Date(firstDay.Year(), firstDay.Month(), firstDay.Day()+i, 0, 0, 0, 0, jst)
		if d.After(lastDay) {
			break
		}
		ds := &shopv1.DailySales{Date: d.Format(dailySalesDateLayout)}
		days = append(days, ds)
		index[ds.Date] = ds
	}

	for _, r := range rows {
		// CANCELLED 不计入;其他状态都计入。
		if r.Status == shopv1.OrderStatus_ORDER_STATUS_CANCELLED {
			continue
		}
		// 只统计期间 [period_start, period_end) 内的订单(查询已过滤,这里保证首尾两天的语义不依赖查询)。
		if r.OrderedAt.Before(start) || !r.OrderedAt.Before(end) {
			continue
		}
		ds, ok := index[r.OrderedAt.In(jst).Format(dailySalesDateLayout)]
		if !ok {
			continue // 期间内的订单必然落在返回的日期里;防御性跳过
		}
		ds.OrderCount++
		ds.TotalAmount += r.TotalAmount
	}

	return &shopv1.GetDailySalesReportResponse{Days: days}, nil
}
