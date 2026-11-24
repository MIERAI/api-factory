"""元件认证的变异测试: 故意把 SQL 后端 / 参考求值器改错,元件认证(factory/core 的测试,FACTORY_TEST_DB)必须失败。

usage: FACTORY_TEST_DB=postgres://... python3 scripts/core-mutate.py [--workers N] [--list] [名字的正则]

每个变异 = 一个文件里的若干处 字面替换(旧 → 新)。旧的文本在范围里必须正好出现 count 次(默认 1),
否则整个运行失败(不是跳过): 代码改了、变异没改到任何东西 = 变异失效,必须改写或删除(和 api-perf 一样: 不认识 = ✗)。
范围(within): 从含有这个文本的那一行开始,到下一个顶层的 "\\n}\\n" 为止(一个函数);不写 = 整个文件。

并行: 每个变异用 `go test -overlay`(工作区的文件不动),认证的测试每个进程一个自己的 schema(按 pid),共用一个库。
"""
import argparse
import concurrent.futures as cf
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
os.chdir(ROOT)

C = "factory/core/"
S, E, A, P, K, JK = C + "sql.go", C + "eval.go", C + "agg.go", C + "pushdown.go", C + "keyindex.go", C + "expr_json_key.go"
LK, NEST, REL, ST, BK = C + "prim_lookup.go", C + "prim_nest.go", C + "relation.go", C + "state.go", C + "bucket.go"


def m(name, file, *edits, within=None):
    """edits: (旧, 新) 或 (旧, 新, 次数)。"""
    return {"name": name, "file": file, "within": within,
            "edits": [e if len(e) == 3 else (e[0], e[1], 1) for e in edits]}


MUTANTS = [
    # ---- 排序 / 比较 / 游标(sql.go、expr_*.go)
    m("SQL: 排序去掉 COLLATE C", S, ('\t\t\te += ` COLLATE "C"`\n', '\t\t\te += ""\n', 3)),
    m("SQL: 比较去掉 COLLATE C", C + "expr_cmp.go", ('a += ` COLLATE "C"`', 'a += ""')),
    m("SQL: 前缀去掉转义", C + "expr_starts_with.go",
      ("""`(%s LIKE replace(replace(replace(%s, '\\', '\\\\'), '%%', '\\%%'), '_', '\\_') || '%%')`""", """`(%s LIKE %s || '%%')`""")),
    m("SQL: 列表不传时没有 coalesce", C + "expr_absent.go", ("(coalesce(cardinality(%s), 0) = 0)", "(cardinality(%s) = 0)")),
    m("SQL: DESC 的 NULL 放最后", S, ('e += " DESC"', 'e += " DESC NULLS LAST"')),
    m("SQL: 游标 > 改成 >=", C + "expr_tuple_after.go", ('op := ">"', 'op := ">="')),
    m("SQL: 每键取一条丢掉顺序", S, ("order = append(order, g.orderItems(lv, lv.dOrder)...)", "_ = 0")),
    m("SQL: 标量不传 → IS NOT NULL", C + "expr_absent.go", ('return "(" + p + " IS NULL)", nil', 'return "(" + p + " IS NOT NULL)", nil')),
    m("SQL: 游标去掉 COLLATE C", C + "expr_tuple_after.go", ('s += ` COLLATE "C"`', 's += ""')),
    m("参考: and 把 UNKNOWN 当 FALSE", C + "expr_logic.go", ("unknown = true", "unknown = false")),
    m("参考: NULL 排在最前", E, ("case a == nil:\n\t\treturn 1", "case a == nil:\n\t\treturn -1")),
    m("参考: 文本不按字节(忽略大小写)", E, ("return bytes.Compare([]byte(x), []byte(b.(string)))",
                                    "return strings.Compare(strings.ToLower(x), strings.ToLower(b.(string)))")),
    m("参考: 空列表 → 不是 absent", E, ("return len(l) == 0", "return len(l) < 0")),
    # ---- 组属于(expr_in_tuples.go)
    m("SQL: 组属于没有按下标对齐", C + "expr_in_tuples.go", ('conds = append(conds, fmt.Sprintf("tup%d.n = tup1.n", i+1))', "_ = 0")),
    m("SQL: 组属于用 IN(三值)", C + "expr_in_tuples.go",
      ('return fmt.Sprintf("EXISTS (SELECT 1 FROM %s WHERE %s)", strings.Join(from, ", "), strings.Join(conds, " AND ")), nil',
       'return fmt.Sprintf("(CASE WHEN EXISTS (SELECT 1 FROM %s WHERE %s) THEN TRUE ELSE NULL::bool END)", strings.Join(from, ", "), strings.Join(conds, " AND ")), nil')),
    m("参考: 组属于列为 NULL → UNKNOWN", C + "expr_in_tuples.go", ("if v == nil {\n\t\t\t\t\treturn false, nil", "if v == nil {\n\t\t\t\t\treturn nil, nil")),
    # ---- lookup(prim_lookup.go、lookup_asof.go)
    m("SQL: lookup 的 keep 写成 JOIN", LK, ('join := "LEFT JOIN"\n', 'join := "JOIN"\n')),
    m("SQL: lookup 的 drop 写成 LEFT JOIN", LK, ('join = "JOIN"\n', 'join = "LEFT JOIN"\n')),
    m("SQL: lookup NULL 当作相等", LK, ('"%s = %s.%s", g.col(lv, k.Col)', '"%s IS NOT DISTINCT FROM %s.%s", g.col(lv, k.Col)')),
    m("SQL: lookup 多列键只比较第一列", LK, ('alias, strings.Join(conds, " AND "))', "alias, conds[0])")),
    m("SQL: lookup 键按文本忽略大小写比较", LK, ('"%s = %s.%s", g.col(lv, k.Col)', '"lower(%s::text) = lower(%s.%s::text)", g.col(lv, k.Col)')),
    m("SQL: 逻辑表的 keep 写成 JOIN LATERAL", LK, ('join := "LEFT JOIN LATERAL"', 'join := "JOIN LATERAL"')),
    m("SQL: 按日期的 keep 写成 JOIN LATERAL", C + "lookup_asof.go", ('join := "LEFT JOIN LATERAL"', 'join := "JOIN LATERAL"')),
    m("参考: lookup NULL 键也进索引", LK, ("if !st.NullEq {", "if false {")),
    m("参考: lookup drop 不丢弃", LK, ('if st.Missing == "drop" {\n\t\t\t\tcontinue', 'if false {\n\t\t\t\tcontinue')),
    m("参考: lookup 文本键忽略大小写", LK, ("b.WriteString(Canonical(v))", "b.WriteString(strings.ToLower(Canonical(v)))")),
    m("参考: 逻辑表直接用物理表的行", REL, ("if len(st.Source) == 0 {\n\t\treturn tables[st.Table], nil", "if true {\n\t\treturn tables[st.Table], nil")),
    # ---- nest / child_agg(prim_nest.go、relation.go、agg.go、prim_child_agg.go)
    m("SQL: nest 空列表 → NULL(没有 coalesce)", NEST,
      ("coalesce(jsonb_agg(jsonb_build_object(%s) ORDER BY %s), '[]'::jsonb)", "jsonb_agg(jsonb_build_object(%s) ORDER BY %s)")),
    m("SQL: nest 没有多取一行(LIMIT N)", NEST, ('strings.Join(g.orderItems(child, st.Order), ", "), st.N+1)', 'strings.Join(g.orderItems(child, st.Order), ", "), st.N)')),
    # 不列: "nest 的 JSON 聚合没有 ORDER BY" —— Postgres 实际按有序子查询的顺序聚合,测试看不出来(SQL 标准不保证,所以照写 ORDER BY)
    m("SQL: nest 十进制写成 JSON 数字", NEST, ('return x + "::text"', "return x")),
    m("SQL: nest 时间不换成 UTC", NEST, (" AT TIME ZONE 'UTC', 'YYYY", ", 'YYYY")),
    m("SQL: nest 日期按会话的写法", NEST, ("""return "to_char(" + x + ", 'YYYY-MM-DD')\"""", 'return x + "::text"')),
    m("SQL: 子表的关联 NULL 当作相等", REL, ('"%s = %s", g.col(child, k.Key)', '"%s IS NOT DISTINCT FROM %s", g.col(child, k.Key)')),
    m("SQL: 子表的条件被忽略", REL, ("if st.Pred != nil {", "if false && st.Pred != nil {"), within="func (g *sqlGen) childFrom("),
    m("SQL: 子表的顺序方向反了", NEST,
      ('strings.Join(g.orderItems(w, st.Order), ", ")', 'strings.Join(g.orderItems(w, reversedOrder(st.Order)), ", ")'),
      ("func takeCols(", "func reversedOrder(o []SortKey) []SortKey {\n\tr := append([]SortKey(nil), o...)\n\tfor i := range r {\n\t\tr[i].Desc = !r[i].Desc\n\t}\n\treturn r\n}\n\nfunc takeCols(")),
    m("SQL: sum 空集合 → NULL", A, ('return fmt.Sprintf("coalesce(sum(%s), 0)", x)', 'return fmt.Sprintf("sum(%s)", x)')),
    m("SQL: 整数 sum 没有 cast", A, ('return fmt.Sprintf("coalesce(sum(%s), 0)::int8", x)', 'return fmt.Sprintf("coalesce(sum(%s), 0)", x)')),
    m("SQL: count = LEFT JOIN 的行数(空 = 1)", A, ('return "count(*)"', 'return "greatest(count(*), 1)"')),
    m("SQL: min ↔ max", A, ('return fmt.Sprintf("%s(%s)", a.Func, x)', 'return fmt.Sprintf("%s(%s)", map[string]string{"min": "max", "max": "min"}[a.Func], x)')),
    m("SQL: min / max 文本没有 COLLATE C", A, ('x += ` COLLATE "C"`', 'x += ""')),
    m("参考: nest 空列表 → NULL", NEST, ('if len(list) == 0 && st.Mutant == "empty_null" {', "if len(list) == 0 {")),
    m("参考: nest 不截断", NEST, ("if len(group) > st.N+1 {", "if false && len(group) > st.N+1 {")),
    m("参考: 子表的关联 NULL 也匹配", REL, ('return "", false', 'b.WriteString("∅")'), within="func childKey("),
    m("参考: 子表的条件被忽略", REL, ("if st.Pred != nil {", "if false && st.Pred != nil {"), within="func (p *Pipeline) childIndex("),
    m("参考: sum 空集合 → NULL", C + "prim_child_agg.go",
      ('case len(group) == 0 && a.Func == "sum" && st.Mutant == "sum_empty_null":', 'case len(group) == 0 && a.Func == "sum":')),
    m("参考: min / max 不忽略 NULL", A, ("if v == nil {\n\t\t\t\tcontinue", "if v == nil {\n\t\t\t\treturn nil, nil"), within='case "min", "max":'),
    m("参考: sum 多加(十进制)", A, ("s.Add(s, v.(*big.Rat))", "s.Add(s, v.(*big.Rat))\n\t\t\t\t\ts.Add(s, big.NewRat(1, 100))")),
    m("参考: min ↔ max", A, ('(a.Func == "min" && c < 0)', '(a.Func == "min" && c > 0)')),
    m("参考: 整数 sum 溢出不报错", A, ("if !s.IsInt64() {", "if false {")),
    # ---- group / rank
    # GROUP BY 去掉 COLLATE "C" 是等价变异: 确定性的排序规则下相等 = 按字节相等(Postgres 用 strcmp 决胜),分组结果不变。
    # 写 COLLATE "C" 是为了和 sort / DISTINCT ON 的写法一致(设计稿 §2.4),不是为了正确性。
    m("SQL: rank → dense_rank", S, ('"rank() OVER (ORDER BY "', '"dense_rank() OVER (ORDER BY "')),
    m("SQL: rank → row_number", S, ('"rank() OVER (ORDER BY "', '"row_number() OVER (ORDER BY "')),
    m("参考: NULL 的键每个一组", C + "prim_group.go",
      ("k := keyString(r, st.Keys)", 'k := keyString(r, st.Keys)\n\t\tif strings.Contains(k, "∅") {\n\t\t\tk += fmt.Sprint(len(groups))\n\t\t}'),
      within="func evalGroup("),
    m("参考: 名次不跳号(dense_rank)", C + "prim_rank.go", ("if compareRows(o, r, st.Order) < 0 {", "if compareRows(o, r, st.Order) < 0 && false {")),
    # ---- R2.5: 包含 / ymd / 逻辑表
    m("SQL: 包含写成前缀匹配", C + "expr_contains.go", ("(%s LIKE '%%' || replace", "(%s LIKE replace")),
    m("参考: 包含写成前缀匹配", C + "expr_contains.go",
      ("return strings.Contains(a.(string), params[e.Name].(string)), nil", "return strings.HasPrefix(a.(string), params[e.Name].(string)), nil")),
    m("SQL: ymd 不检查合法(只按整数比较)", C + "expr_ymd.go",
      ("""return fmt.Sprintf("((%s %s (to_char(%s, 'YYYYMMDD'))::int4) AND %s)", x, e.Name, v, ymdValidSQL(x)), nil""",
       """return fmt.Sprintf("(%s %s (to_char(%s, 'YYYYMMDD'))::int4)", x, e.Name, v), nil""")),
    m("SQL: ymd 的改写在 not 之下也做", C + "expr_cmp.go", ('if pos && len(e.Args) == 2 && e.Args[0].Op == "ymd" {', 'if len(e.Args) == 2 && e.Args[0].Op == "ymd" {')),
    m("参考: ymd 按日历进位", C + "expr_ymd.go", ("return YMDDate(x.(int64), e.Loose), nil", "return YMDDate(x.(int64), true), nil")),
    # ---- 当前状态表(state.go;docs/design/current-state.md)
    m("状态表: 新的版本不覆盖旧的(DO NOTHING)", ST, ("ON CONFLICT (%s) DO UPDATE SET %s\\nWHERE %s", "ON CONFLICT (%s) DO NOTHING%.0s%.0s")),
    m("状态表: 登记顺序比较反了", ST, ('OR %s > %s)", a, b, ca, cb)', 'OR %s < %s)", a, b, ca, cb)')),
    m("状态表: UPDATE 原始表只重算旧的键", ST, ('fn("upd", recompute(fromOld, fromNew))', 'fn("upd", recompute(fromOld))')),
    m("状态表: DELETE 原始表没有反映", ST, ('fn("del", recompute(fromOld))', 'fn("del", "NULL;")')),
    m("状态表: TRUNCATE 原始表没有清空", ST, ('fn("trunc", fmt.Sprintf("TRUNCATE %s;", q(S)))', 'fn("trunc", "NULL;")')),
    m("状态表: 没有回填", ST, ('w("-- 回填(已有的数据;幂等)\\n%s;\\n", upsert(fromRaw))', 'w("-- 回填\\n")')),
    m("状态表: 键为空的行也进状态表", ST, ("steps = append(steps, source[2:k]...)", "_ = k")),
    # ---- 分桶的生产线(bucket.go;docs/design/hotspots.md §4)
    m("分桶: 热路径取的时候不看够不够", BK, ("AND %[2]s >= -d ORDER BY", "ORDER BY")),
    m("分桶: 回退不检查合计(不够也扣)", BK, ("IF coalesce(total, 0) < -d THEN", "IF FALSE THEN")),
    # 不列: "回退没有互斥(表的这一行的 FOR NO KEY UPDATE)" —— 热路径没取到时回滚子事务之后,回退按桶的顺序加锁,认证的并发里
    # 测不出死锁(留着互斥是防御: PostgreSQL 的 ORDER BY … FOR UPDATE 在 EvalPlanQual 时可能不按顺序)
    m("分桶: 还加到了全部的桶", BK, ("ELSIF d > 0 THEN\n        UPDATE %[5]s SET %[2]s = %[2]s + d%[6]s WHERE %[7]s AND bucket = b;",
                                   "ELSIF d > 0 THEN\n        UPDATE %[5]s SET %[2]s = %[2]s + d%[6]s WHERE %[7]s AND b = b;")),
    m("分桶: 视图不合计(最大的桶)", BK, ("sum(b.%s) AS q", "max(b.%s) AS q")),
    m("分桶: 合计设成负数不报错", BK, ("IF NEW.%[3]s < 0 THEN", "IF FALSE THEN")),
    m("分桶: 删除不删桶", BK, ('fn(s.BucketTable+"_del", fmt.Sprintf("BEGIN\\n    DELETE FROM %s WHERE %s;', 'fn(s.BucketTable+"_del", fmt.Sprintf("BEGIN\\n    PERFORM 1%.0s%.0s;')),
    # ---- 筛选越过关联下推(pushdown.go)/ 索引的键(keyindex.go、expr_json_key.go)
    m("下推: 非键的条件也移到取最新之前", P, ("n, ok := name[e.Name]", "n, ok := name[e.Name]\n\t\t\tif !ok {\n\t\t\t\tn, ok = e.Name, true\n\t\t\t}")),
    m("下推: 越过任何步骤(不只 lookup)", P, ('if st.Kind == "lookup" {', 'if st.Kind != "filter" {')),
    m("下推: 加的键列当作关联的列(找不到时 NULL)", P,
      ("delete(name, t.Name)", "if len(st.On) > 1 && t.Col == st.On[1].Key {\n\t\t\t\tname[t.Name] = name[st.On[1].Col]\n\t\t\t}")),
    m("索引的键: 越过取一条之后的条件", K, ('p.Steps[j].Kind == "filter"; j++', 'p.Steps[j].Kind != "sort"; j++')),
    m("索引的键: 整数不检查范围", JK, ("(CASE WHEN (%s::numeric BETWEEN %s AND %s) THEN", "(CASE WHEN TRUE OR (%s::numeric BETWEEN %s AND %s) THEN")),
    m("索引的键: 日期不检查月末", JK, ("(CASE WHEN (%s BETWEEN 1 AND %s) THEN make_date", "(CASE WHEN (%s BETWEEN 1 AND 31 OR %s IS NULL) THEN make_date")),
    m("索引的键: 整数的正则不认空白", JK, ('return fmt.Sprintf("%s ~ %s", v, quoteLit(f.pattern))', 'return fmt.Sprintf("%s ~ %s", v, quoteLit(`^[0-9]+$`))')),
]


def scope(text, within):
    if within is None:
        return 0, len(text)
    if text.count(within) != 1:
        raise ValueError(f"范围 {within!r} 出现 {text.count(within)} 次(必须正好 1 次)")
    start = text.rfind("\n", 0, text.index(within)) + 1
    end = text.find("\n}\n", start)
    return start, (len(text) if end < 0 else end + 3)


def apply(mu):
    """返回改过的文本;旧的文本不是正好 count 次 → ValueError(变异失效)。"""
    with open(mu["file"], encoding="utf-8") as f:
        text = f.read()
    for old, new, count in mu["edits"]:
        a, b = scope(text, mu["within"])
        n = text[a:b].count(old)
        if n != count:
            where = f"{mu['within']!r} 里" if mu["within"] else ""
            raise ValueError(f"{where}{old!r} 出现 {n} 次(应该是 {count} 次)")
        text = text[:a] + text[a:b].replace(old, new) + text[b:]
    return text


def gotest(overlay=None, verbose=False):
    cmd = ["go", "test", "-count=1"] + (["-v"] if verbose else [])
    if overlay:
        cmd += ["-overlay", overlay]
    cmd += ["./factory/core"]
    t = time.time()
    r = subprocess.run(cmd, capture_output=True, text=True, errors="replace")
    return r.returncode, r.stdout + r.stderr, time.time() - t


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--workers", type=int, default=os.cpu_count() or 4)  # 主要在等数据库: 每核一个
    ap.add_argument("--list", action="store_true", help="只检查每个变异都改得到(不需要数据库)")
    ap.add_argument("pattern", nargs="?")
    args = ap.parse_args()
    muts = [mu for mu in MUTANTS if not args.pattern or re.search(args.pattern, mu["name"])]
    names = [mu["name"] for mu in MUTANTS]
    dup = {n for n in names if names.count(n) > 1}
    if dup:
        sys.exit(f"✗ 变异的名字重复: {sorted(dup)}")

    # ① 每个变异都改得到(失效的变异 = 失败,不是跳过)
    work = tempfile.mkdtemp(prefix="core-mutate-")
    stale, texts = [], {}
    for i, mu in enumerate(muts):
        try:
            texts[i] = apply(mu)
        except (ValueError, OSError) as e:
            stale.append(f"  ✗ {mu['name']}({mu['file']}): {e}")
    if stale:
        print("变异失效(没改到代码: 按现在的代码改写,或者删掉并在 PR 里说明):")
        print("\n".join(stale))
        shutil.rmtree(work, ignore_errors=True)
        sys.exit(1)
    print(f"✓ {len(muts)} 个变异都改得到代码")
    if args.list:
        for mu in muts:
            print(f"  - {mu['name']}")
        shutil.rmtree(work, ignore_errors=True)
        return
    if not os.environ.get("FACTORY_TEST_DB"):
        sys.exit("✗ 需要 FACTORY_TEST_DB(元件认证需要 Postgres;没有它认证会跳过,变异全都'没抓到'也看不出来)")

    # ② 不改的代码认证必须通过(否则后面"失败 = 抓到"没有意义);认证没有真的跑(跳过)也算失败
    rc, out, sec = gotest(verbose=True)
    if rc != 0:
        print(out[-3000:])
        sys.exit("✗ 没有变异时元件认证就失败了")
    skipped = re.findall(r"--- SKIP: (\S+)", out)
    if skipped:
        sys.exit(f"✗ 元件认证被跳过了: {skipped}(FACTORY_TEST_DB 连不上?)")
    print(f"✓ 元件认证(不改)通过 {sec:.1f}s")

    # ③ 每个变异: overlay(工作区不动),并行
    def run(i):
        mu = muts[i]
        path = os.path.join(work, f"{i}.go")
        with open(path, "w", encoding="utf-8") as f:
            f.write(texts[i])
        ov = os.path.join(work, f"{i}.json")
        with open(ov, "w") as f:
            json.dump({"Replace": {os.path.join(ROOT, mu["file"]): path}}, f)
        rc, out, sec = gotest(ov)
        # 编译失败也算"抓到"会掩盖写坏的变异: 编译错误单独报
        compile_err = "[build failed]" in out or "[setup failed]" in out
        return i, rc, compile_err, out, sec

    t0 = time.time()
    caught, missed, broken = 0, [], []
    with cf.ThreadPoolExecutor(args.workers) as ex:
        for i, rc, cerr, out, sec in sorted(ex.map(run, range(len(muts)))):
            name = muts[i]["name"]
            if cerr:
                print(f"  ! {name}: 变异编译不过(变异写错了)\n" + "\n".join(out.splitlines()[:8]))
                broken.append(name)
            elif rc == 0:
                print(f"  ✗ 没抓到 {name}")
                missed.append(name)
            else:
                print(f"  ✓ 抓到 {name} ({sec:.0f}s)")
                caught += 1
    shutil.rmtree(work, ignore_errors=True)
    print(f"变异: 抓到 {caught} / {len(muts)}({time.time() - t0:.0f}s,{args.workers} 并行)")
    for n in missed:
        print(f"  - 没抓到: {n}")
    for n in broken:
        print(f"  - 编译不过: {n}")
    sys.exit(0 if caught == len(muts) else 1)


if __name__ == "__main__":
    main()
