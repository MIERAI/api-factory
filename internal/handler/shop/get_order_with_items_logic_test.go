package shop

import (
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	shopv1 "apifactory/gen/factory/shop/v1"
)

func TestGetOrderWithItemsLogic(t *testing.T) {
	i32 := func(v int32) *int32 { return &v }
	i64 := func(v int64) *int64 { return &v }
	ordered := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	shipped := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	req := &shopv1.GetOrderWithItemsRequest{OrderId: 1}

	t.Run("不存在 → NOT_FOUND", func(t *testing.T) {
		_, err := getOrderWithItemsLogic(&shopv1.GetOrderWithItemsRequest{OrderId: 999}, nil)
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Code() != connect.CodeNotFound || ce.Message() != "order not found: 999" {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("订单头 + 明细按行号升序", func(t *testing.T) {
		base := getOrderWithItemsRow{ID: 1, CustomerID: 7, Status: shopv1.OrderStatus_ORDER_STATUS_SHIPPED,
			OrderedAt: ordered, ShippedAt: &shipped, TotalAmount: 3200}
		r2, r1 := base, base
		r2.LineNo, r2.ProductID, r2.Quantity, r2.UnitPrice = i32(2), i64(11), i32(1), i64(1200)
		r1.LineNo, r1.ProductID, r1.Quantity, r1.UnitPrice = i32(1), i64(10), i32(2), i64(1000)
		res, err := getOrderWithItemsLogic(req, []getOrderWithItemsRow{r2, r1})
		if err != nil {
			t.Fatal(err)
		}
		o := res.GetOrder()
		if o.GetId() != 1 || o.GetCustomerId() != 7 || o.GetStatus() != shopv1.OrderStatus_ORDER_STATUS_SHIPPED ||
			o.GetTotalAmount() != 3200 || !o.GetOrderedAt().AsTime().Equal(ordered) || !o.GetShippedAt().AsTime().Equal(shipped) {
			t.Fatalf("order = %v", o)
		}
		it := res.GetItems()
		if len(it) != 2 || it[0].GetLineNo() != 1 || it[0].GetProductId() != 10 || it[0].GetQuantity() != 2 || it[0].GetUnitPrice() != 1000 ||
			it[1].GetLineNo() != 2 || it[1].GetProductId() != 11 || it[1].GetQuantity() != 1 || it[1].GetUnitPrice() != 1200 {
			t.Fatalf("items = %v", it)
		}
	})

	t.Run("没有明细 → items 为空, shipped_at 不设置", func(t *testing.T) {
		rows := []getOrderWithItemsRow{{ID: 2, CustomerID: 7, Status: shopv1.OrderStatus_ORDER_STATUS_PLACED, OrderedAt: ordered}}
		res, err := getOrderWithItemsLogic(&shopv1.GetOrderWithItemsRequest{OrderId: 2}, rows)
		if err != nil {
			t.Fatal(err)
		}
		if res.GetOrder().GetShippedAt() != nil || len(res.GetItems()) != 0 || res.GetOrder().GetId() != 2 {
			t.Fatalf("res = %v", res)
		}
	})
}
