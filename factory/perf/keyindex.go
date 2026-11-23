package perf

// JSON版本 的表的表达式索引 = 查询的式子(core.JSONKeyIndexSQL)。
//
// 查询里 JSON 键上的比较条件写成 json_key(core/keyindex.go),索引要用逐字相同的式子才用得上。手写的索引
// ((data ->> 'store_code'))对整数的键是用不上的(查询里的式子带校验和转换)。这里把 schema 里每个用到 JSON 键的
// 索引和工厂生成的式子(交给 Postgres 规范化之后)比较: 不同 → ✗,并给出该用的 CREATE INDEX。
// 文本的键: 生成的式子就是 (data ->> 'key'),COLLATE "C" 的索引也包含它。

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"apifactory/factory/core"
	"apifactory/factory/ir"
	"apifactory/factory/kinds"
)

// keyIndexes: 每张 JSON版本 的物理表的每个索引里引用的 JSON 键 → 是不是生成的式子。
func keyIndexes(ctx context.Context, conn *pgx.Conn, plan *ir.Plan) ([]Report, error) {
	var out []Report
	seen := map[string]bool{}
	for _, st := range plan.Storages {
		if st.Kind != "json_versions" || seen[st.Table] {
			continue
		}
		seen[st.Table] = true
		rows, err := conn.Query(ctx, `SELECT c.relname, pg_get_indexdef(i.indexrelid) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
			WHERE i.indrelid = to_regclass($1) ORDER BY 1`, st.Table)
		if err != nil {
			return nil, err
		}
		type index struct{ name, def string }
		var idx []index
		for rows.Next() {
			var x index
			if err := rows.Scan(&x.name, &x.def); err != nil {
				rows.Close()
				return nil, err
			}
			idx = append(idx, x)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, f := range st.Fields {
			if f.Column != "" { // 外壳列: 普通的列
				continue
			}
			key := f.Key
			if key == "" {
				key = f.Name
			}
			k, ok := kinds.BySQL(f.Type)
			if !ok || k.Core == "" {
				continue
			}
			ref := regexp.MustCompile(regexp.QuoteMeta(st.DataColumn+" ->> '"+strings.ReplaceAll(key, "'", "''")+"'::text"))
			want, ok := core.JSONKeyIndexSQL(st.DataColumn, key, k.Core)
			var norm string
			for _, x := range idx {
				if !ref.MatchString(x.def) {
					continue
				}
				r := Report{API: "(schema) " + st.Table, Variant: x.name, Plan: x.def}
				if !ok {
					r.Fail = append(r.Fail, fmt.Sprintf("索引 %s 用了 JSON 键 %s(%s): 这个类型不能做表达式索引的键(查询里是带校验的转换)", x.name, key, k.Core))
					out = append(out, r)
					continue
				}
				if norm == "" {
					if norm, err = normalized(ctx, conn, st.Table, want); err != nil {
						return nil, err
					}
				}
				if !strings.Contains(x.def, norm) {
					r.Fail = append(r.Fail, fmt.Sprintf("索引 %s 用了 JSON 键 %s(%s),但不是查询里的式子,用不上。改成生成的式子: CREATE INDEX … ON %s (%s, %s)",
						x.name, key, k.Core, st.Table, want, orderCols(st)))
				}
				out = append(out, r)
			}
		}
	}
	return out, nil
}

// normalized: Postgres 规范化之后的索引的式子(在临时表上建一次索引,读 pg_get_indexdef)。
func normalized(ctx context.Context, conn *pgx.Conn, table, expr string) (string, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // 只读规范化的结果,不留东西
	if _, err := tx.Exec(ctx, fmt.Sprintf("CREATE TEMP TABLE _factory_key (LIKE %s) ON COMMIT DROP", pgx.Identifier{table}.Sanitize())); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("CREATE INDEX _factory_key_idx ON _factory_key (%s)", expr)); err != nil {
		return "", fmt.Errorf("生成的索引的式子 %s: %w", expr, err)
	}
	var def string
	if err := tx.QueryRow(ctx, "SELECT pg_get_indexdef('_factory_key_idx'::regclass)").Scan(&def); err != nil {
		return "", err
	}
	// CREATE INDEX _factory_key_idx ON pg_temp_3._factory_key USING btree (式子) → 式子
	i := strings.Index(def, "USING btree (")
	if i < 0 || !strings.HasSuffix(def, ")") {
		return "", fmt.Errorf("pg_get_indexdef: %s", def)
	}
	return def[i+len("USING btree (") : len(def)-1], nil
}

func orderCols(st ir.Storage) string {
	var cols []string
	for _, c := range st.OrderBy {
		cols = append(cols, c+" DESC")
	}
	return strings.Join(cols, ", ")
}
