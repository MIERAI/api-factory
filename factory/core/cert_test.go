package core

// 元件认证: 每个基本元件在小值域上穷举,比较 Postgres 上 SQL 后端的结果和参考求值器的结果。
// 值域故意包含陷阱: NULL、"10" vs "9"(字典序)、% _(通配符)、大小写(排序规则)、空字符串、非 ASCII。
//
// 需要数据库: FACTORY_TEST_DB=postgres://... go test ./factory/core
// 认证通过后,单元库里的组合不再逐个怀疑(组合的正确性由差分测试在 API 层面再确认)。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

var certCols = []NamedType{
	{"k", Type{Kind: Int64}}, {"g", Type{Kind: Int32}}, {"t", Type{Kind: Text}},
	{"i", Type{Kind: Int32, Nullable: true}}, {"d", Type{Kind: Decimal, Nullable: true}},
	{"s", Type{Kind: Text, Nullable: true}}, {"b", Type{Kind: Bool, Nullable: true}},
	{"dt", Type{Kind: Date, Nullable: true}}, {"ts", Type{Kind: Timestamp, Nullable: true}},
	{"j", Type{Kind: JSON, Nullable: true}},
}

func rat(s string) *big.Rat { r, _ := new(big.Rat).SetString(s); return r }
func day(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return t
}
func ts(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t.UTC()
}

var certDomain = map[string][]any{
	"g":  {int64(1), int64(2), int64(3)},
	"t":  {"a", "B", "b", "ab", "A"},
	"i":  {nil, int64(1), int64(2), int64(10)},
	"d":  {nil, rat("1.5"), rat("9"), rat("10"), rat("-2")},
	"s":  {nil, "a", "b", "B", "a%", "a_", "ab", "", "é", "a%c", "abc"},
	"b":  {nil, true, false},
	"dt": {nil, day("2026-01-01"), day("2026-01-02"), day("2025-12-31")},
	"ts": {nil, ts("2026-01-01T00:00:00Z"), ts("2026-01-01T09:00:00Z"), ts("2025-12-31T23:59:59Z")},
	"j": {nil,
		`{"n": 1, "s": "x", "b": true, "d": "1.50", "dt": "2026-01-01"}`,
		`{"n": "2", "s": "a", "b": "yes", "d": 9, "dt": "2025-12-31"}`,
		`{"n": null, "b": "f", "d": "10"}`,
		`{}`,
		`{"n": 10, "s": "", "b": false, "d": -2.25}`,
		`{"n": " 7 ", "s": "10", "d": "  3.0  "}`,
	},
}

const certRows = 60

type certEnv struct {
	table  string // 空 = "c"
	t      *testing.T
	ctx    context.Context
	conn   *pgx.Conn
	rows   []Row
	n      int
	others map[string][]Row // lookup / nest 的对方的表(表名 → 行)
	extra  map[string][]Row // 另外的表(group 的精度 / 溢出用)
}

func TestCertification(t *testing.T) {
	url := os.Getenv("FACTORY_TEST_DB")
	if url == "" {
		t.Skip("FACTORY_TEST_DB 没有设置(元件认证需要 Postgres)")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	schema := fmt.Sprintf("core_cert_%d", os.Getpid())
	for _, q := range []string{
		"DROP SCHEMA IF EXISTS " + schema + " CASCADE", "CREATE SCHEMA " + schema,
		"SET search_path = " + schema, "SET TimeZone = 'UTC'",
		// 文本列用非字节序的排序规则(模拟生产环境)。测试用的 Alpine 镜像里 en_US.utf8 实际按字节排,
		// 不显式指定的话,"忘了 COLLATE C" 这类错误在测试环境里看不出来
		`CREATE TABLE c (k int8 PRIMARY KEY, g int4 NOT NULL, t text COLLATE "en-US-x-icu" NOT NULL, i int4, d numeric,
		   s text COLLATE "en-US-x-icu", b bool, dt date, ts timestamptz, j jsonb)`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	defer conn.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")

	env := &certEnv{t: t, ctx: ctx, conn: conn}
	rnd := rand.New(rand.NewSource(1))
	for k := 1; k <= certRows; k++ {
		r := Row{"k": int64(k)}
		for _, c := range certCols[1:] {
			dom := certDomain[c.Name]
			r[c.Name] = dom[rnd.Intn(len(dom))]
		}
		env.insert(r)
	}

	scan := Scan("c", certCols...)
	all := func(steps ...Step) []Step { return append([]Step{scan}, steps...) }
	opt := func(name string, k Kind) ParamDecl { return ParamDecl{Name: name, Type: Type{Kind: k}} }
	list := func(name string, k Kind) ParamDecl { return ParamDecl{Name: name, Type: Type{Kind: k}, List: true} }
	byK := SortKey{Col: "k"}

	// 1. cmp: 列 op 参数(参数取值域里的每个值 + 不传),filter 和 project 两种用法
	for _, col := range []string{"i", "d", "s", "b", "dt", "ts"} {
		kind, _ := Schema(certCols).Col(col)
		for op := range cmpOps {
			p := &Pipeline{Params: []ParamDecl{opt("p", kind.Kind)}}
			for _, v := range append(certDomain[col], "<absent>") {
				params := Params{}
				if v != "<absent>" {
					params["p"] = v
				}
				p.Steps = all(Filter(Cmp(op, Col(col), Param("p"))), Sort(byK))
				env.check(fmt.Sprintf("filter %s %s %s", col, op, Canonical(v)), p, params)
				p.Steps = all(Project(NamedExpr{"k", Col("k")}, NamedExpr{"v", Cmp(op, Col(col), Param("p"))}), Sort(byK))
				env.check(fmt.Sprintf("project %s %s %s", col, op, Canonical(v)), p, params)
			}
		}
	}
	// 列 op 列
	for op := range cmpOps {
		env.check("cmp i "+op+" g", &Pipeline{Steps: all(Project(NamedExpr{"k", Col("k")}, NamedExpr{"v", Cmp(op, Col("i"), Col("g"))}), Sort(byK))}, nil)
	}
	// 2. and / or / not(两边都可能是 UNKNOWN)
	x := Cmp(">", Col("i"), Const(int64(1), Type{Kind: Int32}))
	y := Cmp("=", Col("b"), Const(true, Type{Kind: Bool}))
	z := IsNull(Col("s"))
	for name, e := range map[string]*Expr{"and": And(x, y), "or": Or(x, y), "not": Not(x), "and3": And(x, y, Not(z)), "or3": Or(Not(x), y, z)} {
		env.check("logic "+name, &Pipeline{Steps: all(Project(NamedExpr{"k", Col("k")}, NamedExpr{"v", e}), Sort(byK))}, nil)
		env.check("logic filter "+name, &Pipeline{Steps: all(Filter(e), Sort(byK))}, nil)
	}
	// 3. is_null
	for _, c := range certCols[2:] {
		env.check("is_null "+c.Name, &Pipeline{Steps: all(Filter(IsNull(Col(c.Name))), Sort(byK))}, nil)
	}
	// 4. absent(标量 / 列表 / 必填)
	pAbs := &Pipeline{Params: []ParamDecl{opt("p", Int32), list("l", Text), {Name: "r", Type: Type{Kind: Int32}, Required: true}},
		Steps: all(Project(NamedExpr{"k", Col("k")}, NamedExpr{"p", Absent("p")}, NamedExpr{"l", Absent("l")}, NamedExpr{"r", Absent("r")}), Sort(byK))}
	for i, ps := range []Params{{"r": int64(1)}, {"p": int64(1), "r": int64(1)}, {"l": []any{}, "r": int64(1)}, {"l": []any{"a"}, "r": int64(1)}} {
		env.check(fmt.Sprintf("absent #%d", i), pAbs, ps)
	}
	// 5. in_list
	for _, col := range []string{"i", "d", "s", "dt"} {
		kind, _ := Schema(certCols).Col(col)
		dom := certDomain[col][1:]
		for _, l := range [][]any{{}, {dom[0]}, {dom[0], dom[len(dom)-1]}, dom} {
			p := &Pipeline{Params: []ParamDecl{list("l", kind.Kind)}}
			ps := Params{"l": l}
			p.Steps = all(Filter(InList(Col(col), "l")), Sort(byK))
			env.check(fmt.Sprintf("in_list %s %d", col, len(l)), p, ps)
			p.Steps = all(Filter(Or(Absent("l"), InList(Col(col), "l"))), Sort(byK))
			env.check(fmt.Sprintf("属于(空=不筛选) %s %d", col, len(l)), p, ps)
		}
		env.check("属于 不传 "+col, &Pipeline{Params: []ParamDecl{list("l", kind.Kind)}, Steps: all(Filter(Or(Absent("l"), InList(Col(col), "l"))), Sort(byK))}, Params{})
	}
	// 5b. in_tuples(组的列表): 两值(列为 NULL → FALSE,NOT 之后是 TRUE)、空列表、组的组合不能交叉匹配
	for _, pair := range [][2]string{{"g", "s"}, {"i", "dt"}, {"t", "d"}} {
		ka, _ := Schema(certCols).Col(pair[0])
		kb, _ := Schema(certCols).Col(pair[1])
		da, db := certDomain[pair[0]], certDomain[pair[1]]
		if da[0] == nil {
			da = da[1:]
		}
		if db[0] == nil {
			db = db[1:]
		}
		var all2 [][2]any
		for _, a := range da {
			for _, b := range db {
				all2 = append(all2, [2]any{a, b})
			}
		}
		for _, tuples := range [][][2]any{{}, all2[:1], {all2[0], all2[len(all2)-1]}, all2[:len(all2)/2], all2} {
			la, lb := []any{}, []any{}
			for _, x := range tuples {
				la, lb = append(la, x[0]), append(lb, x[1])
			}
			p := &Pipeline{Params: []ParamDecl{list("la", ka.Kind), list("lb", kb.Kind)}}
			ps := Params{"la": la, "lb": lb}
			e := InTuples([]string{pair[0], pair[1]}, []string{"la", "lb"})
			name := fmt.Sprintf("in_tuples %s,%s %d", pair[0], pair[1], len(tuples))
			p.Steps = all(Project(NamedExpr{"k", Col("k")}, NamedExpr{"v", e}, NamedExpr{"nv", Not(e)}), Sort(byK))
			env.check(name, p, ps)
			p.Steps = all(Filter(Or(Absent("la"), e)), Sort(byK))
			env.check(name+" 组属于", p, ps)
		}
	}
	// 6. starts_with
	for _, pre := range []any{"a", "a%", "a_", "", "B", "é", "<absent>"} {
		ps := Params{}
		if pre != "<absent>" {
			ps["p"] = pre
		}
		p := &Pipeline{Params: []ParamDecl{opt("p", Text)}, Steps: all(Project(NamedExpr{"k", Col("k")}, NamedExpr{"v", StartsWith(Col("s"), "p")}), Sort(byK))}
		env.check(fmt.Sprintf("starts_with %v", pre), p, ps)
	}
	// 7. json_field
	for _, f := range []struct {
		key  string
		kind Kind
	}{{"n", Int32}, {"n", Int64}, {"n", Text}, {"s", Text}, {"b", Bool}, {"d", Decimal}, {"dt", Date}, {"missing", Int32}} {
		p := &Pipeline{Steps: all(Project(NamedExpr{"k", Col("k")}, NamedExpr{"v", JSONField(Col("j"), f.key, Type{Kind: f.kind})}), Sort(byK))}
		env.check(fmt.Sprintf("json_field %s %s", f.key, f.kind), p, nil)
	}
	// 8. sort(每列 ASC / DESC,k 决胜)+ limit
	for _, c := range []string{"g", "t", "i", "d", "s", "b", "dt", "ts"} {
		for _, desc := range []bool{false, true} {
			p := &Pipeline{Steps: all(Sort(SortKey{c, desc}, byK))}
			env.check(fmt.Sprintf("sort %s desc=%v", c, desc), p, nil)
			p = &Pipeline{Steps: all(Sort(SortKey{c, desc}, byK), Limit(7))}
			env.check(fmt.Sprintf("sort+limit %s desc=%v", c, desc), p, nil)
		}
	}
	// 9. pick_per_key(键 g;顺序 每列 ASC/DESC + k)
	for _, c := range []string{"i", "d", "s", "dt"} {
		for _, desc := range []bool{false, true} {
			p := &Pipeline{Steps: all(PickPerKey([]string{"g"}, SortKey{c, desc}, SortKey{"k", true}), Sort(SortKey{Col: "g"}))}
			env.check(fmt.Sprintf("pick_per_key g by %s desc=%v", c, desc), p, nil)
		}
	}
	// 9b. pick_per_key 的键可以为 NULL: NULL 自己是一组(s 是可空的文本,i 是可空的整数;和 DISTINCT ON 一致)
	for _, keys := range [][]string{{"s"}, {"i"}, {"g", "s"}, {"s", "i"}} {
		p := &Pipeline{Steps: all(PickPerKey(keys, SortKey{"k", true}), Sort(SortKey{Col: "k"}))}
		env.check(fmt.Sprintf("pick_per_key 可空的键 %v", keys), p, nil)
	}
	// 可空的键 + 只引用键的筛选(下推到取一条之前): NULL 的组对筛选是 UNKNOWN → 整组去掉
	pn := &Pipeline{Params: []ParamDecl{opt("ps", Text)}, Steps: all(PickPerKey([]string{"s"}, SortKey{"k", true}),
		Filter(Or(Absent("ps"), Cmp("=", Col("s"), Param("ps")))), Sort(SortKey{Col: "k"}))}
	for _, v := range []any{"<absent>", "a", "x"} {
		ps := Params{}
		if v != "<absent>" {
			ps["ps"] = v
		}
		env.check(fmt.Sprintf("可空的键 + 下推 ps=%v", v), pn, ps)
	}
	// 10. tuple_after(游标 = 每一行)
	for _, desc := range []bool{false, true} {
		p := &Pipeline{Params: []ParamDecl{opt("cg", Int32), opt("ck", Int64)},
			Steps: all(Filter(TupleAfter([]string{"g", "k"}, []string{"cg", "ck"}, desc)), Sort(SortKey{"g", desc}, SortKey{"k", desc}), Limit(5))}
		env.check(fmt.Sprintf("tuple_after 第一页 desc=%v", desc), p, Params{})
		for _, r := range env.rows[:12] {
			env.check(fmt.Sprintf("tuple_after (%v,%v) desc=%v", r["g"], r["k"], desc), p, Params{"cg": r["g"], "ck": r["k"]})
		}
	}
	// 文本游标(t, k)
	for _, desc := range []bool{false, true} {
		p := &Pipeline{Params: []ParamDecl{opt("ct", Text), opt("ck", Int64)},
			Steps: all(Filter(TupleAfter([]string{"t", "k"}, []string{"ct", "ck"}, desc)), Sort(SortKey{"t", desc}, SortKey{"k", desc}), Limit(5))}
		for _, r := range env.rows[:12] {
			env.check(fmt.Sprintf("tuple_after 文本 (%v,%v) desc=%v", r["t"], r["k"], desc), p, Params{"ct": r["t"], "ck": r["k"]})
		}
	}
	// 文本列的比较(t 不可为 NULL,与参数比较)
	for op := range cmpOps {
		for _, v := range certDomain["t"] {
			p := &Pipeline{Params: []ParamDecl{opt("p", Text)}, Steps: all(Filter(Cmp(op, Col("t"), Param("p"))), Sort(byK))}
			env.check(fmt.Sprintf("filter t %s %v", op, v), p, Params{"p": v})
		}
	}
	// limit 来自参数
	pl := &Pipeline{Params: []ParamDecl{{Name: "n", Type: Type{Kind: Int32}, Required: true}}, Steps: all(Sort(SortKey{"t", false}, byK), LimitParam("n"))}
	for _, n := range []int64{0, 1, 7, 1000} {
		env.check(fmt.Sprintf("limit 参数 %d", n), pl, Params{"n": n})
	}
	// 必填参数: or(absent(p), cmp) 化简为 cmp
	pr := &Pipeline{Params: []ParamDecl{{Name: "p", Type: Type{Kind: Int32}, Required: true}}, Steps: all(Filter(Or(Absent("p"), Cmp("=", Col("i"), Param("p")))), Sort(byK))}
	for _, v := range certDomain["i"][1:] {
		env.check(fmt.Sprintf("必填 等于 %v", v), pr, Params{"p": v})
	}
	// 11. 组合: json 投影 → 每键取最新 → 筛选 → 排序 → 截取(JSON版本 这类单元的形状)
	comp := &Pipeline{Params: []ParamDecl{opt("p", Text)}, Steps: all(
		Project(NamedExpr{"g", Col("g")}, NamedExpr{"k", Col("k")}, NamedExpr{"n", JSONField(Col("j"), "n", Type{Kind: Int32})}, NamedExpr{"s", JSONField(Col("j"), "s", Type{Kind: Text})}),
		PickPerKey([]string{"g"}, SortKey{"k", true}),
		Filter(Or(Absent("p"), Cmp("=", Col("s"), Param("p")))),
		Sort(SortKey{Col: "s"}, SortKey{Col: "g"}), Limit(2))}
	for _, v := range []any{"<absent>", "x", "a", ""} {
		ps := Params{}
		if v != "<absent>" {
			ps["p"] = v
		}
		env.check(fmt.Sprintf("组合 p=%v", v), comp, ps)
	}
	// 11b. 筛选下推: 只引用分组键的条件移到 pick_per_key 之前(SQL 侧);参考求值器跑原样,两边必须一致。
	// 文本键 t(大小写不同的值: 分组用 COLLATE "C" 也不能把 a 和 A 合并)、跨改名的 project、键/非键混合的 and。
	push := &Pipeline{Params: []ParamDecl{{Name: "ts", Type: Type{Kind: Text}, List: true}, opt("pi", Int32), opt("pp", Text)}, Steps: all(
		PickPerKey([]string{"t"}, SortKey{"k", true}),
		Project(NamedExpr{"tt", Col("t")}, NamedExpr{"i", Col("i")}, NamedExpr{"k", Col("k")}),
		Filter(And(Or(Absent("ts"), InList(Col("tt"), "ts")), Or(Absent("pi"), Cmp("=", Col("i"), Param("pi"))))),
		Filter(Or(Absent("pp"), StartsWith(Col("tt"), "pp"))),
		Sort(SortKey{Col: "tt"}), Limit(3))}
	if q := pushdown(push); q.Steps[1].Kind != "filter" || q.Steps[2].Kind != "pick_per_key" || len(q.Steps) != len(push.Steps) {
		t.Errorf("筛选下推的结果不对: %+v", q.Steps)
	}
	for _, ps := range []Params{{}, {"ts": []any{"a", "A"}}, {"ts": []any{"b"}, "pi": certDomain["i"][1]}, {"pp": "a"}, {"pp": "A", "pi": certDomain["i"][2]}, {"ts": []any{}}} {
		env.check(fmt.Sprintf("下推 %v", ps), push, ps)
	}
	// 11c. 每组取一条 + 游标(游标列都是分组键 → 下推到取一条之前): 分组键 (g, t),游标 (g, t) 升序 / 降序
	for _, desc := range []bool{false, true} {
		cur := &Pipeline{Params: []ParamDecl{opt("cg", Int32), opt("ct", Text)}, Steps: all(
			PickPerKey([]string{"g", "t"}, SortKey{"k", true}),
			Project(NamedExpr{"g", Col("g")}, NamedExpr{"tt", Col("t")}, NamedExpr{"k", Col("k")}),
			Filter(TupleAfter([]string{"g", "tt"}, []string{"cg", "ct"}, desc)),
			Sort(SortKey{Col: "g", Desc: desc}, SortKey{Col: "tt", Desc: desc}), Limit(4))}
		if q := pushdown(cur); q.Steps[1].Kind != "filter" || q.Steps[1].Pred.Op != "tuple_after" {
			t.Errorf("游标没有下推: %+v", q.Steps)
		}
		for _, ps := range []Params{{}, {"cg": int64(1), "ct": "a"}, {"cg": int64(2), "ct": "B"}, {"cg": int64(1), "ct": "ab"}, {"cg": int64(3), "ct": "A"}} {
			env.check(fmt.Sprintf("每组取一条 + 游标 desc=%v %v", desc, ps), cur, ps)
		}
	}
	// 11d. 两层的每组取一条(来源的取最新 (g, t) → API 的每组取一条 (g))+ 游标 g: 反复下推,越过两层
	two := &Pipeline{Params: []ParamDecl{opt("cg", Int32)}, Steps: all(
		PickPerKey([]string{"g", "t"}, SortKey{"k", true}),
		Filter(Not(IsNull(Col("i")))),
		PickPerKey([]string{"g"}, SortKey{"i", true}, SortKey{"k", false}),
		Project(NamedExpr{"g", Col("g")}, NamedExpr{"i", Col("i")}, NamedExpr{"k", Col("k")}),
		Filter(TupleAfter([]string{"g"}, []string{"cg"}, false)),
		Sort(SortKey{Col: "g"}), Limit(2))}
	q := two
	for q2 := pushdown(q); q2 != q; q2 = pushdown(q) {
		q = q2
	}
	if q.Steps[1].Kind != "filter" || q.Steps[1].Pred.Op != "tuple_after" {
		t.Errorf("游标没有越过两层下推: %+v", q.Steps)
	}
	for _, ps := range []Params{{}, {"cg": int64(1)}, {"cg": int64(2)}, {"cg": int64(3)}} {
		env.check(fmt.Sprintf("两层每组取一条 + 游标 %v", ps), two, ps)
	}
	// 13. group(分组聚合)/ rank(名次)。聚合的规则在 agg.go(child_agg 共用)
	certGroup(env, all)
	// 12. 格式错 → 两边都报错
	env.insert(Row{"k": int64(1000), "g": int64(1), "t": "a", "j": `{"n": "abc", "d": "x", "b": "maybe", "dt": "2026/13/01"}`})
	for _, f := range []struct {
		key  string
		kind Kind
	}{{"n", Int32}, {"d", Decimal}, {"b", Bool}, {"dt", Date}} {
		p := &Pipeline{Steps: all(Filter(Cmp("=", Col("k"), Const(int64(1000), Type{Kind: Int64}))), Project(NamedExpr{"v", JSONField(Col("j"), f.key, Type{Kind: f.kind})}))}
		env.checkError(fmt.Sprintf("json_field 格式错 %s %s", f.key, f.kind), p)
	}
	// 12b. Postgres 的 cast 宽松接受、但不是规范写法的值 → 两边都报错(以前 SQL 能读、参考求值器报错)
	k := int64(2000)
	for _, c := range []struct {
		kind Kind
		val  string
	}{
		{Date, `"2026/01/10"`}, {Date, `"20260110"`}, {Date, `"Jan 10 2026"`}, {Date, `"2026-01-10 00:00:00"`},
		{Decimal, `"1e2"`}, {Decimal, `"NaN"`}, {Decimal, `"Infinity"`}, {Decimal, `"1_000"`},
		{Int32, `"1_000"`}, {Int32, `"0x10"`}, {Int32, `"　5"`}, // 全角空格
		{Bool, `"tr"`}, {Bool, `"ye"`},
		{Timestamp, `"2026-01-10 10:00:00+09"`}, {Timestamp, `"2026-01-10T10:00:00"`}, {Timestamp, `"2026-01-10t10:00:00z"`}, {Timestamp, `"2026-01-10T10:00:00.1234567Z"`},
	} {
		k++
		env.insert(Row{"k": k, "g": int64(1), "t": "a", "j": `{"v": ` + c.val + `}`})
		p := &Pipeline{Steps: all(Filter(Cmp("=", Col("k"), Const(k, Type{Kind: Int64}))), Project(NamedExpr{"v", JSONField(Col("j"), "v", Type{Kind: c.kind})}))}
		env.checkError(fmt.Sprintf("json_field 不是规范写法 %s %s", c.kind, c.val), p)
	}
	// 规范写法(含 ASCII 空白、大小写、符号)→ 两边一致
	for _, c := range []struct {
		kind Kind
		val  string
	}{
		{Date, `" 2026-01-10 "`}, {Decimal, `" -1.50 "`}, {Decimal, `".5"`}, {Decimal, `"+2."`}, {Int32, `" +7 "`}, {Int32, `-3`},
		{Bool, `"TRUE"`}, {Bool, `" Off "`}, {Bool, `true`}, {Timestamp, `"2026-01-10T10:00:00.5+09:00"`}, {Timestamp, `"2026-01-10T01:00:00Z"`},
	} {
		k++
		env.insert(Row{"k": k, "g": int64(1), "t": "a", "j": `{"v": ` + c.val + `}`})
		p := &Pipeline{Steps: all(Filter(Cmp("=", Col("k"), Const(k, Type{Kind: Int64}))), Project(NamedExpr{"v", JSONField(Col("j"), "v", Type{Kind: c.kind})}))}
		env.check(fmt.Sprintf("json_field 规范写法 %s %s", c.kind, c.val), p, nil)
	}
	certLookup(env, all)
	certNest(env, all) // cert_nest_test.go
	if !t.Failed() {
		t.Logf("元件认证: %d 个用例全部一致", env.n)
	}
}

func (e *certEnv) tables() map[string][]Row {
	table := e.table // 时间运算的认证用自己的表名
	if table == "" {
		table = "c"
	}
	m := map[string][]Row{table: e.rows}
	for k, v := range e.others {
		m[k] = v
	}
	for k, v := range e.extra {
		m[k] = v
	}
	return m
}

// insertOther: lookup 的对方的表的一行(值按列的类型 cast)。
func (e *certEnv) insertOther(table string, cols []NamedType, vals ...any) {
	r := Row{}
	var holders []string
	var args []any
	for i, c := range cols {
		r[c.Name] = vals[i]
		holders = append(holders, fmt.Sprintf("$%d::%s", i+1, c.Type.Kind.SQLType()))
		args = append(args, sqlArg(vals[i]))
	}
	if _, err := e.conn.Exec(e.ctx, fmt.Sprintf("INSERT INTO %s VALUES (%s)", table, strings.Join(holders, ", ")), args...); err != nil {
		e.t.Fatal(err)
	}
	if e.others == nil {
		e.others = map[string][]Row{}
	}
	e.others[table] = append(e.others[table], r)
}

// certLookup: 多对一查找。键匹配 / 不匹配 / NULL(两边),单列和两列的键,keep 和 drop,两级链式;
// 陷阱: 大小写不同的文本键(ICU 的排序规则下也不相等)、1.5 = 1.50(十进制按值相等)、int4 = int8、对方的唯一列里有 NULL。
func certLookup(env *certEnv, all func(steps ...Step) []Step) {
	t := env.t
	nt := func(name string, k Kind, nullable bool) NamedType {
		return NamedType{name, Type{Kind: k, Nullable: nullable}}
	}
	for _, q := range []string{
		`CREATE TABLE o1 (k int8 PRIMARY KEY, name text COLLATE "en-US-x-icu" NOT NULL, ref text COLLATE "en-US-x-icu", amt numeric)`,
		`CREATE TABLE ot (id int8 PRIMARY KEY, u text COLLATE "en-US-x-icu" UNIQUE, name text COLLATE "en-US-x-icu" NOT NULL)`,
		`CREATE TABLE o2 (g int4 NOT NULL, t text COLLATE "en-US-x-icu" NOT NULL, label text COLLATE "en-US-x-icu", PRIMARY KEY (g, t))`,
		`CREATE TABLE od (k numeric PRIMARY KEY, name text NOT NULL)`,
		`CREATE TABLE odt (k date PRIMARY KEY, name text NOT NULL)`,
	} {
		if _, err := env.conn.Exec(env.ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	o1 := []NamedType{nt("k", Int64, false), nt("name", Text, false), nt("ref", Text, true), nt("amt", Decimal, true)}
	for _, r := range [][]any{{int64(1), "one", "a", rat("1.5")}, {int64(10), "ten", nil, nil}, {int64(5), "five", "B", rat("-2")}, {int64(3), "three", "zz", rat("9")}} {
		env.insertOther("o1", o1, r...)
	}
	ot := []NamedType{nt("id", Int64, false), nt("u", Text, true), nt("name", Text, false)}
	for _, r := range [][]any{{int64(1), "a", "ua"}, {int64(2), "B", "uB"}, {int64(3), nil, "unull"}, {int64(4), "", "uempty"}, {int64(5), "a%", "upct"}, {int64(6), nil, "unull2"}} {
		env.insertOther("ot", ot, r...)
	}
	o2 := []NamedType{nt("g", Int32, false), nt("t", Text, false), nt("label", Text, true)}
	for _, r := range [][]any{{int64(1), "a", "1a"}, {int64(1), "B", "1B"}, {int64(2), "a", nil}, {int64(2), "ab", "2ab"}, {int64(3), "A", "3A"}, {int64(9), "a", "9a"}} {
		env.insertOther("o2", o2, r...)
	}
	od := []NamedType{nt("k", Decimal, false), nt("name", Text, false)}
	for _, r := range [][]any{{rat("1.50"), "d1.5"}, {rat("10"), "d10"}, {rat("-2.0"), "d-2"}, {rat("3"), "d3"}} {
		env.insertOther("od", od, r...)
	}
	odt := []NamedType{nt("k", Date, false), nt("name", Text, false)}
	for _, r := range [][]any{{day("2026-01-01"), "jan1"}, {day("2025-12-31"), "dec31"}, {day("2026-03-01"), "mar1"}} {
		env.insertOther("odt", odt, r...)
	}
	pk := func(cols ...string) [][]string { return [][]string{cols} }
	byK := SortKey{Col: "k"}
	type cfg struct {
		name string
		step func(missing string) Step
	}
	cfgs := []cfg{
		{"int4→int8 主键", func(m string) Step {
			return Lookup("o1", o1, pk("k"), []JoinKey{{"i", "k"}}, []TakeCol{{"o_name", "name"}, {"o_amt", "amt"}}, m)
		}},
		{"文本 唯一列(可空)", func(m string) Step {
			return Lookup("ot", ot, [][]string{{"id"}, {"u"}}, []JoinKey{{"s", "u"}}, []TakeCol{{"u_name", "name"}, {"u_id", "id"}}, m)
		}},
		{"两列的键", func(m string) Step {
			return Lookup("o2", o2, pk("g", "t"), []JoinKey{{"g", "g"}, {"t", "t"}}, []TakeCol{{"label", "label"}}, m)
		}},
		{"两列的键(顺序反过来)", func(m string) Step {
			return Lookup("o2", o2, pk("g", "t"), []JoinKey{{"t", "t"}, {"g", "g"}}, []TakeCol{{"label", "label"}, {"o2_t", "t"}}, m)
		}},
		{"十进制", func(m string) Step {
			return Lookup("od", od, pk("k"), []JoinKey{{"d", "k"}}, []TakeCol{{"d_name", "name"}, {"d_k", "k"}}, m)
		}},
		{"日期", func(m string) Step {
			return Lookup("odt", odt, pk("k"), []JoinKey{{"dt", "k"}}, []TakeCol{{"dt_name", "name"}}, m)
		}},
	}
	for _, c := range cfgs {
		for _, m := range []string{"keep", "drop", ""} {
			env.check(fmt.Sprintf("lookup %s missing=%q", c.name, m), &Pipeline{Steps: all(c.step(m), Sort(byK))}, nil)
		}
	}
	// 两级链式: c.i → o1.k(取 ref)→ ot.u(取 name)。keep/drop 的四种组合
	for _, m1 := range []string{"keep", "drop"} {
		for _, m2 := range []string{"keep", "drop"} {
			p := &Pipeline{Steps: all(
				Lookup("o1", o1, pk("k"), []JoinKey{{"i", "k"}}, []TakeCol{{"o_ref", "ref"}, {"o_name", "name"}}, m1),
				Lookup("ot", ot, [][]string{{"u"}}, []JoinKey{{"o_ref", "u"}}, []TakeCol{{"u_name", "name"}}, m2),
				Project(NamedExpr{"k", Col("k")}, NamedExpr{"i", Col("i")}, NamedExpr{"o_ref", Col("o_ref")}, NamedExpr{"o_name", Col("o_name")}, NamedExpr{"u_name", Col("u_name")}),
				Sort(byK))}
			env.check(fmt.Sprintf("lookup 两级链式 %s/%s", m1, m2), p, nil)
		}
	}
	// 关联的列来自计算(project 之后)/ 每组取一条之后(要包一层子查询)/ 之后再每组取一条
	env.check("lookup 在 project 之后", &Pipeline{Steps: all(
		Filter(Cmp("<", Col("k"), Const(int64(1000), Type{Kind: Int64}))), // 12. 之后插入了格式错的 JSON
		Project(NamedExpr{"k", Col("k")}, NamedExpr{"key", JSONField(Col("j"), "n", Type{Kind: Int32})}),
		Lookup("o1", o1, pk("k"), []JoinKey{{"key", "k"}}, []TakeCol{{"o_name", "name"}}, "keep"), Sort(byK))}, nil)
	env.check("lookup 在每组取一条之后", &Pipeline{Steps: all(
		PickPerKey([]string{"g"}, SortKey{"k", true}),
		Lookup("o2", o2, pk("g", "t"), []JoinKey{{"g", "g"}, {"t", "t"}}, []TakeCol{{"label", "label"}}, "keep"), Sort(SortKey{Col: "g"}))}, nil)
	env.check("lookup 之后每组取一条(按加的列分组)", &Pipeline{Steps: all(
		Lookup("o1", o1, pk("k"), []JoinKey{{"i", "k"}}, []TakeCol{{"o_name", "name"}}, "keep"),
		PickPerKey([]string{"o_name"}, SortKey{"k", true}), Sort(SortKey{Col: "k"}))}, nil)
	// 加的列用在筛选 / 排序 / 截取里(文本: 按字节;NULL 在 ASC 最后)
	for _, m := range []string{"keep", "drop"} {
		lk := Lookup("ot", ot, [][]string{{"u"}}, []JoinKey{{"s", "u"}}, []TakeCol{{"u_name", "name"}}, m)
		for _, v := range []any{"<absent>", "ua", "uB", "nope"} {
			ps := Params{}
			if v != "<absent>" {
				ps["p"] = v
			}
			p := &Pipeline{Params: []ParamDecl{{Name: "p", Type: Type{Kind: Text}}}, Steps: all(lk,
				Filter(Or(Absent("p"), Cmp("=", Col("u_name"), Param("p")))), Sort(byK))}
			env.check(fmt.Sprintf("lookup %s + 筛选加的列 p=%v", m, v), p, ps)
		}
		for _, desc := range []bool{false, true} {
			env.check(fmt.Sprintf("lookup %s + 按加的列排序 desc=%v", m, desc), &Pipeline{Steps: all(lk,
				Sort(SortKey{"u_name", desc}, byK), Limit(9))}, nil)
		}
		env.check(fmt.Sprintf("lookup %s + is_null(加的列)", m), &Pipeline{Steps: all(lk, Filter(IsNull(Col("u_name"))), Sort(byK))}, nil)
	}
	// 筛选在 lookup 之前(同一层的 WHERE)和之后
	env.check("筛选 → lookup → 筛选", &Pipeline{Steps: all(
		Filter(Cmp(">", Col("g"), Const(int64(1), Type{Kind: Int32}))),
		Lookup("o2", o2, pk("g", "t"), []JoinKey{{"g", "g"}, {"t", "t"}}, []TakeCol{{"label", "label"}}, "keep"),
		Filter(Not(IsNull(Col("label")))), Sort(byK))}, nil)

	// DRC: 不是唯一键 / 类型不能比较 / 重名 → 编译失败
	for name, st := range map[string]Step{
		"键不唯一":   Lookup("o2", o2, pk("g", "t"), []JoinKey{{"g", "g"}}, []TakeCol{{"label", "label"}}, "keep"),
		"类型不能比较": Lookup("o1", o1, pk("k"), []JoinKey{{"s", "k"}}, []TakeCol{{"o_name", "name"}}, "keep"),
		"重名":     Lookup("o1", o1, pk("k"), []JoinKey{{"i", "k"}}, []TakeCol{{"s", "name"}}, "keep"),
		"列不存在":   Lookup("o1", o1, pk("k"), []JoinKey{{"nope", "k"}}, []TakeCol{{"o_name", "name"}}, "keep"),
		"找不到时":   Lookup("o1", o1, pk("k"), []JoinKey{{"i", "k"}}, []TakeCol{{"o_name", "name"}}, "skip"),
	} {
		if _, err := Check(&Pipeline{Steps: all(st)}); err == nil {
			t.Errorf("lookup DRC %s: 应该报错", name)
		}
	}
	// 类型: keep → 加的列可空;drop → 保持对方的列的可空性
	for m, want := range map[string]bool{"keep": true, "drop": false} {
		s, err := Check(&Pipeline{Steps: all(Lookup("o1", o1, pk("k"), []JoinKey{{"i", "k"}}, []TakeCol{{"o_name", "name"}}, m))})
		if err != nil {
			t.Fatal(err)
		}
		if ty, _ := s.Col("o_name"); ty.Nullable != want {
			t.Errorf("lookup %s: o_name 的可空性 %v,应该是 %v", m, ty.Nullable, want)
		}
	}
}

func (e *certEnv) insert(r Row) {
	args := make([]any, 0, len(certCols))
	for _, c := range certCols {
		v := r[c.Name]
		switch x := v.(type) {
		case *big.Rat:
			v = RatString(x)
		case time.Time:
			v = x.Format(time.RFC3339)
		}
		args = append(args, v)
	}
	_, err := e.conn.Exec(e.ctx, `INSERT INTO c VALUES ($1::int8, $2::int4, $3::text, $4::int4, $5::numeric, $6::text, $7::bool, $8::date, $9::timestamptz, $10::jsonb)`, args...)
	if err != nil {
		e.t.Fatal(err)
	}
	if j, ok := r["j"].(string); ok {
		dec := json.NewDecoder(strings.NewReader(j))
		dec.UseNumber()
		var doc any
		_ = dec.Decode(&doc)
		r["j"] = doc
	}
	e.rows = append(e.rows, r)
}

// run: SQL 后端在 Postgres 上的结果(规范文本)
func (e *certEnv) run(p *Pipeline, params Params) ([]string, *SQL, error) {
	q, err := ToSQL(p, Options{TextOutput: true})
	if err != nil {
		e.t.Fatalf("ToSQL: %v", err)
	}
	var args []any
	for _, d := range q.Params {
		v, ok := params[d.Name]
		if !ok {
			args = append(args, nil)
			continue
		}
		args = append(args, sqlArg(v))
	}
	rows, err := e.conn.Query(e.ctx, q.Text, args...)
	if err != nil {
		return nil, q, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		vals := make([]*string, len(q.Columns))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, q, err
		}
		var parts []string
		for i, c := range q.Columns {
			parts = append(parts, c.Name+"="+canonicalText(vals[i], c.Type))
		}
		out = append(out, strings.Join(parts, " "))
	}
	return out, q, rows.Err()
}

func sqlArg(v any) any {
	switch x := v.(type) {
	case *big.Rat:
		return RatString(x)
	case time.Time:
		return x.Format(time.RFC3339Nano)
	case []any:
		out := make([]any, len(x))
		for i, y := range x {
			out[i] = sqlArg(y)
		}
		return out
	}
	return v
}

// canonicalText: Postgres 的 ::text 输出 → 与 Canonical 相同的写法
func canonicalText(s *string, t Type) string {
	v, err := FromPGTyped(s, t)
	if err != nil {
		return "!" + *s
	}
	return Canonical(v)
}

func (e *certEnv) check(name string, p *Pipeline, params Params) {
	e.t.Helper()
	e.n++
	want, werr := Eval(p, e.tables(), params)
	got, q, gerr := e.run(p, params)
	if errors.Is(werr, ErrAmbiguous) {
		e.t.Fatalf("%s: 认证数据有并列(认证本身的问题): %v", name, werr)
	}
	if werr != nil || gerr != nil {
		e.t.Errorf("%s: 参考求值器 err=%v / SQL err=%v\n%s", name, werr, gerr, q.Text)
		return
	}
	var ws []string
	for _, r := range want.Rows {
		var parts []string
		for _, c := range want.Columns {
			parts = append(parts, c.Name+"="+Canonical(r[c.Name]))
		}
		ws = append(ws, strings.Join(parts, " "))
	}
	if !want.Ordered {
		sort.Strings(ws)
		sort.Strings(got)
	}
	if strings.Join(ws, "\n") != strings.Join(got, "\n") {
		e.t.Errorf("%s: 不一致\nSQL:\n%s\n参考求值器 (%d 行):\n  %s\nPostgres (%d 行):\n  %s", name, q.Text,
			len(ws), strings.Join(ws, "\n  "), len(got), strings.Join(got, "\n  "))
	}
}

func (e *certEnv) checkError(name string, p *Pipeline) {
	e.t.Helper()
	e.n++
	_, werr := Eval(p, e.tables(), nil)
	_, q, gerr := e.run(p, nil)
	if werr == nil || gerr == nil {
		e.t.Errorf("%s: 两边都应该报错: 参考求值器 err=%v / SQL err=%v\n%s", name, werr, gerr, q.Text)
	}
}
