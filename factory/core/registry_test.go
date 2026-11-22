package core

import "testing"

// 登记表的完整性: 每个表达式元件都有三份实现(检查 / 参考求值 / SQL)。sql_ref 只在 SQL 后端内部出现(没有参考实现)。
func TestRegistryComplete(t *testing.T) {
	for op, x := range exprPrims {
		if x.typeOf == nil || x.sql == nil || (x.eval == nil && op != "sql_ref") {
			t.Errorf("expr %s: typeOf / eval / sql 不全", op)
		}
	}
	for _, k := range []string{"scan", "filter", "project", "pick_per_key", "sort", "limit", "lookup", "nest", "child_agg", "group", "rank"} {
		if stepPrims[k] == nil {
			t.Errorf("step %s 没有登记", k)
		}
	}
}
