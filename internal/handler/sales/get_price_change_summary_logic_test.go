package sales

import "testing"

func i32(v int32) *int32 { return &v }

func TestPriceChangeSummaryRounding(t *testing.T) {
	// 3 件 300→400 (1/3) + 5000→5001: 平均 0.25005 → 0.2501
	var rows []getPriceChangeSummaryRow
	for i := 0; i < 3; i++ {
		rows = append(rows, getPriceChangeSummaryRow{NewPrice: i32(400), OldPrice: i32(300)})
	}
	rows = append(rows, getPriceChangeSummaryRow{NewPrice: i32(5001), OldPrice: i32(5000)})
	resp, _ := getPriceChangeSummaryLogic(nil, rows)
	if resp.AvgChangeRate == nil || *resp.AvgChangeRate != 0.2501 {
		t.Fatalf("avg = %v", resp.AvgChangeRate)
	}
	if resp.Alert != "LARGE_CHANGE" {
		t.Fatalf("alert = %s", resp.Alert)
	}
}

func TestPriceChangeSummaryTinyIncrease(t *testing.T) {
	resp, _ := getPriceChangeSummaryLogic(nil, []getPriceChangeSummaryRow{{NewPrice: i32(30001), OldPrice: i32(30000)}})
	if resp.MaxIncreaseRate == nil || *resp.MaxIncreaseRate != 0 {
		t.Fatalf("max = %v", resp.MaxIncreaseRate)
	}
}
