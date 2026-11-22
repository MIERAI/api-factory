#!/usr/bin/env bash
# 元件认证的变异测试: 故意把 SQL 后端 / 参考求值器改错,元件认证必须失败。
# 用法: FACTORY_TEST_DB=postgres://... scripts/core-mutate.sh
set -u
cd "$(dirname "$0")/.."
: "${FACTORY_TEST_DB:?需要 FACTORY_TEST_DB}"
caught=0; total=0; missed=()
mutate() { # 名字 文件 sed表达式
  local name=$1 file=$2 expr=$3
  total=$((total+1))
  cp "$file" "$file.orig"
  sed -i '' "$expr" "$file"
  if cmp -s "$file" "$file.orig"; then
    echo "  ? $name: sed 没有改到任何东西"; missed+=("$name(没改到)")
  elif go test ./factory/core -run TestCertification -count=1 >/dev/null 2>&1; then
    echo "  ✗ 没抓到 $name"; missed+=("$name")
  else
    echo "  ✓ 抓到 $name"; caught=$((caught+1))
  fi
  mv "$file.orig" "$file"
}
S=factory/core/sql.go; E=factory/core/eval.go
mutate "SQL: 排序去掉 COLLATE C"        $S 's|e += ` COLLATE "C"`$|e += ""|'
mutate "SQL: 比较去掉 COLLATE C"        $S 's|a += ` COLLATE "C"`|a += ""|'
mutate "SQL: 前缀去掉转义"              $S "s|(%s LIKE replace(replace(replace(%s, '\\\\', '\\\\\\\\'), '%%', '\\\\%%'), '_', '\\\\_') \\|\\| '%%')|(%s LIKE %s \\|\\| '%%')|"
mutate "SQL: 列表不传时没有 coalesce"   $S 's|(coalesce(cardinality(%s), 0) = 0)|(cardinality(%s) = 0)|'
mutate "SQL: DESC 的 NULL 放最后"       $S 's|e += " DESC"|e += " DESC NULLS LAST"|'
mutate "SQL: 游标 > 改成 >="            $S 's|op := ">"|op := ">="|'
mutate "SQL: 每键取一条丢掉顺序"        $S 's|order = append(order, g.orderItems(lv, lv.dOrder)...)|_ = 0|'
mutate "SQL: 标量不传 → IS NOT NULL"    $S 's|return "(" + p + " IS NULL)", nil|return "(" + p + " IS NOT NULL)", nil|'
mutate "SQL: 游标去掉 COLLATE C"        $S 's|				s += ` COLLATE "C"`|				s += ""|'
mutate "参考: and 把 UNKNOWN 当 FALSE"  $E 's|			if v == nil {\n				unknown = true|&|; s|				unknown = true|				unknown = false|'
mutate "参考: NULL 排在最前"            $E 's|		return 1\n	case b == nil:|&|; /case a == nil:/{n;s|return 1|return -1|;}'
mutate "参考: 文本不按字节(忽略大小写)" $E 's|return bytes.Compare(\[\]byte(x), \[\]byte(b.(string)))|return strings.Compare(strings.ToLower(x), strings.ToLower(b.(string)))|'
mutate "参考: 空列表 → 不是 absent"     $E 's|return len(l) == 0|return false|'
mutate "SQL: 组属于没有按下标对齐"       $S 's|conds = append(conds, fmt.Sprintf("tup%d.n = tup1.n", i+1))|_ = 0|'
mutate "SQL: 组属于用 IN(三值)"          $S 's|return fmt.Sprintf("EXISTS (SELECT 1 FROM %s WHERE %s)", strings.Join(from, ", "), strings.Join(conds, " AND ")), nil|return fmt.Sprintf("(CASE WHEN EXISTS (SELECT 1 FROM %s WHERE %s) THEN TRUE WHEN %s IS NULL THEN NULL ELSE FALSE END)", strings.Join(from, ", "), strings.Join(conds, " AND "), "NULL::bool"), nil|'
mutate "参考: 组属于列为 NULL → UNKNOWN"  $E '/case "in_tuples":/,/vals\[i\] = v/s|return false, nil|return nil, nil|'
mutate "SQL: lookup 的 keep 写成 JOIN"     $S 's|join := "LEFT JOIN"|join := "JOIN"|'
mutate "SQL: lookup 的 drop 写成 LEFT JOIN" $S 's|join = "JOIN"|join = "LEFT JOIN"|'
mutate "SQL: lookup NULL 当作相等"        $S 's|"%s = %s.%s", g.col(lv, k.Col)|"%s IS NOT DISTINCT FROM %s.%s", g.col(lv, k.Col)|'
mutate "SQL: lookup 多列键只比较第一列"     $S 's|ON %s", join, quoteIdent(st.Table), alias, strings.Join(conds, " AND "))|ON %s", join, quoteIdent(st.Table), alias, conds[0])|'
mutate "SQL: lookup 键按文本忽略大小写比较"    $S 's|"%s = %s.%s", g.col(lv, k.Col)|"lower(%s::text) = lower(%s.%s::text)", g.col(lv, k.Col)|'
mutate "参考: lookup NULL 键也进索引"       $E 's|if !st.NullEq {|if false {|'
mutate "参考: lookup drop 不丢弃"          $E 's|			if st.Missing == "drop" {|			if false {|'
mutate "参考: lookup 文本键忽略大小写"       $E 's|b.WriteString(Canonical(v))|b.WriteString(strings.ToLower(Canonical(v)))|'
A=factory/core/agg.go
mutate "SQL: nest 空列表 → NULL(没有 coalesce)" $S "s|coalesce(jsonb_agg(jsonb_build_object(%s) ORDER BY %s), '\[\]'::jsonb)|jsonb_agg(jsonb_build_object(%s) ORDER BY %s)|"
mutate "SQL: nest 没有多取一行(LIMIT N)"     $S 's|strings.Join(g.orderItems(child, st.Order), ", "), st.N+1)|strings.Join(g.orderItems(child, st.Order), ", "), st.N)|'
# 不列: "nest 的 JSON 聚合没有 ORDER BY" —— Postgres 实际按有序子查询的顺序聚合,测试看不出来(SQL 标准不保证,所以照写 ORDER BY)
mutate "SQL: nest 十进制写成 JSON 数字"      $S 's|return x + "::text"|return x|'
mutate "SQL: nest 时间不换成 UTC"            $S "s| AT TIME ZONE 'UTC', 'YYYY|, 'YYYY|"
mutate "SQL: nest 日期按会话的写法"          $S "s|return \"to_char(\" + x + \", 'YYYY-MM-DD')\"|return x + \"::text\"|"
mutate "SQL: 子表的关联 NULL 当作相等"       $S 's|"%s = %s", g.col(child, k.Key)|"%s IS NOT DISTINCT FROM %s", g.col(child, k.Key)|'
mutate "SQL: 子表的条件被忽略"              $S 's|	if st.Pred != nil {|	if false \&\& st.Pred != nil {|'
mutate "SQL: 子表的顺序方向反了"            $S 's|strings.Join(g.orderItems(w, st.Order), ", ")|strings.Join(g.orderItems(w, reversed(st.Order)), ", ")|; s|^func takeCols|func reversed(o []SortKey) []SortKey { r := append([]SortKey(nil), o...); for i := range r { r[i].Desc = !r[i].Desc }; return r }\nfunc takeCols|'
mutate "SQL: sum 空集合 → NULL"             $A 's|return fmt.Sprintf("coalesce(sum(%s), 0)", x)|return fmt.Sprintf("sum(%s)", x)|'
mutate "SQL: count = LEFT JOIN 的行数(空 = 1)" $A 's|return "count(\*)"|return "greatest(count(*), 1)"|'
mutate "SQL: min ↔ max"                    $A 's|return fmt.Sprintf("%s(%s)", a.Func, x)|return fmt.Sprintf("%s(%s)", map[string]string{"min": "max", "max": "min"}[a.Func], x)|'
mutate "SQL: min / max 文本没有 COLLATE C"   $A 's|x += ` COLLATE "C"`|x += ""|'
mutate "参考: nest 空列表 → NULL"            $E 's|if len(list) == 0 \&\& st.Mutant == "empty_null" {|if len(list) == 0 {|'
mutate "参考: nest 不截断"                  $E 's|if len(group) > st.N+1 {|if false \&\& len(group) > st.N+1 {|'
mutate "参考: 子表的关联 NULL 也匹配"        $E '/^func childKey/,/^}/s|return "", false|b.WriteString("∅")|'
mutate "参考: 子表的条件被忽略"              $E '/^func (p \*Pipeline) evalChildren/,/^}/s|if st.Pred != nil {|if false \&\& st.Pred != nil {|'
mutate "参考: sum 空集合 → NULL"             $E 's|len(group) == 0 \&\& a.Func == "sum" \&\& st.Mutant == "sum_empty_null"|len(group) == 0 \&\& a.Func == "sum"|'
mutate "参考: min / max 不忽略 NULL"         $A '/case "min", "max":/,/return best/s|continue|return nil, nil|'
A=factory/core/agg.go
mutate "聚合 SQL: 空集合的 sum 返回 NULL"   $A 's|return fmt.Sprintf("coalesce(sum(%s), 0)", x)|return fmt.Sprintf("sum(%s)", x)|'
mutate "聚合 SQL: 整数 sum 没有 cast"       $A 's|return fmt.Sprintf("coalesce(sum(%s), 0)::int8", x)|return fmt.Sprintf("coalesce(sum(%s), 0)", x)|'
mutate "聚合 SQL: min/max 去掉 COLLATE C"   $A 's|x += ` COLLATE "C"`|x += ""|'
mutate "聚合 参考: sum 不忽略 NULL(当 0 以外)" $A 's|if v != nil {\n\t\t\t\t\ts.Add(s, v.(\*big.Rat))|&|; s|s.Add(s, v.(\*big.Rat))|s.Add(s, v.(*big.Rat)); s.Add(s, big.NewRat(1, 100))|'
mutate "聚合 参考: min ↔ max"               $A 's|(a.Func == "min" \&\& c < 0)|(a.Func == "min" \&\& c > 0)|'
mutate "聚合 参考: 整数溢出不报错"           $A 's|if !s.IsInt64() {|if false {|'
# GROUP BY 去掉 COLLATE "C" 是等价变异: 确定性的排序规则下相等 = 按字节相等(Postgres 用 strcmp 决胜),分组结果不变。
# 写 COLLATE "C" 是为了和 sort / DISTINCT ON 的写法一致(设计稿 §2.4),不是为了正确性。
mutate "SQL: rank → dense_rank"             $S 's|"rank() OVER (ORDER BY "|"dense_rank() OVER (ORDER BY "|'
mutate "SQL: rank → row_number"             $S 's|"rank() OVER (ORDER BY "|"row_number() OVER (ORDER BY "|'
mutate "参考: NULL 的键每个一组"             $E '/func evalGroup/,/^}/s|k := keyString(r, st.Keys)|k := keyString(r, st.Keys); if strings.Contains(k, "∅") { k += fmt.Sprint(len(groups)) }|'
mutate "参考: 名次不跳号(dense_rank)"        $E '/func evalRank/,/^}/s|if compareRows(o, r, st.Order) < 0 {|if compareRows(o, r, st.Order) < 0 \&\& false {|'
echo "变异: 抓到 $caught / $total"
for m in "${missed[@]:-}"; do if [ -n "$m" ]; then echo "  - $m"; fi; done
[ "$caught" -eq "$total" ]
