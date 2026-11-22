package design

import (
	"strings"
	"testing"

	"apifactory/factory/spec"
)

// 契约的版本规则(锁文件): 同一个版本里只能新增可选字段。
func TestContractCompatible(t *testing.T) {
	base := []lockField{{Name: "order_id", Type: "整数64", Required: true}, {Name: "items", Type: "列表", Required: true,
		Elem: []lockField{{Name: "sku", Type: "文本", Required: true}}}}
	cases := []struct {
		name string
		cur  []lockField
		want string // "" = 兼容;否则错误里要有这段
	}{
		{"same", base, ""},
		{"new optional field", append(append([]lockField{}, base...), lockField{Name: "note", Type: "文本"}), ""},
		{"new required field", append(append([]lockField{}, base...), lockField{Name: "note", Type: "文本", Required: true}), "必填"},
		{"removed field", base[:1], "删除了字段 items"},
		{"type changed", []lockField{{Name: "order_id", Type: "文本", Required: true}, base[1]}, "类型"},
		{"optional to required", []lockField{{Name: "order_id", Type: "整数64", Required: true}, {Name: "items", Type: "列表", Required: true,
			Elem: []lockField{{Name: "sku", Type: "整数32", Required: true}}}}, "元素"},
	}
	for _, c := range cases {
		got := compatible(base, c.cur)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: compatible = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestProtoNameAndInputs(t *testing.T) {
	c := &spec.Contract{Type: "inventory.reservation_failed", Version: 2}
	if got := protoName(c); got != "InventoryReservationFailedV2" {
		t.Errorf("protoName = %s", got)
	}
	in := inputsOf([]spec.ContractField{{Name: "items", Type: ListType, Required: true, Rules: map[string]string{"个数": "1~5"},
		Elem: []spec.ContractField{{Name: "sku", Type: "文本", Required: true}, {Name: "qty", Type: "整数32", Required: true}}}})
	if len(in) != 1 || in[0].Type != GroupListType || len(in[0].Group) != 2 || in[0].Constraints["个数"] != "1~5" {
		t.Errorf("inputsOf: %+v", in[0])
	}
}
