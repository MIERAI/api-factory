package core

import "testing"

// 参数名 / 列名是 SQL 的关键字(limit、from、order …)时: sqlc 的参数写成文本,列名加双引号(sqlc 的解析器和 Postgres 都能读)。
// 普通的名字原样(以前生成的 SQL 一个字节也不变)。
func TestSQLReservedNames(t *testing.T) {
	for _, c := range []struct {
		d    ParamDecl
		want string
	}{
		{ParamDecl{Name: "store_code", Required: true}, "sqlc.arg(store_code)"},
		{ParamDecl{Name: "status"}, "sqlc.narg(status)"},
		{ParamDecl{Name: "limit"}, "sqlc.narg('limit')"},
		{ParamDecl{Name: "from", Required: true}, "sqlc.arg('from')"},
		{ParamDecl{Name: "to", List: true}, "sqlc.arg('to')"},
		{ParamDecl{Name: "order"}, "sqlc.narg('order')"},
		{ParamDecl{Name: "user", Required: true}, "sqlc.arg('user')"},
		{ParamDecl{Name: "time"}, "sqlc.narg('time')"},
	} {
		if got := Sqlc(c.d, 1); got != c.want {
			t.Errorf("%s: %s,期待 %s", c.d.Name, got, c.want)
		}
	}
	for name, want := range map[string]string{"name": "name", "limit": `"limit"`, "between": `"between"`, "valid_from": "valid_from", "Code": `"Code"`} {
		if got := quoteIdent(name); got != want {
			t.Errorf("quoteIdent(%s) = %s,期待 %s", name, got, want)
		}
	}
	// 关键字的参数在 SQL 里每处都一样写(sqlc 按名字合并成一个参数)
	p := &Pipeline{Params: []ParamDecl{{Name: "limit", Type: Type{Kind: Int32}}}, Steps: []Step{
		Scan("c", NamedType{"k", Type{Kind: Int64}}, NamedType{"limit", Type{Kind: Int32, Nullable: true}}),
		Filter(Or(Absent("limit"), Cmp("<=", Col("limit"), Param("limit")))),
	}}
	s, err := ToSQL(p, Options{ParamRef: Sqlc})
	if err != nil || !contains(s.Text, `(t1."limit" <= sqlc.narg('limit')::int4)`) || !contains(s.Text, `(sqlc.narg('limit')::int4 IS NULL)`) {
		t.Errorf("%v\n%s", err, s.Text)
	}
}
