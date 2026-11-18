// Logic for specs/sales/get-price-change-summary/spec.yaml — implemented by AI.

package sales

import (
	"math"

	salesv1 "apifactory/gen/factory/sales/v1"
)

const priceChangeForcedDealID int64 = 99999999

func roundRate4(x float64) float64 {
	return math.Round(x*10000) / 10000
}

// getPriceChangeSummaryLogic 实现 GetPriceChangeSummary 的业务逻辑(式样书 behavior 节)。
// 纯函数: 不访问 DB/网络/文件,不用 context,不调用 time.Now,不用全局可变状态。
func getPriceChangeSummaryLogic(req *salesv1.GetPriceChangeSummaryRequest, rows []getPriceChangeSummaryRow) (*salesv1.GetPriceChangeSummaryResponse, error) {
	resp := &salesv1.GetPriceChangeSummaryResponse{Total: int64(len(rows))}
	var sum float64
	var rateCount int64
	var maxUp float64
	hasUp := false
	large := false

	for _, r := range rows {
		if r.DealID != nil && *r.DealID == priceChangeForcedDealID {
			resp.Forced++
		}
		switch {
		case r.NewPrice == nil:
			resp.Undecided++
			continue
		case r.OldPrice == nil:
			resp.NewProducts++
			continue
		case *r.NewPrice > *r.OldPrice:
			resp.PriceUp++
		case *r.NewPrice < *r.OldPrice:
			resp.PriceDown++
		default:
			resp.Unchanged++
		}
		if *r.OldPrice <= 0 {
			continue
		}
		n, o := int64(*r.NewPrice), int64(*r.OldPrice)
		rate := float64(n-o) / float64(o)
		sum += rate
		rateCount++
		if rate > 0 && (!hasUp || rate > maxUp) {
			maxUp = rate
			hasUp = true
		}
		diff := n - o
		if diff < 0 {
			diff = -diff
		}
		if diff*10 >= o*3 { // |rate| >= 0.3,整数比较避免浮点误差
			large = true
		}
	}

	if rateCount > 0 {
		v := roundRate4(sum / float64(rateCount))
		resp.AvgChangeRate = &v
	}
	if hasUp {
		v := roundRate4(maxUp)
		resp.MaxIncreaseRate = &v
	}
	switch {
	case len(rows) == 0:
		resp.Alert = "NO_DATA"
	case large:
		resp.Alert = "LARGE_CHANGE"
	default:
		resp.Alert = "NONE"
	}
	return resp, nil
}
