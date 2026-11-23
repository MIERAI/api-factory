package core

// 元件认证: 筛选越过 lookup 的下推(pushdown.go)和 JSON 键的索引条件(keyindex.go / expr_json_key.go)。
// 两个都是只给 SQL 的改写: 参考求值器跑原始的流水线,SQL 跑改写后的,结果必须逐条相同。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// pushFixpoint: ToSQL 里的反复下推。
func pushFixpoint(p *Pipeline) *Pipeline {
	for q := pushdown(p); q != p; q = pushdown(p) {
		p = q
	}
	return p
}

// refsOnly: 条件只引用 cols 里的列。
func refsOnly(e *Expr, cols ...string) bool {
	ok := map[string]bool{}
	for _, c := range cols {
		ok[c] = true
	}
	var walk func(e *Expr) bool
	walk = func(e *Expr) bool {
		if e.Op == "col" && !ok[e.Name] {
			return false
		}
		for _, a := range e.Args {
			if !walk(a) {
				return false
			}
		}
		return true
	}
	return walk(e)
}

// certPushLookup: 每组取一条 → 关联(keep / drop,对方的唯一列可空、找不到的行)→ 筛选。
// 只引用分组键的条件越过关联下推;引用加的列的条件(=、is_null、和键 or 在一起)和非键的条件不下推。
// 在 certLookup 之后调用(用它建的对方的表)。
func certPushLookup(env *certEnv, all func(steps ...Step) []Step) {
	t := env.t
	nt := func(name string, k Kind, nullable bool) NamedType {
		return NamedType{name, Type{Kind: k, Nullable: nullable}}
	}
	o2 := []NamedType{nt("g", Int32, false), nt("t", Text, false), nt("label", Text, true)}
	ot := []NamedType{nt("id", Int64, false), nt("u", Text, true), nt("name", Text, false)}
	byK := SortKey{Col: "k"}
	params := []ParamDecl{{Name: "gs", Type: Type{Kind: Int32}, List: true}, {Name: "pt", Type: Type{Kind: Text}},
		{Name: "pl", Type: Type{Kind: Text}}, {Name: "pi", Type: Type{Kind: Int32}}}
	conds := map[string]*Expr{
		"键(属于 + 等于)":   And(Or(Absent("gs"), InList(Col("g"), "gs")), Or(Absent("pt"), Cmp("=", Col("tt"), Param("pt")))),
		"加的列 = 参数":      Or(Absent("pl"), Cmp("=", Col("label"), Param("pl"))),
		"加的列 is_null":   IsNull(Col("label")),
		"键 or 加的列为空":    Or(Cmp("=", Col("g"), Const(int64(1), Type{Kind: Int32})), IsNull(Col("u_name"))),
		"非键的列":          Or(Absent("pi"), Cmp("=", Col("i"), Param("pi"))),
		"键 and 加的列(拆开)": And(Or(Absent("gs"), InList(Col("g"), "gs")), Not(IsNull(Col("label")))),
	}
	pss := []Params{{}, {"gs": []any{int64(1), int64(2)}}, {"pt": "a"}, {"pt": "A", "gs": []any{int64(1), int64(3)}},
		{"pl": "1a"}, {"pl": "2ab", "gs": []any{int64(2)}}, {"pi": int64(2)}, {"gs": []any{}}}
	for _, name := range sortedKeys(conds) {
		for _, m := range []string{"keep", "drop"} {
			p := &Pipeline{Params: params, Steps: all(
				Filter(Cmp("<", Col("k"), Const(int64(1000), Type{Kind: Int64}))), // 格式错的 JSON 的行(12.)不要
				PickPerKey([]string{"g", "t"}, SortKey{"k", true}),
				Project(NamedExpr{"g", Col("g")}, NamedExpr{"tt", Col("t")}, NamedExpr{"i", Col("i")}, NamedExpr{"s", Col("s")}, NamedExpr{"k", Col("k")}),
				Lookup("o2", o2, [][]string{{"g", "t"}}, []JoinKey{{"g", "g"}, {"tt", "t"}}, []TakeCol{{"label", "label"}}, m),
				Lookup("ot", ot, [][]string{{"u"}}, []JoinKey{{"s", "u"}}, []TakeCol{{"u_name", "name"}}, "keep"),
				Filter(conds[name]), Sort(byK))}
			q := pushFixpoint(p)
			// 下推到取一条之前的条件(第 3 步)只引用分组键;引用加的列的条件还在关联之后
			var pushed *Expr
			if q.Steps[2].Kind == "filter" && q.Steps[3].Kind == "pick_per_key" {
				pushed = q.Steps[2].Pred
			}
			if pushed != nil && !refsOnly(pushed, "g", "t") {
				t.Errorf("越过关联下推 %s/%s: 下推了不只引用分组键的条件 %+v", name, m, pushed)
			}
			wantPushed := strings.HasPrefix(name, "键(") || strings.HasPrefix(name, "键 and")
			if (pushed != nil) != wantPushed {
				t.Errorf("越过关联下推 %s/%s: 下推了 = %v,应该是 %v: %+v", name, m, pushed != nil, wantPushed, q.Steps)
			}
			for _, ps := range pss {
				env.check(fmt.Sprintf("越过关联下推 %s %s %v", name, m, ps), p, ps)
			}
		}
	}
	// 加的列 = 对方的键列(找到时和当前行的列相等): 找不到(keep)时是 NULL,不能当作当前行的列下推
	for _, m := range []string{"keep", "drop"} {
		for _, c := range []*Expr{Or(Absent("pt"), Cmp("=", Col("o2_t"), Param("pt"))), IsNull(Col("o2_t")), Not(IsNull(Col("o2_t")))} {
			p := &Pipeline{Params: params, Steps: all(
				Filter(Cmp("<", Col("k"), Const(int64(1000), Type{Kind: Int64}))),
				PickPerKey([]string{"g", "t"}, SortKey{"k", true}),
				Lookup("o2", o2, [][]string{{"g", "t"}}, []JoinKey{{"g", "g"}, {"t", "t"}}, []TakeCol{{"o2_t", "t"}, {"label", "label"}}, m),
				Filter(c), Sort(byK))}
			if q := pushFixpoint(p); len(q.Steps) != len(p.Steps) || q.Steps[2].Kind != "pick_per_key" {
				t.Errorf("加的键列 %s: 不应该下推: %+v", m, q.Steps)
			}
			for _, ps := range []Params{{}, {"pt": "a"}, {"pt": "B"}, {"pt": "zz"}} {
				env.check(fmt.Sprintf("加的键列 %s %s %v", m, exprText(c), ps), p, ps)
			}
		}
	}
	// 越过关联之后遇到第二个每组取一条(键不同): 停下(第一层的键 t 上的条件越过第二层会改变第二层取哪一条)
	two := &Pipeline{Params: params, Steps: all(
		Filter(Cmp("<", Col("k"), Const(int64(1000), Type{Kind: Int64}))),
		PickPerKey([]string{"g", "t"}, SortKey{"k", true}),
		Lookup("o2", o2, [][]string{{"g", "t"}}, []JoinKey{{"g", "g"}, {"t", "t"}}, []TakeCol{{"label", "label"}}, "keep"),
		PickPerKey([]string{"g"}, SortKey{"k", false}),
		Filter(Or(Absent("pt"), Cmp("=", Col("t"), Param("pt")))), Sort(byK))}
	if q := pushFixpoint(two); len(q.Steps) != len(two.Steps) || q.Steps[len(q.Steps)-2].Kind != "filter" {
		t.Errorf("越过关联之后的第二层取一条: 不应该下推: %+v", q.Steps)
	}
	for _, ps := range []Params{{}, {"pt": "a"}, {"pt": "B"}, {"pt": "ab"}} {
		env.check(fmt.Sprintf("关联 + 第二层取一条 + 第一层的键 %v", ps), two, ps)
	}
}

// certJSONKey: json_key(索引的键)的参考实现 = SQL(含不合法 / 不规范 / 超出范围的值: 两边都是 NULL,不报错),
// 以及 JSON 字段上的比较另加 json_key 的条件(keyindex.go)之后结果不变。最后调用(往 c 里插不合法的值)。
func certJSONKey(env *certEnv, all func(steps ...Step) []Step) {
	t := env.t
	k := int64(3000)
	for _, v := range []string{`"99999999999"`, `"2147483647"`, `"2147483648"`, `"-2147483648"`, `"-2147483649"`, `"9223372036854775807"`,
		`"9223372036854775808"`, `" +07 "`, `"-0"`, `"007"`, `"2026-02-30"`, `"2024-02-29"`, `"2023-02-29"`, `"0000-01-01"`, `" 2026-12-31 "`,
		`"2026-13-01"`, `"2026-00-10"`, `"9999-12-31"`, `"1.50"`, `"1.5"`, `"YES"`, `"0"`, `"abc"`, `{"x": 1}`, `[1]`, `null`, `true`, `12`, `-3.25`} {
		k++
		env.insert(Row{"k": k, "g": int64(1), "t": "a", "j": `{"v": ` + v + `}`})
	}
	for _, f := range []struct {
		key  string
		kind Kind
	}{{"n", Int32}, {"n", Int64}, {"n", Decimal}, {"n", Text}, {"s", Text}, {"b", Bool}, {"d", Decimal}, {"dt", Date}, {"missing", Int32},
		{"v", Int32}, {"v", Int64}, {"v", Decimal}, {"v", Bool}, {"v", Date}} { // 文本的 v: 嵌套的对象 / 数组参考实现不支持(和 json_field 一样)
		p := &Pipeline{Steps: all(Project(NamedExpr{"k", Col("k")}, NamedExpr{"v", JSONKey(Col("j"), f.key, Type{Kind: f.kind})}), Sort(SortKey{Col: "k"}))}
		env.check(fmt.Sprintf("json_key %s %s", f.key, f.kind), p, nil)
	}
	// JSON 字段上的比较(规范的值: k < 1000)→ 另加 json_key 的条件;结果和参考求值器(原样的流水线)一致
	cols := Project(NamedExpr{"k", Col("k")}, NamedExpr{"n", JSONField(Col("j"), "n", Type{Kind: Int32})}, NamedExpr{"n8", JSONField(Col("j"), "n", Type{Kind: Int64})},
		NamedExpr{"d", JSONField(Col("j"), "d", Type{Kind: Decimal})}, NamedExpr{"b", JSONField(Col("j"), "b", Type{Kind: Bool})},
		NamedExpr{"dt", JSONField(Col("j"), "dt", Type{Kind: Date})})
	params := []ParamDecl{{Name: "pn", Type: Type{Kind: Int32}}, {Name: "ln", Type: Type{Kind: Int64}, List: true}, {Name: "pd", Type: Type{Kind: Decimal}},
		{Name: "pb", Type: Type{Kind: Bool}}, {Name: "pdt", Type: Type{Kind: Date}}}
	for _, c := range []struct {
		col  string
		pred *Expr
		pss  []Params
	}{
		{"n", Or(Absent("pn"), Cmp("=", Col("n"), Param("pn"))), []Params{{}, {"pn": int64(1)}, {"pn": int64(2)}, {"pn": int64(7)}, {"pn": int64(10)}, {"pn": int64(3)}}},
		{"n", Cmp("<", Col("n"), Param("pn")), []Params{{"pn": int64(2)}, {"pn": int64(8)}}},
		{"n", Cmp(">=", Param("pn"), Col("n")), []Params{{"pn": int64(7)}}},
		{"n8", Or(Absent("ln"), InList(Col("n8"), "ln")), []Params{{}, {"ln": []any{int64(2), int64(7)}}, {"ln": []any{int64(10)}}, {"ln": []any{}}}},
		{"d", Or(Absent("pd"), Cmp("=", Col("d"), Param("pd"))), []Params{{}, {"pd": rat("1.5")}, {"pd": rat("3")}, {"pd": rat("10")}, {"pd": rat("-2.25")}, {"pd": rat("9")}}},
		{"b", Cmp("=", Col("b"), Param("pb")), []Params{{"pb": true}, {"pb": false}}},
		{"dt", Or(Absent("pdt"), Cmp("=", Col("dt"), Param("pdt"))), []Params{{}, {"pdt": day("2026-01-01")}, {"pdt": day("2025-12-31")}, {"pdt": day("2026-01-02")}}},
		{"dt", Cmp(">", Col("dt"), Param("pdt")), []Params{{"pdt": day("2025-12-31")}}},
	} {
		p := &Pipeline{Params: params, Steps: all(Filter(Cmp("<", Col("k"), Const(int64(1000), Type{Kind: Int64}))), cols, Filter(c.pred), Sort(SortKey{Col: "k"}))}
		q, err := ToSQL(p, Options{})
		if err != nil {
			t.Fatal(err)
		}
		var kind Kind
		for _, x := range cols.Project {
			if x.Name == c.col {
				kind = x.Expr.Type.Kind
			}
		}
		key := map[string]string{"n": "n", "n8": "n", "d": "d", "b": "b", "dt": "dt"}[c.col]
		want, _ := JSONKeySQL(fmt.Sprintf("(t1.j ->> %s)", quoteLit(key)), kind)
		if !strings.Contains(q.Text, want) {
			t.Errorf("JSON 字段的条件 %s: SQL 里没有索引的键的式子 %s\n%s", exprText(c.pred), want, q.Text)
		}
		for _, ps := range c.pss {
			env.check(fmt.Sprintf("JSON 字段的条件 + 索引的键 %s %v", exprText(c.pred), ps), p, ps)
		}
	}
	// 每组取一条之后的条件(不是分组键)不改写: 移到取一条之前会改变取哪一条
	after := &Pipeline{Params: params, Steps: all(Filter(Cmp("<", Col("k"), Const(int64(1000), Type{Kind: Int64}))), cols,
		PickPerKey([]string{"b"}, SortKey{"k", true}), Filter(Or(Absent("pn"), Cmp("=", Col("n"), Param("pn")))), Sort(SortKey{Col: "k"}))}
	if q := keyIndex(after); q != after {
		t.Errorf("取一条之后的条件不应该改写: %+v", q.Steps)
	}
	for _, ps := range []Params{{}, {"pn": int64(1)}, {"pn": int64(2)}, {"pn": int64(7)}, {"pn": int64(10)}} {
		env.check(fmt.Sprintf("取一条之后的 JSON 字段的条件 %v", ps), after, ps)
	}
}

func exprText(e *Expr) string {
	b, _ := json.Marshal(e)
	return string(b)
}

func sortedKeys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// TestJSONKeyIndexMatchesPredicate: 每一种 JSON 键的类型: 查询里的条件的式子 = 生成的索引的式子(JSONKeyIndexSQL),
// 而且 Postgres 真的用这个索引(FACTORY_TEST_DB)。不能做索引的键的类型(时刻): 生成器拒绝,条件不改写。
func TestJSONKeyIndexMatchesPredicate(t *testing.T) {
	kindsAll := []Kind{Text, Int32, Int64, Decimal, Bool, Date, Timestamp}
	samples := map[Kind][]string{ // 表里的值(JSON 的写法);第一个 = 参数
		Text:      {`"A-1"`, `"a-1"`, `"x"`, `""`},
		Int32:     {`7`, `" +07 "`, `"7"`, `8`, `"abc"`, `"99999999999"`},
		Int64:     {`7`, `"007"`, `9223372036854775807`, `"9223372036854775808"`},
		Decimal:   {`"1.5"`, `"1.50"`, `1.5`, `"NaN"`, `2`},
		Bool:      {`true`, `"yes"`, `"T"`, `false`, `"maybe"`},
		Date:      {`"2026-01-10"`, `" 2026-01-10 "`, `"2026-02-30"`, `"2026-01-11"`, `"0000-01-01"`},
		Timestamp: {`"2026-01-10T00:00:00Z"`, `"2026-01-10T09:00:00+09:00"`},
	}
	url := os.Getenv("FACTORY_TEST_DB")
	var conn *pgx.Conn
	ctx := context.Background()
	if url != "" {
		var err error
		if conn, err = pgx.Connect(ctx, url); err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
	}
	for _, kind := range kindsAll {
		t.Run(string(kind), func(t *testing.T) {
			p := &Pipeline{Params: []ParamDecl{{Name: "p", Type: Type{Kind: kind}, Required: true}}, Steps: []Step{
				Scan("kx", NamedType{"id", Type{Kind: Int64}}, NamedType{"data", Type{Kind: JSON}}),
				Project(NamedExpr{"id", Col("id")}, NamedExpr{"v", JSONField(Col("data"), "v", Type{Kind: kind})}),
				Filter(Cmp("=", Col("v"), Param("p"))),
			}}
			q, err := ToSQL(p, Options{})
			if err != nil {
				t.Fatal(err)
			}
			idx, ok := JSONKeyIndexSQL("data", "v", kind)
			if !ok {
				if kind != Timestamp {
					t.Fatalf("%s: 应该能做索引的键", kind)
				}
				if keyIndex(p) != p || strings.Contains(q.Text, "json_key") {
					t.Fatalf("%s: 不能做索引的键,条件不应该改写:\n%s", kind, q.Text)
				}
				return
			}
			pred, _ := JSONKeySQL("(t1.data ->> 'v')", kind)
			if strings.ReplaceAll(pred, "t1.data", "data") != idx || !strings.Contains(q.Text, pred) {
				t.Fatalf("%s: 查询里的式子和索引的式子不同\n索引: %s\n查询:\n%s", kind, idx, q.Text)
			}
			if conn == nil {
				return
			}
			tbl := "kx_" + string(kind)
			text := strings.ReplaceAll(q.Text, "FROM kx AS", "FROM "+tbl+" AS")
			for _, s := range []string{
				"DROP TABLE IF EXISTS " + tbl, "CREATE TEMP TABLE " + tbl + " (id int8 PRIMARY KEY, data jsonb NOT NULL)",
				"CREATE INDEX " + tbl + "_idx ON " + tbl + " (" + idx + ")",
			} {
				if _, err := conn.Exec(ctx, s); err != nil {
					t.Fatalf("%s: %v", s, err)
				}
			}
			// 不合法的值也写得进去(索引的式子不报错);大部分行是别的值(用索引才划算)
			id := 0
			for i := 0; i < 3000; i++ {
				v := samples[kind][i%len(samples[kind])]
				if i%len(samples[kind]) == 0 && i > 0 {
					v = fmt.Sprintf(`"other-%d"`, i)
					if kind == Int32 || kind == Int64 || kind == Decimal {
						v = fmt.Sprint(100000 + i)
					}
				}
				id++
				if _, err := conn.Exec(ctx, fmt.Sprintf("INSERT INTO %s VALUES ($1, $2::jsonb)", tbl), id, `{"v": `+v+`}`); err != nil {
					t.Fatalf("写入 %s: %v", v, err)
				}
			}
			if _, err := conn.Exec(ctx, "ANALYZE "+tbl); err != nil {
				t.Fatal(err)
			}
			var doc any
			dec := json.NewDecoder(strings.NewReader(`{"v": ` + samples[kind][0] + `}`))
			dec.UseNumber()
			if err := dec.Decode(&doc); err != nil {
				t.Fatal(err)
			}
			param, err := jsonField(doc, "v", kind)
			if err != nil || param == nil {
				t.Fatalf("参数 %s: %v", samples[kind][0], err)
			}
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx) //nolint:errcheck
			if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
				t.Fatal(err)
			}
			var plan string
			if err := tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+text, sqlArg(param)).Scan(&plan); err != nil {
				t.Fatalf("explain: %v\n%s", err, text)
			}
			if !strings.Contains(plan, `"Index Name": "`+tbl+`_idx"`) {
				t.Errorf("%s: Postgres 没有用生成的索引:\n%s\n%s", kind, text, plan)
			}
			_ = tx.Rollback(ctx)
		})
	}
}
