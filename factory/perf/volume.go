package perf

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"apifactory/factory/core"
)

// otherTables: 当前 schema 里没有 API 读的表(列由 allColumns 补)。
func otherTables(ctx context.Context, conn *pgx.Conn, tables map[string]*table) error {
	rows, err := conn.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		names = append(names, n)
	}
	rows.Close()
	for _, n := range names {
		if _, ok := tables[n]; ok || n == "goose_db_version" {
			continue
		}
		t := newTable(n, nil)
		if err := loadKeys(ctx, conn, t); err != nil {
			return err
		}
		tables[n] = t
	}
	return rows.Err()
}

// allColumns: 物理表的全部列(FillOnly: 没有 API 读的列也要有值,迁移排练才真实)+ 可为空的标记。
func allColumns(ctx context.Context, conn *pgx.Conn, t *table) error {
	rows, err := conn.Query(ctx, `
		SELECT column_name, udt_name, is_nullable = 'YES' FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1 ORDER BY ordinal_position`, t.name)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, udt string
		var nullable bool
		if err := rows.Scan(&name, &udt, &nullable); err != nil {
			return err
		}
		t.nullable[name] = nullable
		if slices.ContainsFunc(t.cols, func(x core.NamedType) bool { return x.Name == name }) {
			continue
		}
		k, ok := kindOfSQL(udt)
		if udt == "jsonb" || udt == "json" {
			k, ok = core.JSON, true
		}
		if !ok {
			continue // 不认识的类型: 留给默认值
		}
		t.cols = append(t.cols, core.NamedType{Name: name, Type: core.Type{Kind: k}})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	checks, err := conn.Query(ctx, `
		SELECT a.attname, pg_get_constraintdef(c.oid) FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
		WHERE c.contype = 'c' AND c.conrelid = to_regclass($1)`, pgx.Identifier{t.name}.Sanitize())
	if err != nil {
		return err
	}
	defer checks.Close()
	for checks.Next() {
		var name, def string
		if err := checks.Scan(&name, &def); err != nil {
			return err
		}
		t.checked[name] = true
		if vals := checkValues(name, def); len(vals) > 0 { // 取值列表: 当作枚举
			t.literals[name] = vals
		}
	}
	return checks.Err()
}

// dirtySQL: 在正常值 e 上掺生产里常见的"脏"值(只用约束允许的值: 唯一 / 组合键 / 枚举 / 操作列不碰)。
// 比例按行号取模,不同的列用 salt 错开: 空值 5%(只有可为空的列)、各种边界值各 1~2%。
// checked: 列在 CHECK 约束里 → 只掺空值(边界值可能违反约束,约束本身就是数据的保证)。
func dirtySQL(k core.Kind, e string, nullable, checked bool, salt int) string {
	var cases []string
	if nullable {
		cases = append(cases, fmt.Sprintf("WHEN (i + %d) %% 20 = 0 THEN NULL", salt))
	}
	m := func(mod, rem int, v string) {
		cases = append(cases, fmt.Sprintf("WHEN (i + %d) %% %d = %d THEN %s", salt, mod, rem, v))
	}
	if checked {
		k = ""
	}
	switch k {
	case core.Text:
		m(50, 1, "''")                    // 空字符串
		m(50, 2, "'  ' || "+e+" || '  '") // 前后空格
		m(100, 3, "repeat('长', 300)")     // 超长
		m(100, 4, "'带''引号\"和\\\\反斜杠🙂'")   // 特殊字符
	case core.Int32, core.Int64:
		m(50, 1, "0")
		m(50, 2, "-("+e+")")
	case core.Decimal:
		m(50, 1, "0")
		m(50, 2, "-("+e+")")
		m(100, 3, "("+e+") + 0.005") // 舍入的边界
	case core.Date:
		m(100, 1, "date '1970-01-01'")
		m(100, 2, "date '2099-12-31'")
	case core.Timestamp:
		m(100, 1, "timestamptz '1970-01-01 00:00:00+00'")
	}
	if len(cases) == 0 {
		return e
	}
	return "CASE " + joinSpace(cases) + " ELSE " + e + " END"
}

func joinSpace(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += " "
		}
		out += x
	}
	return out
}

var (
	checkAnyRe = regexp.MustCompile(`^CHECK \(\(\(?(\w+)\)?(?:::\w+)? = ANY \(\(?ARRAY\[(.*)\]\)?(?:::\w+\[\])?\)\)\)$`)
	checkEqRe  = regexp.MustCompile(`^CHECK \(\((\w+) = ('[^']*'(?:::\w+)?|-?\d+)\)\)$`)
	literalRe  = regexp.MustCompile(`'((?:[^']|'')*)'(?:::[\w ]+)?|(-?\d+(?:\.\d+)?)`)
)

// checkValues: CHECK (col = ANY (ARRAY[...])) / CHECK (col = 'x') 的取值列表(SQL 字面量,原样用在 INSERT 里)。
func checkValues(col, def string) []string {
	var list string
	if m := checkAnyRe.FindStringSubmatch(def); m != nil && m[1] == col {
		list = m[2]
	} else if m := checkEqRe.FindStringSubmatch(def); m != nil && m[1] == col {
		list = m[2]
	} else {
		return nil
	}
	var out []string
	for _, m := range literalRe.FindAllStringSubmatch(list, -1) {
		if m[2] != "" {
			out = append(out, m[2])
		} else {
			out = append(out, quote(strings.ReplaceAll(m[1], "''", "'")))
		}
	}
	return out
}
