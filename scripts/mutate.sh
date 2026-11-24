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
# MUT_ONLY=正则: 只列出名字匹配的变异(调一个变异的余量时用;margin.py 的个数检查不适用)
if [[ -n ${PLAN:-} ]]; then run() { [[ -n ${MUT_ONLY:-} && ! $1 =~ $MUT_ONLY ]] && return; printf '%s\0%s\0%s\0' "$1" "$2" "$3"; }; fi
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
run "JSON版本: 不处理删除" $J 's/\(t2\._action <> 1::int4\) AND //'
run "JSON版本: 不处理逻辑删除" $J "s/ AND \(\(t2\._logical_delete IS NULL\) OR \(t2\._logical_delete <> '1'::text\)\)//"
run "状态表: 读了原始表(全部版本,不取最新)" $J 's/FROM factory_current_product_master AS t1/FROM product_events AS t1/'
run "状态表的筛选: 丢掉了键上的条件(属于)" $J 's/\n\s+AND \(\(coalesce\(cardinality\(\$2::text\[\]\), 0\) = 0\) OR \(t2\.product_code = ANY\(\$2::text\[\]\)\)\)//'
NK=gen/db/catalog.list-product-labels.sql.go
run "可以为空的键: 丢掉键为空的行(以前的工厂)" $NK 's/AND \(NOT \(t2\.product_code IS NULL\)\)/AND (NOT (t2.product_code IS NULL)) AND (NOT (t2.label_code IS NULL))/'
run "输出改名: 取错了来源的列" $NK 's/t3\.text AS display_text/t3.label_code AS display_text/'
EV=gen/db/catalog.list-recent-label-changes.sql.go
run "外壳列: 从 JSON 里取(没有这个键)"          $EV 's/t1\.registered_at AS updated_at/((t1.data ->> \x27updated_at\x27)::timestamptz) AS updated_at/'
run "外壳列: 筛选 >= 写成 >" $EV 's/t2\.updated_at >= \$2/t2.updated_at > \$2/'
run "排序: 第二列的方向跟着第一列(以前的工厂)" $EV 's/ORDER BY t4\.changed_at DESC, t4\.label_code COLLATE "C"/ORDER BY t4.changed_at DESC, t4.label_code COLLATE "C" DESC/'
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
GT=gen/db/receipts.list-top-line-stores.sql.go
run "分组: 合计写成件数"                  $GT 's/coalesce\(sum\(t1\.amount\), 0\) AS total/count(*)::numeric AS total/'
run "分组+前N条: 排序方向反了"            $GT 's/ORDER BY t3\.total DESC, t3\.store_code/ORDER BY t3.total, t3.store_code/'
GR=gen/db/receipts.list-store-line-ranking.sql.go
run "分组: 件数写成合计"                  $GR 's/count\(\*\) AS lines/coalesce(sum(t1.quantity), 0)::int8 AS lines/'
run "名次: RANK 写成 DENSE_RANK(不跳号)"  $GR 's/rank\(\) OVER/dense_rank() OVER/'
run "名次: RANK 写成 ROW_NUMBER(并列不同名次)" $GR 's/rank\(\) OVER \(ORDER BY t2\.lines DESC\)/row_number() OVER (ORDER BY t2.lines DESC, t2.store_code)/'
GC=gen/db/receipts.list-category-sales.sql.go
run "分组: 少了分组键 category"           $GC 's/t1\.category COLLATE "C" AS category/min(t1.category COLLATE "C") AS category/; s/GROUP BY t1\.store_code, t1\.category COLLATE "C"/GROUP BY t1.store_code/'
run "分组: 少了分组键 store_code"         $GC 's/SELECT t1\.store_code AS store_code, t1\.category/SELECT min(t1.store_code) AS store_code, t1.category/; s/GROUP BY t1\.store_code, t1\.category/GROUP BY t1.category/'
run "分组: NULL 的键分成多组"             $GC 's/GROUP BY t1\.store_code, t1\.category COLLATE "C"/GROUP BY t1.store_code, t1.category COLLATE "C", CASE WHEN t1.category IS NULL THEN t1.line_id END/'
run "分组: 最小 ↔ 最大"                   $GC 's/min\(t1\.discount\) AS min_discount/max(t1.discount) AS min_discount/'
GP=gen/db/receipts.list-store-sales-totals.sql.go
run "分组+分页: 全部为空的最大值变成 0"     $GP 's/max\(t1\.discount\) AS max_discount/coalesce(max(t1.discount), 0) AS max_discount/'
run "分组+分页: 游标 > 写成 >="           $GP 's/\(t3\.store_code\) > \(\$2::int4\)/(t3.store_code) >= (\$2::int4)/'
SJ=gen/db/jobs.list-stale-pending-jobs.sql.go
SJH=internal/handler/jobs/list_stale_pending_jobs_gen.go
run "派生参数: 早于 写成 不晚于(< → <=)"        $SJ 's/t1\.created_at < \(\$1::timestamptz/t1.created_at <= (\$1::timestamptz/'
run "派生参数: 在 UTC 截日(不是领域的时区)"      $SJ 's/AT TIME ZONE \x27Asia\/Tokyo\x27\)\) AT TIME ZONE \x27Asia\/Tokyo\x27/AT TIME ZONE \x27UTC\x27)) AT TIME ZONE \x27UTC\x27/'
run "now: 读了系统时钟(不是请求开始的时刻)"      $SJH 's/FactoryNow:(\s+)clock\.Now\(ctx\)/FactoryNow:${1}clock.Now(context.Background())/'
PM=gen/db/sales.list-prev-month-sales.sql.go
run "月的加法: 月末没有截到最后一天"              $PM 's/\(\(\$2::date \+ make_interval\(months => \(-1\)\)\)::date\)/((date_trunc(\x27month\x27, \$2::date) + make_interval(months => (-1)))::date + (extract(day from \$2::date)::int - 1))/'
run "派生参数: 前月の末日を含まない(<= → <)"      $PM 's/t1\.sales_date <= \(\(date_trunc/t1.sales_date < ((date_trunc/'
# 当前状态表(docs/design/current-state.md): 生成的触发器(db/schema/zz_factory_current.sql)。服务器不变,验收用改过的表结构的库
ST=db/schema/zz_factory_current.sql
run "状态表: 写入不更新状态表(触发器什么也不做)"     $ST 's/(FUNCTION factory_current_product_master_ins\(\) RETURNS trigger LANGUAGE plpgsql AS \$factory\$\nBEGIN\n)/${1}RETURN NULL;\n/'
run "状态表: 新的版本不覆盖旧的(DO NOTHING)"        $ST 's/(FUNCTION factory_current_product_master_ins\(\).*?)DO UPDATE SET [^\n]*\n\s*WHERE [^\n]*;/${1}DO NOTHING;/s'
run "状态表: 登记顺序比较反了(取了最旧的版本)"      $ST 's/(FUNCTION factory_current_product_master_ins\(\).*?)EXCLUDED\.registered_at > s\.registered_at/${1}EXCLUDED.registered_at < s.registered_at/s'
run "状态表: 删除没有反映(删除的版本不进状态表)"    $ST 's/(FUNCTION factory_current_product_master_ins\(\).*?)FROM factory_new AS t1/${1}FROM (SELECT * FROM factory_new WHERE action_type <> 1) AS t1/s'
run "状态表: 逻辑删除没有反映"                      $ST 's/(FUNCTION factory_current_product_master_ins\(\).*?)FROM factory_new AS t1/${1}FROM (SELECT * FROM factory_new WHERE data ->> \x27deleted\x27 IS DISTINCT FROM \x271\x27) AS t1/s'
run "状态表: 可以为空的键的空值和空字符串合成一组"  $ST 's/(FUNCTION factory_current_product_label_ins\(\).*?)t3\.label_code AS _k_label_code/${1}coalesce(t3.label_code, \x27\x27) AS _k_label_code/s'
run "状态表: 关联的对方(分类)的状态表是空的"      $ST 's/(FUNCTION factory_current_category_master_ins\(\).*?)FROM factory_new AS t1/${1}FROM (SELECT * FROM factory_new WHERE FALSE) AS t1/s'
run "外壳列: 取了最早的版本的时间(状态表的更新没换外壳列)" $ST 's/(FUNCTION factory_current_product_label_ins\(\).*?)registered_at = EXCLUDED\.registered_at/${1}registered_at = s.registered_at/s'
# 分桶的生产线(docs/design/hotspots.md §4;ConsumeJobQuota 同时抢: 是): 生成的 db/schema/zz_factory_buckets.sql
BK=db/schema/zz_factory_buckets.sql
run "分桶: 视图不合计桶(只看表本身)"            $BK 's/\(t\.remaining \+ coalesce\(x\.q, 0\)\)/(t.remaining + 0)/'
run "分桶: 回退不检查合计(不够也扣)"            $BK 's/IF coalesce\(total, 0\) < -d THEN/IF FALSE THEN/'
run "分桶: 热路径不看桶够不够"                  $BK 's/ AND remaining >= -d ORDER BY/ ORDER BY/'
run "分桶: 设合计时丢了余数"                    $BK 's/CASE WHEN i < NEW\.remaining % 16 THEN 1 ELSE 0 END/0/'
run "分桶: 取的时候按桶记的列没更新"            $BK 's/, updated_at = NEW\.updated_at WHERE job_type = OLD\.job_type AND bucket = bb;/ WHERE job_type = OLD.job_type AND bucket = bb;/'
# R2.5 T1: 关联 / 嵌套 / 子聚合的对方是逻辑表(JSON版本)
LP=gen/db/catalog.list-labels-with-product.sql.go
run "逻辑表の关联: 不处理删除" $LP 's/\(t5\._action <> 1::int4\) AND //'
run "逻辑表の链: 末端不处理逻辑删除" $LP "s/ AND \(\(t8\._logical_delete IS NULL\) OR \(t8\._logical_delete <> '1'::text\)\)//"
run "逻辑表の关联: 按物理表的行(不取最新)" $LP 's/FROM factory_current_product_master AS t4/FROM product_events AS t4/'
# 筛选越过关联下推(core/pushdown.go): 键の条件はラベルの取最新の前へ。外部结合(keep)の側に入れると効かない
run "关联の下推: 键の条件を落とした" $LP 's/\n\s+AND \(t2\.product_code = ANY\(\$1::text\[\]\)\)//'
run "关联の下推: keep の关联(カテゴリ)の条件に入れた" $LP 's/\n\s+AND \(t2\.product_code = ANY\(\$1::text\[\]\)\)//; s/AND \(t8\.category = t6\.category\)/AND (t8.category = t6.category) AND (t3.product_code = ANY(\$1::text[]))/'
CP2=gen/db/catalog.list-category-products.sql.go
run "逻辑表の嵌套: 子行读了原始表(全部版本)" $CP2 's/FROM factory_current_product_label AS t7/FROM product_label_events AS t7/'
run "逻辑表の子聚合: 删除的子行也数" $CP2 's/\(t13\._action <> 1::int4\) AND //'
# R2.5 T2: 分组 / 排名之后的筛选
RT=gen/db/receipts.list-store-ranking-top-n.sql.go
run "名次 ≤ N: RANK 写成 ROW_NUMBER(并列被截断)" $RT 's/rank\(\) OVER/row_number() OVER/'
run "名次 ≤ N: <= 写成 <"                        $RT 's/\(t3\.rank <= \$2::int4\)/(t3.rank < \$2::int4)/'
# R2.5 T3: 整数编码的日期
YM=gen/db/sales.list-prev-month-last-prices.sql.go
run "整数日期: 前月の末日を含まない(<= → <)"     $YM 's/\(\(t1\.snap_ymd <= \(to_char/((t1.snap_ymd < (to_char/'
run "整数日期: 前月ではなく当月(月初 - 1 个月 が抜けた)" $YM 's/ \+ make_interval\(months => \(-1\)\)//'
# R2.5 T4: 包含
SN=gen/db/catalog.search-products-by-name.sql.go
run "包含: 前缀匹配になった"                      $SN "s/LIKE '%' \|\| replace/LIKE replace/"
run "包含: % の转义なし(通配符)"                 $SN "s/, '%', '\\\\%'\)/, '%', '%')/"
# R2.6: 今天(领域时区的日期)/ 固定条件里的 今天 / 参数名是 SQL 的关键字
RS=gen/db/sales.list-recent-snapshots.sql.go
run "今天: 按 UTC 的日期(不是领域的时区)"         $RS 's/AT TIME ZONE \x27Asia\/Tokyo\x27/AT TIME ZONE \x27UTC\x27/g'
run "今天: 未来の日付も返す(上界なし)"            $RS 's/\(t1\.snap_ymd <= \(to_char/(t1.snap_ymd <= 99999999 OR (to_char/'
CC=gen/db/logistics.list-carrier-contracts.sql.go
run "嵌套の固定条件: 今日終わる契約を含まない(>= → >)" $CC 's/\(t2\.valid_to >= /(t2.valid_to > /'
run "嵌套の固定条件: 今日始まる契約を含まない(<= → <)" $CC 's/\(t2\.valid_from <= /(t2.valid_from < /'
run "嵌套の固定条件: 終了日が空 = 無効"              $CC 's/\(t2\.valid_to IS NULL\) OR //'
run "子聚合の固定条件: 今天 を UTC で"               $CC 's/\(t5\.valid_from <= \(\(\$1::timestamptz AT TIME ZONE \x27Asia\/Tokyo\x27\)/(t5.valid_from <= ((\$1::timestamptz AT TIME ZONE \x27UTC\x27)/'
run "子聚合の固定条件: 明日以降 > 写成 >="           $CC 's/\(t7\.valid_from > /(t7.valid_from >= /'
SRG=gen/db/sales.list-store-sales-range.sql.go
run "参数名 to: <= 写成 <"                          $SRG 's/\(t1\.sales_date <= \$3::date\)/(t1.sales_date < \$3::date)/'
run "参数名 from: 被忽略"                           $SRG 's/\(t1\.sales_date >= \$2::date\)/(TRUE)/'
# R2.6 G1: 嵌套里的关联(子行上的多对一查找)
CW=gen/db/logistics.list-carrier-warehouses.sql.go
run "嵌套里的关联: 保留写成丢弃(LEFT JOIN → JOIN)"   $CW 's/LEFT JOIN warehouses AS t3/JOIN warehouses AS t3/'
run "嵌套里的关联: 链式第二级写成 JOIN"             $CW 's/LEFT JOIN regions AS t4/JOIN regions AS t4/'
run "嵌套里的关联: 链式第二级从子表的列关联"         $CW 's/ON t3\.region_code = t4\.region_code/ON t2.warehouse_code = t4.region_code/'
run "嵌套里的关联: 按加的列排序的方向反了"           $CW 's/(t[56])\.region_name COLLATE "C"/$1.region_name COLLATE "C" DESC/g'
run "嵌套里的关联: 加的列取错(倉庫名 ← 地域コード)"  $CW 's/t3\.name AS warehouse_name/t3.region_code AS warehouse_name/'
# R2.6 G4: 按日期的关联(历史表)+ 对方的列 = 常量
DC=gen/db/sales.list-daily-sales-cost-rates.sql.go
run "按日期: 开始日を含まない(<= → <)"              $DC 's/AND t2\.valid_from <= t1\.sales_date/AND t2.valid_from < t1.sales_date/'
run "按日期: 最終日を含まない(<= → <)"              $DC 's/OR t1\.sales_date <= t2\.valid_to\)/OR t1.sales_date < t2.valid_to)/'
run "按日期: 一番古い改定を取る(DESC → ASC)"         $DC 's/ORDER BY t2\.valid_from DESC/ORDER BY t2.valid_from/'
run "按日期: 最終日を見ない"                        $DC 's/\n\s+AND \(t2\.valid_to IS NULL OR t1\.sales_date <= t2\.valid_to\)//'
run "对方的列 = 常量: 区分を見ない"                  $DC 's/\n\s+AND \(t2\.rate_kind = \x27STD\x27::text\)//'
run "按日期(今天): UTC の日付"                      $DC 's/t4\.valid_from <= \(\(\$1::timestamptz AT TIME ZONE \x27Asia\/Tokyo\x27\)/t4.valid_from <= ((\$1::timestamptz AT TIME ZONE \x27UTC\x27)/'
# R2.6 G5: 分库: 随父 の表への关联
PN=gen/db/sales.list-store-price-names.sql.go
PNH=internal/handler/sales/list_store_price_names_gen.go
run "随父: 保留写成丢弃(LEFT JOIN → JOIN)"           $PN 's/LEFT JOIN local_products AS t2/JOIN local_products AS t2/'
run "随父: 别の店舗の DB を引く(店舗コード + 1)"      $PNH 's/h\.at\(int64\(m\.GetStoreCode\(\)\)\)/h.at(int64(m.GetStoreCode())+2)/'
# R2.7 H1/H2: 逻辑表への按日期的关联 + 整数编码的日期(YYYYMMDD)+ 对方的列 = 常量
PC=gen/db/catalog.list-product-costs.sql.go
run "YMD按日期: 开始の不正な日付を検査しない" $PC 's/\n\s+AND \(CASE WHEN t6\.valid_from BETWEEN[^\n]*END\)//'
run "YMD按日期: 不正な最終日を無期限扱い" $PC 's/ AND \(CASE WHEN t6\.valid_to BETWEEN[^\n]*?ELSE FALSE END\)\)\)/))/'
run "按日期(逻辑表): 最終日を含まない(<= → <)" $PC 's/\x27YYYYMMDD\x27\)\)::int4 <= t6\.valid_to/\x27YYYYMMDD\x27))::int4 < t6.valid_to/'
run "按日期(逻辑表): 一番古い改定を取る" $PC 's/ORDER BY t6\.valid_from DESC/ORDER BY t6.valid_from/'
run "对方的列 = 常量(逻辑表): 区分を見ない" $PC 's/WHERE \(t6\.cost_kind = \x27STD\x27::text\)\n\s+AND /WHERE /'
run "按日期(逻辑表): 读了原始表(全部版本)" $PC 's/FROM factory_current_product_cost AS t4/FROM product_cost_events AS t4/'
run "按日期(逻辑表・今天): UTC の日付"               $PC 's/AT TIME ZONE \x27Asia\/Tokyo\x27/AT TIME ZONE \x27UTC\x27/g'
# R2.7 H1: 嵌套里的关联(子表・对方が逻辑表)
CS=gen/db/catalog.list-category-shelf-labels.sql.go
run "嵌套里的关联(逻辑表): 保留写成丢弃" $CS 's/\) AS t6\n(\s+)LEFT JOIN LATERAL/) AS t6\n${1}JOIN LATERAL/'
run "嵌套里的关联(逻辑表): 棚札の区分を見ない" $CS 's/WHERE \(t9\.label_code = \x27SHELF\x27::text\)/WHERE TRUE/'
run "嵌套里的关联(逻辑表): 削除されたラベルも使う" $CS 's/WHERE \(\(t8\._action <> 1::int4\) AND/WHERE ((TRUE) AND/'
run "嵌套(逻辑表的子表+关联): 削除された商品も出す" $CS 's/WHERE \(\(t5\._action <> 1::int4\) AND/WHERE ((TRUE) AND/'
# R2.7 H3: 分库: 随父 の逻辑表への关联
PL=gen/db/sales.list-store-price-local-names.sql.go
PLH=internal/handler/sales/list_store_price_local_names_gen.go
run "随父(逻辑表): 别の店舗の DB を引く"             $PLH 's/h\.at\(int64\(m\.GetStoreCode\(\)\)\)/h.at(int64(m.GetStoreCode())+2)/'
run "随父(逻辑表): 削除された商品名も使う" $PL 's/WHERE \(\(t3\._action <> 1::int4\) AND/WHERE ((TRUE) AND/'
run "随父(逻辑表): 读了原始表(全部版本)" $PL 's/FROM factory_current_store_product_names AS t2/FROM store_product_name_events AS t2/'
# R2.7 H4: 入力が分库キーだけの 前N条 1(逻辑表)
GN=gen/db/sales.get-latest-store-notice.sql.go
GNH=internal/handler/sales/get_latest_store_notice_gen.go
run "最新のお知らせ: 削除を見ない" $GN 's/WHERE \(\(t2\._action <> 1::int4\) AND/WHERE ((TRUE) AND/'
run "最新のお知らせ: 論理削除を見ない" $GN 's/OR \(t2\._logical_delete <> \x271\x27::text\)/OR TRUE/'
run "最新のお知らせ: 同じ日は番号の小さい方" $GN 's/t4\.notice_no DESC/t4.notice_no/'
run "最新のお知らせ: 今天 を UTC で"                 $GN 's/AT TIME ZONE \x27Asia\/Tokyo\x27/AT TIME ZONE \x27UTC\x27/'
run "最新のお知らせ: 今日の掲載を含まない(<= → <)" $GN 's/\(t2\.posted_ymd <= \(to_char/(t2.posted_ymd < (to_char/'
# 索引的键(core/keyindex.go): 整数の键の条件は表达式索引と同じ式(不正な値は NULL)。文本での比较は ' +7 '・'07' の版を落とす
run "索引的键: 整数を文本で比较"                    $GN 's/WHERE \(\(CASE WHEN \(t1\.data ->> \x27store_code\x27\) ~ [^\n]*= \$1::int4\)/WHERE ((t1.data ->> \x27store_code\x27) = \$1::int4::text)/'
run "索引的键: 前后の空白・符号を许さない"          $GN 's/(WHERE \(\(CASE WHEN \(t1\.data ->> \x27store_code\x27\) ~ )\x27[^\x27]*\x27/$1\x27^[0-9]+\$\x27/'
run "最新のお知らせ: 別の店舗の DB"                  $GNH 's/h\.at\(int64\(m\.GetStoreCode\(\)\)\)/h.at(int64(m.GetStoreCode())+2)/'
# 命令工厂(R3-1): 生成的写代码。每个变异都要被 ≥3 个状态差分的请求抓到(factory/accept/command_diff.go)
UJ=internal/handler/schedules/update_job_schedule_gen.go
SE=internal/handler/schedules/commands_gen.go
CJ=internal/handler/jobs/create_job_gen.go
DJ=internal/handler/jobs/delete_job_gen.go
CE=internal/handler/jobs/commands_gen.go
run "命令: 去掉版本检查(乐观锁)"                 $UJ 's/\n\s+Version:\s+&txn\.Col\{Name: "version", Value: m\.GetVersion\(\)\},//'
run "命令: 版本号不加 1"                         $UJ 's/\n\s+txn\.Incr1\("version"\),//'
run "命令: 不存在 / 版本冲突 的错误码对调"        $SE 's/(case txn\.NotFound:\n\s+return connect\.NewError\()connect\.CodeNotFound(.*?case txn\.Conflict:[^\n]*\n\s+return connect\.NewError\()connect\.CodeAborted/${1}connect.CodeAborted${2}connect.CodeNotFound/s'
run "命令: 唯一约束冲突的错误码写错(→ INVALID_ARGUMENT)" $CE 's/(case txn\.Unique:\n\s+return connect\.NewError\()connect\.CodeAlreadyExists/${1}connect.CodeInvalidArgument/'
run "命令: 返回更新前的行(旧的版本号)"           $UJ 's/Version:(\s+)row\.Version,/Version:${1}m.GetVersion(),/'
run "命令: 更新漏写一列(enabled)"                $UJ 's/\n\s+\{Name: "enabled", Value: m\.GetEnabled\(\)\},//'
run "命令: 可选的输入不传时写成空值(不是默认值)" $CJ 's/if v := m\.JobType; v != nil \{\n(\s+)values = append\(values, txn\.Col\{Name: "job_type", Value: \*v\}\)\n\s+\}/values = append(values, txn.Col{Name: "job_type", Value: m.GetJobType()})/'
run "命令: 删除的键写错"                         $DJ 's/\{Name: "id", Value: m\.GetJobId\(\)\}/{Name: "id", Value: m.GetJobId() + 1}/'
# 命令工厂(R3-2): 组合命令 / 条件更新 / 幂等新建。状态差分 + 故障注入 + 并发 + 重放(factory/accept/command_prog.go)
CQ=internal/handler/runs/consume_job_quota_gen.go
SR=internal/handler/runs/start_job_run_gen.go
SQ=internal/handler/runs/submit_job_request_gen.go
RE=internal/handler/runs/commands_gen.go
run "条件更新: 去掉条件(扣成负数)"               $CQ 's/\n\s+Cond:\s+txn\.Cmp\(">=", txn\.C\("remaining"\), txn\.V\(m\.GetCount\(\)\)\),//'
run "条件更新: >= 写成 >(边界)"                 $CQ 's/txn\.Cmp\(">=", txn\.C\("remaining"\)/txn.Cmp(">", txn.C("remaining")/'
run "条件更新: 业务原因码写错"                   $CQ 's/Unmet:(\s+)"QUOTA_EXHAUSTED"/Unmet:${1}"QUOTA_EMPTY"/'
run "条件更新: 设置成请求的值(不是减去)"          $CQ 's/Expr: txn\.Sub\(txn\.C\("remaining"\), txn\.V\(m\.GetCount\(\)\)\)/Expr: txn.V(m.GetCount())/'
run "条件更新: 条件不满足的错误码写错(→ ABORTED)" $RE 's/(case txn\.Unmet:[^\n]*\n\s+ce := connect\.NewError\()connect\.CodeFailedPrecondition/${1}connect.CodeAborted/'
# "条件更新: 条件和设置不一致" 以前在 StartJobRun(扣 1 检查 >= 0): job_quotas 分桶之后(ConsumeJobQuota 同时抢: 是)数据库自己保证
# 不扣成负数(桶的 CHECK + 合计不够不更新),这个变异等价了 → 换到不分桶的 DebitAccounts(下面)
run "组合命令: 事务提前提交(拆成两个事务)"        $SR 's/\n(\t+)\},\n(\t+)txn\.Op\{ \/\/ 执行/\n${1}}); err != nil {\n\t\treturn nil, writeError(err)\n\t}\n\tif err := h.tx.Run(ctx,\n${2}txn.Op{ \/\/ 执行/'
run "组合命令: 引用了错误的返回值(日志指向别的执行)" $SR 's/return r2\.Id \}\)/return r2.Id + 1 })/'
run "组合命令: 输出取错操作的值"                 $SR 's/LogId:(\s+)r3\.Id,/LogId:${1}r2.Id,/'
run "幂等新建: 冲突改成报错(去掉 ON CONFLICT)"    $SQ 's/\n\s+Conflict:\s+\[\]string\{"request_id"\},//'
run "幂等新建: 新建的标志反了"                   $SQ 's/Created:(\s+)created1,/Created:${1}!created1,/'
run "幂等新建: 命中时返回请求的值(不是已有的行)"   $SQ 's/JobType:(\s+)r1\.JobType,/JobType:${1}m.GetJobType(),/'
# R4: 消息(发出 / 消息处理器 / 每项 / 仅当 / 跳过 / 墓碑 / 死信)
OA=internal/handler/ledger/open_account_gen.go
DA=internal/handler/ledger/debit_accounts_gen.go
run "条件更新: 条件和设置不一致(扣 x 检查 >= x - 1)" $DA 's/txn\.Cmp\(">=", txn\.C\("balance"\), txn\.V\(it\.GetAmount\(\)\)\)/txn.Cmp(">=", txn.C("balance"), txn.V(it.GetAmount() - 1))/'
RA=internal/handler/ledger/record_account_opened_gen.go
RV=internal/handler/ledger/reverse_debits_gen.go
SD=internal/handler/ledger/settle_debits_gen.go
run "发出: outbox 在提交之后另开事务写"             $OA 's/\n(\t\td1 := &ledgerv1\.AccountOpenedV1)/\n\t\treturn nil\n\t}); err != nil {\n\t\treturn nil, writeError(err)\n\t}\n\tif err := h.tx.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {\n${1}/'
run "发出: data 的字段取错(余额写成 0)"            $OA 's/(AccountOpenedV1\{[^}]*?Balance:\s+)r1\.Balance,/${1}0,/s'
run "发出: 主体取错字段"                           $OA 's/fmt\.Sprint\(d1\.GetAccountId\(\)\)/fmt.Sprint(d1.GetBalance())/'
run "消息处理: 去掉 仅当(重发会再扣)"              $DA 's/if created1 \{ \/\/ 仅当/if true { \/\/ 仅当/'
run "消息处理: 缺保存点(业务失败时部分扣减残留)"     $DA 's/err := txn\.Savepoint\(ctx, tx, func\(/err := (func(/; s/\n\t\}\)\n\tresult, err := handlerResult/\n\t})(ctx, tx)\n\tresult, err := handlerResult/'
run "消息处理: 业务失败发出了成功的事件类型"         $DA 's/(case "INSUFFICIENT_FUNDS":.*?)"account\.debit_failed"/${1}"account.debited"/s'
run "消息处理: 业务结果的 data 取错(原因码)"        $DA 's/Reason:(\s+)"INSUFFICIENT_FUNDS",/Reason:${1}"REVERSED",/'
run "消息处理: 任一项 的判定反了(墓碑没挡住)"        $DA 's/int64\(r1\.Status\) == int64\(3\)/int64(r1.Status) != int64(3)/'
run "每项: 只处理第一项"                           $DA 's/for _, it := range items \{/for _, it := range items[:1] {/'
run "消息处理: 事件 id 的主键取了消息的字段"         $RA 's/\{Name: "event_id", Value: env\.ID\}/{Name: "event_id", Value: m.GetAccountId()}/'
run "消息处理: 如果 的边界 >= 写成 >"              $RA 's/int64\(m\.GetBalance\(\)\) >= int64\(1000000\)/int64(m.GetBalance()) > int64(1000000)/'
run "跳过: 条件不成立当成错误"                     $RV 's/\} else if txn\.ReasonOf\(err\) != txn\.Unmet \{ \/\/ 条件不成立: 跳过/} else { \/\/ 条件不成立: 跳过/'
run "仅当 已更新: 跳过时也退回"                     $RV 's/if updated3 \{ \/\/ 仅当 检查/if updated3 || true { \/\/ 仅当 检查/'
run "墓碑写成 HELD(迟到的扣款照样扣)"              $RV 's/\{Name: "status", Value: int32\(3\)\},/{Name: "status", Value: int32(1)},/'
run "死信的结果提交了(不进 DLQ)"                   $SD 's/return consume\.Poison\(errors\.New\("ALREADY_REVERSED: [^"]*"\)\)/_ = errors.New; return nil/'
# R5: 状态机(转移 / 分类 / 守卫 / 超时 / 期限 / 乐观锁 / 载入)+ 组合命令的 每项
DR=internal/handler/approvals/document_review_gen.go
SDOC=internal/handler/approvals/submit_document_gen.go
run "状态机: 不检查当前状态(终态也接受转移)"       $DR 's/Cond:(\s+)txn\.Cmp\("=", txn\.C\("status"\), txn\.V\(int32\(2\)\)\),/Cond:${1}txn.Cmp("<>", txn.C("status"), txn.V(int32(0))),/'
run "状态机: 转移到错的状态"                     $DR 's/\{Name: "status", Value: int32\(3\)\},/{Name: "status", Value: int32(5)},/'
run "状态机: 去掉守卫"                           $DR 's/txn\.And\((txn\.Cmp\("=", txn\.C\("status"\), txn\.V\(int32\(1\)\)\)), txn\.Cmp\("=", txn\.V\(m\.GetRevision\(\)\), txn\.C\("revision"\)\)\)/$1/'
run "状态机: 忽略的消息也发出了事件"               $DR 's/(OnReviewRejected\(ctx.*?sm\.Ignored\("DocumentReview", "late"\)\n\t\t\tconsume\.Business\(ctx, "IGNORED"\)\n\t\t\t)return nil/${1}return h.emit(ctx, tx, emitMsg(message.Event, "document.rejected", 1, fmt.Sprint(m.GetDocId()), &approvalsv1.DocumentRejectedV1{DocId: m.GetDocId(), Reason: m.GetReason()}))/s'
run "状态机: 迟到当成不可能(进 DLQ)"             $DR 's/case 2, 3, 4, 5: \/\/ 迟到/case 99: \/\/ 迟到/'
run "状态机: 不可能当成迟到(吞掉)"               $DR 's/case 3: \/\/ 迟到/case 1, 3, 4, 5: \/\/ 迟到/'
run "状态机: 进入等待状态发出别的命令"             $DR 's/emitMsg\(message\.Command, "publish\.request", 1, fmt\.Sprint\(d1\.GetDocId\(\)\), d1\)/emitMsg(message.Command, "review.request", 1, fmt.Sprint(d1.GetDocId()), d1)/'
run "状态机: 进入终态不发出事件"                   $DR 's/return h\.emit\(ctx, tx,\n\t\temitMsg\(message\.Event, "document\.published", 1, fmt\.Sprint\(d1\.GetDocId\(\)\), d1\),\n\t\)/_ = d1\n\treturn nil/'
run "状态机: 进入新状态不清零重发计数"             $DR 's/\n\t\t\t\{Name: "attempts", Value: int32\(0\)\},//'
run "超时: 刚好到 不重发(<= 写成 <)"              $DR 's/if !cur\.Entered\.Add\(time\.Duration\(30\+60\*int64\(cur\.Attempts\)\) \* time\.Second\)\.After\(now\)/if cur.Entered.Add(time.Duration(30+60*int64(cur.Attempts)) * time.Second).Before(now)/'
run "超时: 扫描的比较 <= 写成 <"                 $DR 's/\(30 \+ 60 \* \\"attempts\\"\) <= \$1/(30 + 60 * \\"attempts\\") < \$1/'
run "超时: 重发不累加计数"                        $DR 's/\n\t+\{Name: "attempts", Expr: txn\.Add\(txn\.C\("attempts"\), txn\.V\(int32\(1\)\)\)\},//'
run "超时: 期限差一秒"                            $DR 's/cur\.Entered\.Add\(3600 \* time\.Second\)/cur.Entered.Add(3601 * time.Second)/'
run "超时: 期限的扫描条件漏了"                     $DR 's/ OR \\"entered_at\\" \+ interval .3600 seconds. <= \$1//'
run "超时: 去掉版本的乐观锁(两个扫描各重发一次)"   $DR 's/\n\t+Version:\s+&txn\.Col\{Name: "version", Value: cur\.Version\},//g'
run "载入: 子行的顺序反了"                        $DR 's/ORDER BY \\"reviewer\\" ASC/ORDER BY \\"reviewer\\" DESC/'
run "每项(组合命令): 只处理第一项"                 $SDOC 's/for _, it := range items2 \{/for _, it := range items2[:1] {/'
run "每项(组合命令): 发出的列表少了元素"           $SDOC 's/for _, x := range m\.GetReviewers\(\) \{/for _, x := range m.GetReviewers()[:1] {/'
[[ -n ${PLAN:-} ]] && exit 0
go build -o bin/server ./cmd/server; git status --short
