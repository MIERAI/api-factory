// Logic for specs/shop/get-order-with-items/spec.yaml — implemented by AI.

package shop

import (
	"errors"

	"connectrpc.com/connect"

	shopv1 "apifactory/gen/factory/shop/v1"
)

// getOrderWithItemsLogic 实现 GetOrderWithItems 的业务逻辑(式样书 behavior 节)。
// 纯函数: 不访问 DB/网络/文件,不用 context,不调用 time.Now,不用全局可变状态。
func getOrderWithItemsLogic(req *shopv1.GetOrderWithItemsRequest, rows []getOrderWithItemsRow) (*shopv1.GetOrderWithItemsResponse, error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("getOrderWithItemsLogic: not implemented"))
}
