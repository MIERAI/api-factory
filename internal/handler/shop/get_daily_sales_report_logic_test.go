package shop

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	shopv1 "apifactory/gen/factory/shop/v1"
)

func TestGetDailySalesReportLogic(t *testing.T) {
	ts := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	req := func(start, end string) *shopv1.GetDailySalesReportRequest {
		return &shopv1.GetDailySalesReportRequest{
			PeriodStart: timestamppb.New(ts(start)),
			PeriodEnd:   timestamppb.New(ts(end)),
		}
	}
	delivered := shopv1.OrderStatus_ORDER_STATUS_DELIVERED
	cancelled := shopv1.OrderStatus_ORDER_STATUS_CANCELLED
	fixture := []getDailySalesReportRow{
		{ID: 1, Status: delivered, OrderedAt: ts("2026-10-01T14:59:59Z"), TotalAmount: 1000},
		{ID: 2, Status: delivered, OrderedAt: ts("2026-10-01T15:00:00Z"), TotalAmount: 2000},
		{ID: 3, Status: cancelled, OrderedAt: ts("2026-10-01T16:00:00Z"), TotalAmount: 9000},
	}
	type day struct {
		date         string
		count, total int64
	}
	check := func(t *testing.T, res *shopv1.GetDailySalesReportResponse, want []day) {
		t.Helper()
		got := res.GetDays()
		if len(got) != len(want) {
			t.Fatalf("days: got %d, want %d: %v", len(got), len(want), got)
		}
		for i, w := range want {
			g := got[i]
			if g.GetDate() != w.date || g.GetOrderCount() != w.count || g.GetTotalAmount() != w.total {
				t.Errorf("day[%d]: got {%s %d %d}, want %v", i, g.GetDate(), g.GetOrderCount(), g.GetTotalAmount(), w)
			}
		}
	}

	t.Run("按日本时间划分;取消不计;没有订单的日子返回 0", func(t *testing.T) {
		res, err := getDailySalesReportLogic(req("2026-09-30T15:00:00Z", "2026-10-03T15:00:00Z"), fixture)
		if err != nil {
			t.Fatal(err)
		}
		check(t, res, []day{{"2026-10-01", 1, 1000}, {"2026-10-02", 1, 2000}, {"2026-10-03", 0, 0}})
	})

	t.Run("期间不在 0 点 — 首尾两天只统计期间内的订单", func(t *testing.T) {
		res, err := getDailySalesReportLogic(req("2026-10-01T03:00:00Z", "2026-10-01T15:30:00Z"), fixture)
		if err != nil {
			t.Fatal(err)
		}
		check(t, res, []day{{"2026-10-01", 1, 1000}, {"2026-10-02", 1, 2000}})
	})

	t.Run("期间外的行即使传入也不计", func(t *testing.T) {
		// JST 10/1 23:59:59 ~ 10/2 00:00:00 之后: 只有 period_start 那一刻起的订单
		res, err := getDailySalesReportLogic(req("2026-10-01T15:00:00Z", "2026-10-01T15:00:01Z"), fixture)
		if err != nil {
			t.Fatal(err)
		}
		check(t, res, []day{{"2026-10-02", 1, 2000}})
	})

	t.Run("period_end 恰好在 0 点 — 不包含 period_end 所在的日子", func(t *testing.T) {
		res, err := getDailySalesReportLogic(req("2026-09-30T15:00:00Z", "2026-10-01T15:00:00Z"), fixture)
		if err != nil {
			t.Fatal(err)
		}
		check(t, res, []day{{"2026-10-01", 1, 1000}})
	})

	t.Run("同一天多笔订单累加;未知状态计入", func(t *testing.T) {
		rows := []getDailySalesReportRow{
			{ID: 1, Status: shopv1.OrderStatus_ORDER_STATUS_PLACED, OrderedAt: ts("2026-10-01T00:00:00Z"), TotalAmount: 100},
			{ID: 2, Status: shopv1.OrderStatus_ORDER_STATUS_UNSPECIFIED, OrderedAt: ts("2026-10-01T01:00:00Z"), TotalAmount: 200},
		}
		res, err := getDailySalesReportLogic(req("2026-09-30T15:00:00Z", "2026-10-01T15:00:00Z"), rows)
		if err != nil {
			t.Fatal(err)
		}
		check(t, res, []day{{"2026-10-01", 2, 300}})
	})

	t.Run("跨月 31 天", func(t *testing.T) {
		res, err := getDailySalesReportLogic(req("2026-10-31T15:00:00Z", "2026-12-01T15:00:00Z"), nil)
		if err != nil {
			t.Fatal(err)
		}
		days := res.GetDays()
		if len(days) != 31 || days[0].GetDate() != "2026-11-01" || days[30].GetDate() != "2026-12-01" {
			t.Fatalf("got %d days: %v .. %v", len(days), days[0], days[len(days)-1])
		}
	})
}
