package shop

import (
	"testing"

	shopv1 "apifactory/gen/factory/shop/v1"
)

func TestGetCustomerOrderStatsLogic(t *testing.T) {
	const (
		placed    = shopv1.OrderStatus_ORDER_STATUS_PLACED
		delivered = shopv1.OrderStatus_ORDER_STATUS_DELIVERED
		cancelled = shopv1.OrderStatus_ORDER_STATUS_CANCELLED
		unknown   = shopv1.OrderStatus_ORDER_STATUS_UNSPECIFIED
	)
	i64 := func(v int64) *int64 { return &v }
	f64 := func(v float64) *float64 { return &v }

	tests := []struct {
		name       string
		rows       []getCustomerOrderStatsRow
		count      int64
		total      int64
		avg        *int64
		cancelRate *float64
	}{
		{
			name:  "取消计入件数和取消率,不计入金额",
			rows:  []getCustomerOrderStatsRow{{1, delivered, 1000}, {2, cancelled, 5000}, {3, placed, 2001}},
			count: 3, total: 3001, avg: i64(1501), cancelRate: f64(0.3333),
		},
		{name: "没有订单", rows: nil, count: 0, total: 0},
		{
			name:  "全部取消: avg 不设置,取消率 1",
			rows:  []getCustomerOrderStatsRow{{1, cancelled, 100}, {2, cancelled, 200}},
			count: 2, total: 0, cancelRate: f64(1),
		},
		{
			name:  "未知状态算作未取消",
			rows:  []getCustomerOrderStatsRow{{1, unknown, 300}},
			count: 1, total: 300, avg: i64(300), cancelRate: f64(0),
		},
		{
			name:  "平均 0.5 向上",
			rows:  []getCustomerOrderStatsRow{{1, placed, 1}, {2, placed, 2}},
			count: 2, total: 3, avg: i64(2), cancelRate: f64(0),
		},
		{
			name:  "平均不足 0.5 舍去",
			rows:  []getCustomerOrderStatsRow{{1, placed, 1}, {2, placed, 1}, {3, placed, 2}},
			count: 3, total: 4, avg: i64(1), cancelRate: f64(0),
		},
		{
			name:  "取消率 2/3 → 0.6667",
			rows:  []getCustomerOrderStatsRow{{1, cancelled, 1}, {2, cancelled, 1}, {3, placed, 10}},
			count: 3, total: 10, avg: i64(10), cancelRate: f64(0.6667),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := getCustomerOrderStatsLogic(&shopv1.GetCustomerOrderStatsRequest{CustomerId: 1}, tt.rows)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.GetOrderCount() != tt.count || res.GetTotalAmount() != tt.total {
				t.Errorf("count/total = %d/%d, want %d/%d", res.GetOrderCount(), res.GetTotalAmount(), tt.count, tt.total)
			}
			if (res.AvgAmount == nil) != (tt.avg == nil) || (tt.avg != nil && *res.AvgAmount != *tt.avg) {
				t.Errorf("avg = %v, want %v", res.AvgAmount, tt.avg)
			}
			if (res.CancelRate == nil) != (tt.cancelRate == nil) || (tt.cancelRate != nil && *res.CancelRate != *tt.cancelRate) {
				t.Errorf("cancel_rate = %v, want %v", res.CancelRate, tt.cancelRate)
			}
		})
	}
}

func TestDivRoundHalfAwayFromZero(t *testing.T) {
	cases := []struct{ n, d, want int64 }{
		{3, 2, 2}, {-3, 2, -2}, {5, 3, 2}, {4, 3, 1}, {-4, 3, -1}, {-5, 3, -2}, {0, 7, 0},
	}
	for _, c := range cases {
		if got := divRoundHalfAwayFromZero(c.n, c.d); got != c.want {
			t.Errorf("div(%d,%d) = %d, want %d", c.n, c.d, got, c.want)
		}
	}
}
