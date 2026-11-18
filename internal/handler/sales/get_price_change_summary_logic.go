// Logic for specs/sales/get-price-change-summary/spec.yaml — implemented by AI.

package sales

import (
	"math/big"

	salesv1 "apifactory/gen/factory/sales/v1"
)

const priceChangeForcedDealID int64 = 99999999

// roundRate4 把精确分数四舍五入到小数 4 位(0.5 向远离 0 的方向),结果为 0 时返回无符号的 0。
func roundRate4(x *big.Rat) float64 {
	scaled := new(big.Rat).Mul(x, big.NewRat(10000, 1))
	neg := scaled.Sign() < 0
	if neg {
		scaled.Neg(scaled)
	}
	scaled.Add(scaled, big.NewRat(1, 2))
	n := new(big.Int).Quo(scaled.Num(), scaled.Denom()) // floor(正数)
	if n.Sign() == 0 {
		return 0
	}
	f := float64(n.Int64()) / 10000
	if neg {
		f = -f
	}
	return f
}

// getPriceChangeSummaryLogic 实现 GetPriceChangeSummary 的业务逻辑(式样书 behavior 节)。
// 纯函数: 不访问 DB/网络/文件,不用 context,不调用 time.Now,不用全局可变状态。
func getPriceChangeSummaryLogic(req *salesv1.GetPriceChangeSummaryRequest, rows []getPriceChangeSummaryRow) (*salesv1.GetPriceChangeSummaryResponse, error) {
	resp := &salesv1.GetPriceChangeSummaryResponse{Total: int64(len(rows))}
	sum := new(big.Rat)
	var rateCount int64
	var maxUp *big.Rat
	large := false
	threshold := big.NewRat(3, 10)

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
		rate := big.NewRat(n-o, o)
		sum.Add(sum, rate)
		rateCount++
		if rate.Sign() > 0 && (maxUp == nil || rate.Cmp(maxUp) > 0) {
			maxUp = rate
		}
		if new(big.Rat).Abs(rate).Cmp(threshold) >= 0 {
			large = true
		}
	}

	if rateCount > 0 {
		avg := new(big.Rat).Quo(sum, big.NewRat(rateCount, 1))
		v := roundRate4(avg)
		resp.AvgChangeRate = &v
	}
	if maxUp != nil {
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
