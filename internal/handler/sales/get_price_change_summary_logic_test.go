package sales

import "testing"

func pcRow(n, o *int32, d *int64) getPriceChangeSummaryRow {
	return getPriceChangeSummaryRow{NewPrice: n, OldPrice: o, DealID: d}
}

func i32(v int32) *int32 { return &v }
func i64(v int64) *int64 { return &v }

func TestPriceChangeSummaryAll(t *testing.T) {
	rows := []getPriceChangeSummaryRow{
		pcRow(i32(98), i32(108), nil),
		pcRow(i32(150), i32(150), nil),
		pcRow(i32(150), i32(150), i64(99999999)),
		pcRow(i32(200), nil, nil),
		pcRow(nil, i32(120), nil),
		pcRow(i32(130), i32(100), nil),
		pcRow(i32(101), i32(100), nil),
	}
	r, err := getPriceChangeSummaryLogic(nil, rows)
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 7 || r.PriceUp != 2 || r.PriceDown != 1 || r.Unchanged != 2 || r.NewProducts != 1 || r.Undecided != 1 || r.Forced != 1 {
		t.Fatalf("counts: %+v", r)
	}
	if r.AvgChangeRate == nil || *r.AvgChangeRate != 0.0435 || r.MaxIncreaseRate == nil || *r.MaxIncreaseRate != 0.3 || r.Alert != "LARGE_CHANGE" {
		t.Fatalf("rates: %+v", r)
	}
}

func TestPriceChangeSummaryEdge(t *testing.T) {
	r, _ := getPriceChangeSummaryLogic(nil, nil)
	if r.Alert != "NO_DATA" || r.AvgChangeRate != nil || r.MaxIncreaseRate != nil {
		t.Fatalf("empty: %+v", r)
	}
	r, _ = getPriceChangeSummaryLogic(nil, []getPriceChangeSummaryRow{pcRow(i32(50), i32(0), nil)})
	if r.PriceUp != 1 || r.AvgChangeRate != nil || r.Alert != "NONE" {
		t.Fatalf("zero old: %+v", r)
	}
	r, _ = getPriceChangeSummaryLogic(nil, []getPriceChangeSummaryRow{pcRow(i32(12999), i32(10000), nil)})
	if r.Alert != "NONE" || *r.MaxIncreaseRate != 0.2999 {
		t.Fatalf("29.99: %+v", r)
	}
	r, _ = getPriceChangeSummaryLogic(nil, []getPriceChangeSummaryRow{pcRow(i32(60), i32(100), nil)})
	if r.Alert != "LARGE_CHANGE" || *r.AvgChangeRate != -0.4 || r.MaxIncreaseRate != nil {
		t.Fatalf("down: %+v", r)
	}
}
