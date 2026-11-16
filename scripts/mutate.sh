#!/usr/bin/env bash
# 变异测试: 往生成的代码里植入 bug,确认自动生成的验收用例能抓到。每个变异之后用 git 还原。
set -uo pipefail
DB_URL=${DB_URL:-postgres://factory:factory@127.0.0.1:55432/factory?sslmode=disable}
go build -o .factory/factory ./factory/cmd/factory
run() { # $1=名称 $2=文件 $3=perl 替换式
  cp "$2" /tmp/mut.bak
  perl -0pi -e "$3" "$2"
  if cmp -s "$2" /tmp/mut.bak; then echo "  ? $1: 替换式没有命中"; return; fi
  if ! go build -o bin/server ./cmd/server 2>/dev/null; then echo "  ? $1: 编译失败"; cp /tmp/mut.bak "$2"; return; fi
  out=$(.factory/factory accept --server bin/server --db "$DB_URL" 2>&1)
  if ! echo "$out" | grep -q 'case(s)'; then echo "  ? $1: 执行器出错 — $(echo "$out" | tail -1)"; cp /tmp/mut.bak "$2"; return; fi
  human=$(echo "$out" | grep '✗' | grep -vc ' auto:')
  auto=$(echo "$out" | grep '✗' | grep -c ' auto:')
  verdict="✗ 漏掉"; [[ $auto -gt 0 ]] && verdict="✓ 抓到"
  printf "  %s %-36s 自动用例失败 %2d / 人写用例失败 %2d\n" "$verdict" "$1" "$auto" "$human"
  cp /tmp/mut.bak "$2"
}
Q=gen/db/jobs.list-jobs.sql.go
H=internal/handler/jobs/list_jobs_gen.go
run "游标比较 < 改成 <="           $Q 's/\(base\.created_at, base\.id\) < \(/(base.created_at, base.id) <= (/'
run "去掉 id 的次级排序"            $Q 's/ORDER BY base\.created_at DESC, base\.id DESC/ORDER BY base.created_at DESC/'
run "id 的次级排序方向反了"          $Q 's/base\.created_at DESC, base\.id DESC/base.created_at DESC, base.id ASC/'
run "created_after: >= 改成 >"      $Q 's/created_at >= \$3/created_at > \$3/'
run "created_before: < 改成 <="     $Q 's/created_at < \$4/created_at <= \$4/'
run "status=0 被当成未指定"          $H 's/Status:(\s+)m\.Status,/Status:${1}func() *int32 { if m.GetStatus() == 0 { return nil }; return m.Status }(),/'
run "RowLimit 没有 +1"              $H 's/RowLimit:(\s+)size \+ 1,/RowLimit:${1}size,/'
run "游标取多出来的那一行"           $H 's/last := rows\[len\(rows\)-1\]/last := rows[len(rows)-1]; _ = last; last = rows[0]/'
run "page_size 默认值写错(20→2)"     $H 's/paging\.Size\(m\.GetPageSize\(\), 20, 100\)/paging.Size(m.GetPageSize(), 2, 100)/'
run "token 不绑定筛选条件"           $H 's/fp := paging\.Fingerprint\(m, "page_token", "page_size"\)/fp := "fixed"/'
run "字段映射: job_type 漏掉"         $H 's/\n\s+JobType:\s+r\.JobType,//'
G=internal/handler/jobs/get_job_status_gen.go
run "get_by_id: NOT_FOUND 变成 INTERNAL" $G 's/return nil, apierr\.NotFound\(.*\)\n/return nil, err\n/'
go build -o bin/server ./cmd/server; git status --short
