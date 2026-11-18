// Logic for specs/sales/get-daily-sales-summary/spec.yaml — implemented by AI.

package sales

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	salesv1 "apifactory/gen/factory/sales/v1"
)

// formatDecimalRat 按十进制输出,去掉小数末尾的 0,整数不带小数点。
func formatDecimalRat(r *big.Rat) string {
	s := r.FloatString(30)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	if s == "-0" || s == "" {
		return "0"
	}
	return s
}

// getDailySalesSummaryLogic 实现 GetDailySalesSummary 的业务逻辑(式样书 behavior 节)。
// 纯函数: 不访问 DB/网络/文件,不用 context,不调用 time.Now,不用全局可变状态。
func getDailySalesSummaryLogic(req *salesv1.GetDailySalesSummaryRequest, rows []getDailySalesSummaryRow) (*salesv1.GetDailySalesSummaryResponse, error) {
	totalAmount := new(big.Rat)
	totalDiscount := new(big.Rat)
	var unclosed int64
	for _, row := range rows {
		a, ok := new(big.Rat).SetString(strings.TrimSpace(row.Amount))
		if !ok {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("invalid amount %q (store %d)", row.Amount, row.StoreCode))
		}
		totalAmount.Add(totalAmount, a)
		if row.Discount != nil {
			d, ok := new(big.Rat).SetString(strings.TrimSpace(*row.Discount))
			if !ok {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("invalid discount %q (store %d)", *row.Discount, row.StoreCode))
			}
			totalDiscount.Add(totalDiscount, d)
		}
		if row.ClosedOn == nil {
			unclosed++
		}
	}

	resp := &salesv1.GetDailySalesSummaryResponse{
		StoreCount:     int64(len(rows)),
		TotalAmount:    formatDecimalRat(totalAmount),
		TotalDiscount:  formatDecimalRat(totalDiscount),
		NetAmount:      formatDecimalRat(new(big.Rat).Sub(totalAmount, totalDiscount)),
		UnclosedStores: unclosed,
	}

	switch {
	case len(rows) == 0:
		resp.Status = "NO_DATA"
	case unclosed > 0:
		resp.Status = "OPEN"
	default:
		resp.Status = "CLOSED"
	}

	if totalAmount.Sign() != 0 {
		x := new(big.Rat).Quo(totalDiscount, totalAmount)
		x.Mul(x, big.NewRat(10000, 1))
		neg := x.Sign() < 0
		x.Abs(x)
		x.Add(x, big.NewRat(1, 2))
		n := new(big.Int).Quo(x.Num(), x.Denom()) // floor (非负)
		if neg {
			n.Neg(n)
		}
		scaled := new(big.Rat).SetFrac(n, big.NewInt(10000))
		f, err := strconv.ParseFloat(scaled.FloatString(4), 64)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		resp.DiscountRate = &f
	}
	return resp, nil
}
