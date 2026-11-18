package perf

import (
	"reflect"
	"testing"
)

func TestCheckValues(t *testing.T) {
	for _, c := range []struct {
		col, def string
		want     []string
	}{
		{"currency", "CHECK ((currency = 'JPY'::text))", []string{"'JPY'"}},
		{"status", "CHECK ((status = ANY (ARRAY[0, 1, 2])))", []string{"0", "1", "2"}},
		{"kind", "CHECK ((kind = ANY (ARRAY['a'::text, 'b''c'::text])))", []string{"'a'", "'b''c'"}},
		{"amount", "CHECK ((amount >= (0)::numeric))", nil},
		{"x", "CHECK ((status = ANY (ARRAY[0, 1])))", nil}, // 别的列
	} {
		if got := checkValues(c.col, c.def); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v want %v", c.def, got, c.want)
		}
	}
}
