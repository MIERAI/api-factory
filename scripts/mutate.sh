#!/usr/bin/env bash
# 变异测试: 往生成的代码里植入 bug,确认自动生成的验收用例能抓到。每个变异之后用 git 还原。
set -uo pipefail
DB_URL=${DB_URL:-postgres://factory:factory@127.0.0.1:55432/factory?sslmode=disable}
SHARD_BASE=${DB_URL%/factory\?*}
export FACTORY_SHARDS=${FACTORY_SHARDS:-"1-2=$SHARD_BASE/shard_a?sslmode=disable;*=$SHARD_BASE/shard_b?sslmode=disable"}
[[ -z ${PLAN:-} ]] && go build -o .factory/factory ./factory/cmd/factory
run() { # $1=名称 $2=文件 $3=perl 替换式
  cp "$2" /tmp/mut.bak
  perl -0pi -e "$3" "$2"
  if cmp -s "$2" /tmp/mut.bak; then echo "  ? $1: 替换式没有命中"; return; fi
  if ! go build -o bin/server ./cmd/server 2>/dev/null; then echo "  ? $1: 编译失败"; cp /tmp/mut.bak "$2"; return; fi
  out=$(.factory/factory serve-test --server bin/server --db "$DB_URL" -- .factory/factory accept --db "$DB_URL" 2>&1)
  unit=$(go test ./internal/... 2>&1 | grep -c '^--- FAIL')
  if ! echo "$out" | grep -q 'case(s)'; then echo "  ? $1: 执行器出错 — $(echo "$out" | tail -1)"; cp /tmp/mut.bak "$2"; return; fi
  fails=$(echo "$out" | grep '^  ✗')
  diff=$(echo "$fails" | grep -c ' 差分\]')
  auto=$(echo "$fails" | grep -c ' auto:')
  human=$(echo "$fails" | grep -v ' 差分\]' | grep -vc ' auto:')
  [[ -z "$fails" ]] && human=0
  verdict="✗ 漏掉"; [[ $auto -gt 0 || $unit -gt 0 || $diff -gt 0 ]] && verdict="✓ 抓到"
  printf "  %s %-36s 差分 %2d / 自动用例 %2d / 生成的单元测试 %d / 人写 %2d\n" "$verdict" "$1" "$diff" "$auto" "$unit" "$human"
  cp /tmp/mut.bak "$2"
}
# PLAN=1: 只列出变异(名称 \0 文件 \0 替换式 \0),由 scripts/mutate-par.py 并行执行
if [[ -n ${PLAN:-} ]]; then run() { printf '%s\0%s\0%s\0' "$1" "$2" "$3"; }; fi
Q=gen/db/jobs.list-jobs.sql.go
H=internal/handler/jobs/list_jobs_gen.go
run "游标比较 < 改成 <="           $Q 's/\(t2\.created_at, t2\.id\) < \(/(t2.created_at, t2.id) <= (/'
run "去掉 id 的次级排序"            $Q 's/ORDER BY t2\.created_at DESC, t2\.id DESC/ORDER BY t2.created_at DESC/'
run "id 的次级排序方向反了"          $Q 's/t2\.created_at DESC, t2\.id DESC/t2.created_at DESC, t2.id ASC/'
run "created_after: >= 改成 >"      $Q 's/created_at >= \$3/created_at > \$3/'
run "created_before: < 改成 <="     $Q 's/created_at < \$4/created_at <= \$4/'
run "status 筛选条件被忽略"             $H 's/Status:(\s+)jobsStatusPtrToDB\(m\.Status\),/Status:${1}nil,/'
run "RowLimit 没有 +1"              $H 's/RowLimit:(\s+)size \+ 1,/RowLimit:${1}size,/'
run "游标取多出来的那一行"           $H 's/last := rows\[len\(rows\)-1\]/last := rows[len(rows)-1]; _ = last; last = rows[0]/'
run "page_size 默认值写错(20→2)"     $H 's/paging\.Size\(m\.GetPageSize\(\), 20, 100\)/paging.Size(m.GetPageSize(), 2, 100)/'
run "token 不绑定筛选条件"           $H 's/fp := paging\.Fingerprint\(m, "page_token", "page_size"\)/fp := "fixed"/'
run "字段映射: job_type 漏掉"         $H 's/\n\s+JobType:\s+r\.JobType,//'
G=internal/handler/jobs/get_job_status_gen.go
run "get_by_id: NOT_FOUND 变成 INTERNAL" $G 's/return nil, apierr\.NotFound\(.*\)\n/return nil, err\n/'
E=internal/handler/jobs/enums_gen.go
run "enum 映射: DB 1/2 对调"            $E 's/case 1:\n(\s+)return jobsv1\.JobStatus_JOB_STATUS_SUCCEEDED\n(\s+)case 2:\n(\s+)return jobsv1\.JobStatus_JOB_STATUS_FAILED/case 1:\n${1}return jobsv1.JobStatus_JOB_STATUS_FAILED\n${2}case 2:\n${3}return jobsv1.JobStatus_JOB_STATUS_SUCCEEDED/'
run "enum 未知值不记录指标 [单元测试负责]"              $E 's/\tanomaly\.Report\("jobs", "status", v\)\n/\t_ = anomaly.Report\n/'
run "enum 筛选: proto→DB 映射错"         $E 's/case jobsv1\.JobStatus_JOB_STATUS_PENDING:\n(\s+)return 0/case jobsv1.JobStatus_JOB_STATUS_PENDING:\n${1}return 2/'
P=gen/db/shop.list-products.sql.go
run "前缀: 去掉 % 的转义"               $P "s/, '%', '\\\\%'\\)/, '%', '%')/"
run "前缀: 去掉 _ 的转义"               $P "s/, '_', '\\\\_'\\)/, '_', '_')/"
L=gen/db/jobs.list-jobs-by-types.sql.go
run "属于: 不传时没有 coalesce(试用里的真 bug)" $L 's/\(coalesce\(cardinality\(\$1::text\[\]\), 0\) = 0\)/(cardinality(\$1::text[]) = 0)/'
run "属于: 忽略列表条件"                $L 's/OR \(t1\.id = ANY\(\$2::int8\[\]\)\)/OR TRUE/'
S=gen/db/sales.list-top-sales.sql.go
run "小数: 按文本比较(试用里的真 bug)"    $S 's/\(t1\.amount >= \$3::numeric\)/(t1.amount::text >= \$3::text)/'
run "小数: 按文本排序(试用里的真 bug)"    $S 's/ORDER BY t2\.amount DESC/ORDER BY t2.amount::text DESC/'
M=gen/db/sales.list-store-sales.sql.go
MH=internal/handler/sales/list_store_sales_gen.go
run "多条: 超限时悄悄截断"             $MH 's/return nil, (apierr\.FailedPrecondition\([^\n]*\))/rows, _ = rows[:10], $1/'
run "多条: 没有多取一行(LIMIT 10)"     $M 's/LIMIT 11/LIMIT 10/'
V=gen/db/sales.list-prices-as-of.sql.go
run "有效期: 结束日不含当天(<= → <)"     $V 's/\(\$2::date <= t1\.valid_to\)/(\$2::date < t1.valid_to)/'
run "有效期: 结束日为空没处理"          $V 's/\(t1\.valid_to IS NULL\) OR //'
J=gen/db/catalog.list-product-master.sql.go
run "JSON版本: 取最旧的版本"            $J 's/t2\._o_registered_at DESC, t2\._o_id DESC/t2._o_registered_at ASC, t2._o_id ASC/'
run "JSON版本: 不处理删除"              $J 's/\(t3\._action <> 1::int4\) AND //'
run "JSON版本: 不处理逻辑删除"          $J "s/ AND \(\(t3\._logical_delete IS NULL\) OR \(t3\._logical_delete <> '1'::text\)\)//"
run "JSON版本: 先筛选再取最新(公司引擎的问题)" $J 's/WHERE \(NOT \(t2\.product_code IS NULL\)\)/WHERE (NOT (t2.product_code IS NULL)) AND ((\$2::text IS NULL) OR (t2.category = \$2::text))/'
run "下推: 丢掉了键上的条件(属于)"        $J 's/\n\s+AND \(\(coalesce\(cardinality\(\$1::text\[\]\), 0\) = 0\) OR \(t2\.product_code = ANY\(\$1::text\[\]\)\)\)//'
NK=gen/db/catalog.list-product-labels.sql.go
run "可以为空的键: 丢掉键为空的行(以前的工厂)"   $NK 's/WHERE \(NOT \(t2\.product_code IS NULL\)\)/WHERE (NOT (t2.product_code IS NULL)) AND (NOT (t2.label_code IS NULL))/'
run "输出改名: 取错了来源的列"               $NK 's/t4\.text AS display_text/t4.label_code AS display_text/'
run "可以为空的键: 空值和空字符串合成一组"       $NK 's/t2\.label_code COLLATE "C"/coalesce(t2.label_code, \x27\x27) COLLATE "C"/g'
EV=gen/db/catalog.list-recent-label-changes.sql.go
run "外壳列: 从 JSON 里取(没有这个键)"          $EV 's/t1\.registered_at AS updated_at/((t1.data ->> \x27updated_at\x27)::timestamptz) AS updated_at/'
run "外壳列: 取了最早的版本的时间"              $EV 's/t1\.registered_at AS updated_at/min(t1.registered_at) OVER (PARTITION BY t1.data ->> \x27product_code\x27, t1.data ->> \x27label_code\x27) AS updated_at/'
run "外壳列: 筛选 >= 写成 >"                 $EV 's/t4\.updated_at >= \$2/t4.updated_at > \$2/'
run "排序: 第二列的方向跟着第一列(以前的工厂)"   $EV 's/ORDER BY t5\.changed_at DESC, t5\.label_code COLLATE "C"/ORDER BY t5.changed_at DESC, t5.label_code COLLATE "C" DESC/'
CP=gen/db/sales.list-current-prices.sql.go
run "一览+每组取一条: 取了最早的"              $CP 's/t1\.valid_from DESC\n/t1.valid_from\n/'
run "一览+每组取一条: 没有每组取一条"          $CP 's/SELECT DISTINCT ON \(t1\.store_code, t1\.product_code COLLATE "C"\) /SELECT /'
run "一览+每组取一条: 游标 > 写成 >="          $CP 's/\(t1\.product_code COLLATE "C"\) > \(\$3::text\)/(t1.product_code COLLATE "C") >= (\$3::text)/'
FX=gen/db/sales.list-effective-price-changes.sql.go
run "固定条件: 空值也算变了(IS DISTINCT FROM)"     $FX 's/\(t1\.new_price <> t1\.old_price\)/(t1.new_price IS DISTINCT FROM t1.old_price)/'
run "固定条件: 空值的行留下(coalesce TRUE)"      $FX 's/\(\(t1\.new_price <> t1\.old_price\) OR \(t1\.deal_id = 99999999::int8\)\)/coalesce(((t1.new_price <> t1.old_price) OR (t1.deal_id = 99999999::int8)), TRUE)/'
run "固定条件: 忘了强制送信"                       $FX 's/ OR \(t1\.deal_id = 99999999::int8\)//'
run "固定条件: 常量写错"                           $FX 's/99999999::int8/99999998::int8/'
GP=internal/handler/catalog/get_product_label_gen.go
run "找不到时返回空: 变成 NOT_FOUND"              $GP 's/return connect\.NewResponse\(&catalogv1\.GetProductLabelResponse\{\}\), nil/return nil, apierr.NotFound("x")/'
run "找不到时返回空: 找到了没设 found"             $GP 's/\n\s+Found: true,//'
TP=gen/db/catalog.list-labels-by-pairs.sql.go
run "组属于: 按列各自属于(交叉组合)"            $TP 's/EXISTS \(SELECT 1 FROM [^\n]*tup2\.n = tup1\.n\)/(t2.product_code = ANY(\$1::text[])) AND (t2.label_code = ANY(\$2::text[]))/'
run "组属于: 没有按下标对齐"                    $TP 's/ AND tup2\.n = tup1\.n//'
TH=internal/handler/catalog/list_labels_by_pairs_gen.go
run "组属于: 组的字段取错(商品 ← ラベル)"        $TH 's/out = append\(out, x\.GetProductCode\(\)\)/out = append(out, x.GetLabelCode())/'
SH=internal/handler/sales/list_prices_as_of_gen.go
run "分库: 不选库(一直用主库)"           $SH 's/h\.at\(int64\(m\.GetStoreCode\(\)\)\)\./h.q./'
run "分库: 选错库(店铺代码 +1)"          $SH 's/h\.at\(int64\(m\.GetStoreCode\(\)\)\)/h.at(int64(m.GetStoreCode())+1)/'
T=gen/db/sales.list-top-stores.sql.go
run "前N条: 取的条数不对(LIMIT 4)"        $T 's/LIMIT 3/LIMIT 4/'
run "前N条: 排序方向反了"                $T 's/ORDER BY t2\.amount DESC, t2\.store_code DESC/ORDER BY t2.amount, t2.store_code/'
LD=gen/db/logistics.list-shipments-by-day.sql.go
run "关联: 保留写成丢弃(LEFT JOIN → JOIN)"     $LD 's/LEFT JOIN warehouses AS t2/JOIN warehouses AS t2/'
run "关联: NULL 当作相等"                     $LD 's/ON t1\.partner_code = t3\.partner_code/ON t1.partner_code IS NOT DISTINCT FROM t3.partner_code/'
run "关联: 关联列换成另一列"                   $LD 's/ON t1\.partner_code = t3\.partner_code/ON t1.warehouse_code = t3.partner_code/'
run "关联: 取错了对方的列"                     $LD 's/t3\.name AS carrier_name/t3.partner_code AS carrier_name/'
LZ=gen/db/logistics.list-zoned-shipments.sql.go
run "关联: 丢弃写成保留(JOIN → LEFT JOIN)"     $LZ 's/\n(\s+)JOIN zones AS t2/\n${1}LEFT JOIN zones AS t2/'
run "关联: 两列的键少比较区画番号"             $LZ 's/ AND t1\.zone_no = t2\.zone_no//'
run "关联: 两列的键少比较倉庫"                 $LZ 's/ON t1\.warehouse_code = t2\.warehouse_code AND /ON /'
run "关联: 加的列上的筛选被忽略"               $LZ 's/ OR \(t2\.label = \$2::text\)\)/ OR TRUE)/'
LR=gen/db/logistics.list-shipments-by-region.sql.go
run "关联的链: 第二级写成 JOIN"                $LR 's/LEFT JOIN regions AS t3/JOIN regions AS t3/'
run "关联的链: 第二级从来源的列关联"            $LR 's/ON t2\.region_code = t3\.region_code/ON t1.warehouse_code = t3.region_code/'
NP=gen/db/logistics.list-shipment-packages.sql.go
NH=internal/handler/logistics/list_shipment_packages_gen.go
run "嵌套: 去掉子表的条件"                     $NP 's/\n\s+AND \(NOT \(t2\.sealed_at IS NULL\)\)//'
run "嵌套: 顺序第 1 列的方向反了"               $NP 's/(t[23])\.weight DESC/$1.weight/g'
run "嵌套: 顺序第 2 列的方向反了"               $NP 's/(t[23])\.package_no(\)|\n)/$1.package_no DESC$2/g'
run "嵌套: 关联列换成另一列"                   $NP 's/WHERE t2\.shipment_id = t1\.id/WHERE t2.shipment_id = t1.zone_no/'
run "嵌套: 没有多取一行(LIMIT 3)"              $NP 's/LIMIT 4\n/LIMIT 3\n/'
run "嵌套: 上限 > 写成 >="                      $NH 's/len\(r\.Packages\) > 3/len(r.Packages) >= 3/'
run "嵌套: 超过上限时悄悄截断"                  $NH 's/(\s+)return nil, apierr\.FailedPrecondition\("too many packages[^\n]*/$1r.Packages = r.Packages[:3]/'
run "嵌套: 空列表返回 NULL"                    $NP "s/SELECT coalesce\(jsonb_agg/SELECT (jsonb_agg/; s/, '\[\]'::jsonb\) AS v/) AS v/"
run "嵌套: 十进制写成 JSON 数字"                $NP 's/\x27weight\x27, t3\.weight::text/\x27weight\x27, t3.weight/'
run "嵌套: 子行的字段映射漏掉 label"            $NH 's/\n\s+Label:\s+c\.Label,//'
run "子聚合: 空集合的合计返回 NULL"             $NP 's/coalesce\(sum\(t7\.weight\), 0\)/sum(t7.weight)/'
run "子聚合: 件数 = LEFT JOIN 的行数(空 = 1)"  $NP 's/SELECT count\(\*\) AS package_count/SELECT greatest(count(*), 1) AS package_count/'
run "子聚合: 最小 ↔ 最大"                      $NP 's/SELECT min\(t9\.sealed_at\)/SELECT max(t9.sealed_at)/'
run "子聚合: 去掉子表的条件"                    $NP 's/\n\s+AND \(NOT \(t7\.sealed_at IS NULL\)\)//'
run "子聚合: 关联列换成另一列"                  $NP 's/WHERE t5\.shipment_id = t1\.id/WHERE t5.shipment_id = t1.zone_no/'
[[ -n ${PLAN:-} ]] && exit 0
go build -o bin/server ./cmd/server; git status --short
