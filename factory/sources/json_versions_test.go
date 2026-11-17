package sources

import (
	"strings"
	"testing"

	"apifactory/factory/cells"
	"apifactory/factory/core"
	"apifactory/factory/spec"
)

// 外壳列: 物理表的列,不在 JSON 里。写错的几种情况要在编译时报出来
func TestEnvelopeColumn(t *testing.T) {
	lib, err := cells.Load()
	if err != nil {
		t.Fatal(err)
	}
	scan := core.Scan("ev",
		core.NamedType{Name: "id", Type: core.Type{Kind: core.Int64}},
		core.NamedType{Name: "data", Type: core.Type{Kind: core.Text}},
		core.NamedType{Name: "action_type", Type: core.Type{Kind: core.Int32}},
		core.NamedType{Name: "registered_at", Type: core.Type{Kind: core.Timestamp}})
	st := func(f ...spec.StorageField) *spec.Storage {
		return &spec.Storage{Table: "ev", DataColumn: "data", ActionColumn: "action_type", OrderBy: []string{"registered_at", "id"},
			Fields: append([]spec.StorageField{{Name: "code", Type: "text"}}, f...)}
	}
	for _, c := range []struct {
		name string
		f    spec.StorageField
		want string
	}{
		{"ok", spec.StorageField{Name: "at", Type: "timestamptz", Column: "registered_at"}, ""},
		{"没有这列", spec.StorageField{Name: "at", Type: "timestamptz", Column: "updated_at"}, "没有列"},
		{"类型不一致", spec.StorageField{Name: "at", Type: "text", Column: "registered_at"}, "声明的类型"},
		{"JSON 列", spec.StorageField{Name: "at", Type: "text", Column: "data"}, "外壳列不能"},
	} {
		_, err := jsonVersionsSteps(st(c.f), []string{"code"}, scan, lib)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	// 自然键不能是外壳列
	_, err = jsonVersionsSteps(st(spec.StorageField{Name: "at", Type: "timestamptz", Column: "registered_at"}), []string{"code", "at"}, scan, lib)
	if err == nil || !strings.Contains(err.Error(), "外壳列不能") {
		t.Errorf("自然键: err = %v", err)
	}
}
