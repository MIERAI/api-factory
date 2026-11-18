package sales

import (
	"testing"
	"time"

	salesv1 "apifactory/gen/factory/sales/v1"
)

func strp(s string) *string { return &s }

func TestGetDailySalesSummaryLogic(t *testing.T) {
	closed := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	req := &salesv1.GetDailySalesSummaryRequest{}

	t.Run("open with null discount", func(t *testing.T) {
		resp, err := getDailySalesSummaryLogic(req, []getDailySalesSummaryRow{
			{StoreCode: 1, Amount: "100.50", Discount: strp("0.50"), ClosedOn: &closed},
			{StoreCode: 2, Amount: "200", ClosedOn: &closed},
			{StoreCode: 3, Amount: "50.25", Discount: strp("1.25")},
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.TotalAmount != "350.75" || resp.TotalDiscount != "1.75" || resp.NetAmount != "349" ||
			resp.Status != "OPEN" || resp.UnclosedStores != 1 || resp.StoreCount != 3 ||
			resp.DiscountRate == nil || *resp.DiscountRate != 0.005 {
			t.Fatalf("unexpected: %v", resp)
		}
	})

	t.Run("no data", func(t *testing.T) {
		resp, err := getDailySalesSummaryLogic(req, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Status != "NO_DATA" || resp.NetAmount != "0" || resp.DiscountRate != nil {
			t.Fatalf("unexpected: %v", resp)
		}
	})

	t.Run("half rounds away from zero", func(t *testing.T) {
		resp, err := getDailySalesSummaryLogic(req, []getDailySalesSummaryRow{
			{StoreCode: 1, Amount: "10000", Discount: strp("0.5"), ClosedOn: &closed},
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.DiscountRate == nil || *resp.DiscountRate != 0.0001 {
			t.Fatalf("unexpected: %v", resp)
		}
	})
}
