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
echo "变异: 抓到 $caught / $total"
for m in "${missed[@]:-}"; do if [ -n "$m" ]; then echo "  - $m"; fi; done
[ "$caught" -eq "$total" ]
