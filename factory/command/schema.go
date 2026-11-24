// Package command 是命令工厂(写入)的参考实现: 内存里的模型状态(每张表的行),按设计稿 §3 的语义执行写操作,
// 不调用数据库、不看生成的代码(独立实现)。状态差分测试(factory/accept/command_diff.go)对同一串请求比较
// 模型和数据库: 每个请求的响应(含错误码)+ 相关的表的完整状态。
//
// 模型重现的数据库的效果: 主键 / 唯一约束(包括唯一索引)、NOT NULL、列的默认值(常量;now() / 序号等
// 由数据库生成的值是"生成值",和数据库比较时只要求不为空,之后采用数据库的值)、外键(只做"不存在 / 还被引用"
// 的检查,没有 ON DELETE 的动作)。CHECK 约束不重现: 合法的请求(protovalidate)碰到 CHECK
// 说明设计稿漏了规则 —— 差分测试会因此失败,这正是要发现的问题(设计稿 §3.3)。
package command

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Column: 表的一列(数据库的目录)。
type Column struct {
	Name    string
	Type    string // pg_type.typname: int4 / int8 / text / numeric / timestamptz / bool / date …
	NotNull bool
	// Default: 列的默认值。nil = 没有(→ NULL);Generated = 数据库生成(now()、序号 …,模型不知道具体的值)
	Default *Default
}

type Default struct {
	Value     any // 规范值(Canon)
	Generated bool
}

// FK: 外键(本表的列 → 对方的表的列)。
type FK struct {
	Name     string
	Cols     []string
	RefTable string
	RefCols  []string
}

// Table: 一张表的目录。
type Table struct {
	Name    string
	Cols    []Column
	PK      []string
	Uniques [][]string // 主键 + 唯一约束 / 唯一索引(没有条件的)
	FKs     []FK       // 本表引用别的表
	RefBy   []FK       // 别的表引用本表(Name = 引用方的表 + 列,RefTable = 引用方的表)
	// Bounds: CHECK 约束里 "列 比较 整数" 的形状(held >= 0 …)。别的 CHECK 模型不重现(输入的约束覆盖,G4)
	Bounds []Bound
	// ReadFrom: 读行的时候读这个(分桶的表: 视图 factory_hot_<表>,数量 = 桶的合计;docs/design/hotspots.md)
	ReadFrom string
}

// Bound: CHECK (列 Op N)。
type Bound struct {
	Name, Col, Op string
	N             int64
}

var reBound = regexp.MustCompile(`^CHECK \(\(([a-z_][a-z0-9_]*) (>=|>|<=|<) \(?(-?[0-9]+)\)?\)\)$`)

func (t *Table) Col(name string) *Column {
	for i := range t.Cols {
		if t.Cols[i].Name == name {
			return &t.Cols[i]
		}
	}
	return nil
}

// LoadTable: 从数据库的目录读一张表(当前的 schema)。
func LoadTable(ctx context.Context, conn *pgx.Conn, name string) (*Table, error) {
	t := &Table{Name: name}
	rows, err := conn.Query(ctx, `SELECT a.attname, t.typname, a.attnotnull, coalesce(pg_get_expr(d.adbin, d.adrelid), ''),
			a.attidentity <> '' OR a.attgenerated <> ''
		FROM pg_attribute a JOIN pg_type t ON t.oid = a.atttypid
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, pgx.Identifier{name}.Sanitize())
	if err != nil {
		return nil, fmt.Errorf("table %s: %w", name, err)
	}
	for rows.Next() {
		var c Column
		var def string
		var generated bool
		if err := rows.Scan(&c.Name, &c.Type, &c.NotNull, &def, &generated); err != nil {
			rows.Close()
			return nil, err
		}
		switch {
		case generated:
			c.Default = &Default{Generated: true}
		case def != "":
			c.Default = parseDefault(c.Type, def)
		}
		t.Cols = append(t.Cols, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(t.Cols) == 0 {
		return nil, fmt.Errorf("table %s: no columns", name)
	}
	// 分桶的表(docs/design/hotspots.md): 行从视图读(数量 = 桶的合计)
	var view *string
	if err := conn.QueryRow(ctx, `SELECT to_regclass($1)::text`, "factory_hot_"+name).Scan(&view); err == nil && view != nil {
		t.ReadFrom = *view
	}
	rows, err = conn.Query(ctx, `SELECT i.indisprimary, array_agg(a.attname ORDER BY k.n)
		FROM pg_index i CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, n)
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		WHERE i.indrelid = $1::regclass AND i.indisunique AND i.indpred IS NULL AND i.indexprs IS NULL
		GROUP BY i.indexrelid, i.indisprimary ORDER BY i.indisprimary DESC, i.indexrelid`, pgx.Identifier{name}.Sanitize())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var primary bool
		var cols []string
		if err := rows.Scan(&primary, &cols); err != nil {
			rows.Close()
			return nil, err
		}
		if primary {
			t.PK = cols
		}
		t.Uniques = append(t.Uniques, cols)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(t.PK) == 0 {
		return nil, fmt.Errorf("table %s: no primary key (commands write by primary key)", name)
	}
	rows, err = conn.Query(ctx, `SELECT c.conname, c.conrelid::regclass::text, c.confrelid::regclass::text,
			(SELECT array_agg(a.attname ORDER BY x.n) FROM unnest(c.conkey) WITH ORDINALITY x(k, n) JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = x.k),
			(SELECT array_agg(a.attname ORDER BY x.n) FROM unnest(c.confkey) WITH ORDINALITY x(k, n) JOIN pg_attribute a ON a.attrelid = c.confrelid AND a.attnum = x.k)
		FROM pg_constraint c WHERE c.contype = 'f' AND (c.conrelid = $1::regclass OR c.confrelid = $1::regclass) ORDER BY c.conname`, pgx.Identifier{name}.Sanitize())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var fk FK
		var from, to string
		var refCols []string
		if err := rows.Scan(&fk.Name, &from, &to, &fk.Cols, &refCols); err != nil {
			rows.Close()
			return nil, err
		}
		from, to = strings.Trim(from, `"`), strings.Trim(to, `"`)
		if from == name {
			t.FKs = append(t.FKs, FK{Name: fk.Name, Cols: fk.Cols, RefTable: to, RefCols: refCols})
		}
		if to == name {
			t.RefBy = append(t.RefBy, FK{Name: fk.Name, Cols: refCols, RefTable: from, RefCols: fk.Cols})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = conn.Query(ctx, `SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint WHERE contype = 'c' AND conrelid = $1::regclass ORDER BY conname`, pgx.Identifier{name}.Sanitize())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var cn, def string
		if err := rows.Scan(&cn, &def); err != nil {
			rows.Close()
			return nil, err
		}
		if m := reBound.FindStringSubmatch(def); m != nil {
			n, _ := strconv.ParseInt(m[3], 10, 64)
			t.Bounds = append(t.Bounds, Bound{Name: cn, Col: m[1], Op: m[2], N: n})
		}
	}
	rows.Close()
	return t, rows.Err()
}

// parseDefault: 列的默认值的表达式 → 常量(模型可以重现)或者"数据库生成"。
// 常量: 'abc'::text、1、-1、1.5、true / false;其他(now()、nextval(…)、函数)= 生成值。
func parseDefault(typ, expr string) *Default {
	e := strings.TrimSpace(expr)
	if i := strings.Index(e, "::"); i > 0 && strings.HasPrefix(e, "'") {
		e = e[:i]
	}
	e = strings.TrimSuffix(strings.TrimPrefix(e, "("), ")")
	if strings.HasPrefix(e, "'") && strings.HasSuffix(e, "'") && len(e) >= 2 {
		lit := strings.ReplaceAll(e[1:len(e)-1], "''", "'")
		if v, err := Canon(typ, lit); err == nil {
			return &Default{Value: v}
		}
		return &Default{Generated: true}
	}
	if v, err := Canon(typ, e); err == nil && (isNumeric(e) || e == "true" || e == "false") {
		return &Default{Value: v}
	}
	return &Default{Generated: true}
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !(r >= '0' && r <= '9' || r == '.' || (i == 0 && r == '-')) {
			return false
		}
	}
	return true
}

// ReadRows: 表的全部行(规范值)。每列读成文本再按类型换成规范形(和模型、期待值同一种写法)。
// 连接的 TimeZone 要是 UTC(验收的连接已经设置)。
func ReadRows(ctx context.Context, conn *pgx.Conn, t *Table) ([]Row, error) {
	sel := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		sel[i] = pgx.Identifier{c.Name}.Sanitize() + "::text"
	}
	// 按主键排序(没有主键: 按全部列): 行的顺序要确定 —— 验收从这些行里随机挑(同一个种子要造出同样的数据),
	// 不排序时是堆的物理顺序,UPDATE / autovacuum 之后会变
	order := make([]string, 0, len(t.Cols))
	for _, k := range t.PK {
		order = append(order, pgx.Identifier{k}.Sanitize())
	}
	if len(order) == 0 {
		for i := range t.Cols {
			order = append(order, fmt.Sprint(i+1))
		}
	}
	from := t.Name
	if t.ReadFrom != "" {
		from = t.ReadFrom
	}
	rows, err := conn.Query(ctx, "SELECT "+strings.Join(sel, ", ")+" FROM "+pgx.Identifier{from}.Sanitize()+" ORDER BY "+strings.Join(order, ", "))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		vals := make([]*string, len(t.Cols))
		ptrs := make([]any, len(t.Cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := Row{}
		for i, c := range t.Cols {
			if vals[i] == nil {
				r[c.Name] = nil
				continue
			}
			v, err := Canon(c.Type, *vals[i])
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", t.Name, c.Name, err)
			}
			r[c.Name] = v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
