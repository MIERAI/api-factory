// Logic for specs/shop/get-order-with-items/spec.yaml — implemented by AI.

package shop

import (
	"fmt"
	"sort"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	shopv1 "apifactory/gen/factory/shop/v1"
)

// getOrderWithItemsLogic 实现 GetOrderWithItems 的业务逻辑(式样书 behavior 节)。
// 纯函数: 不访问 DB/网络/文件,不用 context,不调用 time.Now,不用全局可变状态。
func getOrderWithItemsLogic(req *shopv1.GetOrderWithItemsRequest, rows []getOrderWithItemsRow) (*shopv1.GetOrderWithItemsResponse, error) {
	if len(rows) == 0 {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("order not found: %d", req.GetOrderId()))
	}

	head := rows[0]
	order := &shopv1.Order{
		Id:          head.ID,
		CustomerId:  head.CustomerID,
		Status:      head.Status,
		OrderedAt:   timestamppb.New(head.OrderedAt),
		TotalAmount: head.TotalAmount,
	}
	if head.ShippedAt != nil {
		order.ShippedAt = timestamppb.New(*head.ShippedAt)
	}

	items := make([]*shopv1.OrderItem, 0, len(rows))
	for _, r := range rows {
		if r.LineNo == nil { // LEFT JOIN 无明细
			continue
		}
		items = append(items, &shopv1.OrderItem{
			LineNo:    *r.LineNo,
			ProductId: derefOr(r.ProductID),
			Quantity:  derefOr(r.Quantity),
			UnitPrice: derefOr(r.UnitPrice),
		})
	}
	// SQL 已 ORDER BY line_no;这里再稳定排序一次,保证不依赖输入顺序。
	sort.SliceStable(items, func(i, j int) bool { return items[i].LineNo < items[j].LineNo })

	return &shopv1.GetOrderWithItemsResponse{Order: order, Items: items}, nil
}

// derefOr: 明细列在 schema 上都是 NOT NULL,line_no 非 NULL 时不会为 nil;防御性地取零值。
func derefOr[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
