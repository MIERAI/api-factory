// Logic for specs/shop/get-customer-order-stats/spec.yaml — implemented by AI.

package shop

import (
	"math"

	shopv1 "apifactory/gen/factory/shop/v1"
)

// getCustomerOrderStatsLogic 实现 GetCustomerOrderStats 的业务逻辑(式样书 behavior 节)。
// 纯函数: 不访问 DB/网络/文件,不用 context,不调用 time.Now,不用全局可变状态。
func getCustomerOrderStatsLogic(req *shopv1.GetCustomerOrderStatsRequest, rows []getCustomerOrderStatsRow) (*shopv1.GetCustomerOrderStatsResponse, error) {
	var orderCount, cancelled, active, total int64
	for _, r := range rows {
		orderCount++
		// 式样书: 取消 = CANCELLED 的行;未取消 = 其他(含 UNSPECIFIED)
		if r.Status == shopv1.OrderStatus_ORDER_STATUS_CANCELLED {
			cancelled++
			continue
		}
		active++
		total += r.TotalAmount
	}

	res := &shopv1.GetCustomerOrderStatsResponse{
		OrderCount:  orderCount,
		TotalAmount: total,
	}
	if active > 0 {
		avg := divRoundHalfAwayFromZero(total, active)
		res.AvgAmount = &avg
	}
	if orderCount > 0 {
		rate := math.Round(float64(cancelled)/float64(orderCount)*1e4) / 1e4
		res.CancelRate = &rate
	}
	return res, nil
}

// divRoundHalfAwayFromZero: n / d(d > 0)四舍五入到整数,0.5 远离零。整数运算,无浮点误差。
func divRoundHalfAwayFromZero(n, d int64) int64 {
	q, r := n/d, n%d // Go 的 % 符号与被除数相同
	if r < 0 {
		r = -r
	}
	if 2*r >= d {
		if n < 0 {
			q--
		} else {
			q++
		}
	}
	return q
}
