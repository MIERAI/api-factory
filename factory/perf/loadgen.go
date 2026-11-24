package perf

// LoadRequests: realistic requests for load tests (soak / step load). For every read API, rows sampled from the real
// data fill its filters, so equality / prefix filters hit existing rows; range filters get the smaller / larger value
// of two rows (from < to). The rest of the request (and reads without declarative filters) comes from BaseRequest, the
// first OK request of the spec. Valid-at and tuple filters keep BaseRequest's values. Write APIs: loadwrite.go
// (distinct new keys for inserts, existing keys for updates / deletes, --skew). docs/loadgen.md.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"apifactory/factory/core"
	"apifactory/factory/ir"
)

type Request struct {
	API  string         `json:"api"`
	Path string         `json:"path"` // Connect: /<package>.<Service>/<Method>("" = a message, not HTTP)
	Body map[string]any `json:"body"`
	// Kind: read / write / message(消息处理器 / 状态机: 发到队列的消息的 data,Type = 消息的类型;k6 不发)
	Kind string `json:"kind"`
	Type string `json:"type,omitempty"`
	// SingleUse: 发第二次做的不是同样的事(新的键 / 删除 / 乐观锁的版本): 每个请求只发一次(k6: 按顺序取,不要随机重复)
	SingleUse bool `json:"single_use,omitempty"`
}

// LoadRequests: perAPI requests per API (per message type for state machines). skew: ParseSkew; "" = each API's
// declared distribution (IR Load.Skew: 负载.键分布, or the platform default of its kind — docs/design/hotspots.md §6).
func LoadRequests(ctx context.Context, plan *ir.Plan, db string, perAPI int, skew string, newOffset int) ([]Request, error) {
	if _, err := ParseSkew(skew); err != nil {
		return nil, err
	}
	pickFor := func(api *ir.API) (func(n, i int) int, error) {
		s := skew
		if s == "" && api.Load != nil {
			s = api.Load.Skew
		}
		return ParseSkew(s)
	}
	conn, err := pgx.Connect(ctx, db)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SET TimeZone = 'UTC'"); err != nil {
		return nil, err
	}
	var tables map[string]*table
	seq := map[string]int{}
	var out []Request
	for i := range plan.APIs {
		api := &plan.APIs[i]
		if api.Command != nil {
			if tables == nil {
				if tables, err = loadTables(ctx, conn, plan); err != nil {
					return nil, err
				}
				for _, t := range tables {
					seq[t.name] = newOffset
				}
			}
			pick, err := pickFor(api)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", api.ID, err)
			}
			rs, err := writeRequests(ctx, conn, plan, api, tables, perAPI, seq, pick)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", api.ID, err)
			}
			out = append(out, rs...)
			continue
		}
		pick, err := pickFor(api)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", api.ID, err)
		}
		rs, err := apiRequests(ctx, conn, plan, api, perAPI, pick)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", api.ID, err)
		}
		out = append(out, rs...)
	}
	return out, nil
}

func apiRequests(ctx context.Context, conn *pgx.Conn, plan *ir.Plan, api *ir.API, n int, pick func(n, i int) int) ([]Request, error) {
	path := "/" + api.ProtoPackage + "." + api.Service + "/" + api.Method
	base := func() map[string]any {
		m := map[string]any{}
		for k, v := range api.BaseRequest {
			m[k] = v
		}
		return m
	}
	var fs []ir.Filter
	for _, f := range api.Filters {
		switch f.Op {
		case "eq", "ne", "in", "prefix", "contains", "gt", "gte", "lt", "lte":
			if api.Request.Field(f.Field) != nil {
				fs = append(fs, f)
			}
		}
	}
	var out []Request
	if api.Pipeline == nil || len(fs) == 0 {
		for range n {
			out = append(out, Request{API: api.ID, Path: path, Body: base(), Kind: "read"})
		}
		return out, nil
	}
	// n*2 random rows with every filter column (as text, timestamps in RFC 3339), from the rows the filters see: the
	// pipeline before them (lookups, nests, logical tables … — sample.go). One query when one prefix has all the
	// columns; otherwise per filter (a filter after a group sees the group's rows), the i-th rows put together.
	query := func(idx []int) ([][]string, error) {
		var params, cols, exprs, conds []string
		for _, j := range idx {
			f := fs[j]
			params = append(params, f.Field)
			if !slices.Contains(cols, f.Column) {
				cols = append(cols, f.Column)
			}
			k, ok := kindOfSQL(f.ColumnType)
			if !ok {
				k = core.Text
			}
			e := "s." + pgx.Identifier{f.Column}.Sanitize()
			conds = append(conds, e+" IS NOT NULL")
			if k == core.Timestamp {
				e = "to_char(" + e + " AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')"
			}
			exprs = append(exprs, e+"::text")
		}
		prefix, args, err := prefixRows(api, params, cols, time.Now())
		if err != nil {
			return nil, err
		}
		q := fmt.Sprintf("SELECT %s FROM (%s) AS s WHERE %s ORDER BY random() LIMIT %d", strings.Join(exprs, ", "), prefix, strings.Join(conds, " AND "), n*2)
		rows, err := conn.Query(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("%w\n%s", err, q)
		}
		defer rows.Close()
		var out [][]string
		for rows.Next() {
			r := make([]string, len(idx))
			dst := make([]any, len(idx))
			for i := range r {
				dst[i] = &r[i]
			}
			if err := rows.Scan(dst...); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		return out, rows.Err()
	}
	all := make([]int, len(fs))
	for j := range fs {
		all[j] = j
	}
	sample, err := query(all)
	if err != nil { // per filter
		parts := make([][][]string, len(fs))
		sample = nil
		for j := range fs {
			if parts[j], err = query([]int{j}); err != nil {
				return nil, err
			}
			if len(parts[j]) == 0 {
				parts = nil
				break
			}
		}
		for i := 0; parts != nil && i < n*2; i++ {
			r := make([]string, len(fs))
			for j := range fs {
				r[j] = parts[j][i%len(parts[j])][0]
			}
			sample = append(sample, r)
		}
	}
	if len(sample) == 0 { // empty table: the base request only
		for range n {
			out = append(out, Request{API: api.ID, Path: path, Body: base(), Kind: "read"})
		}
		return out, nil
	}
	for i := range n {
		a, b := sample[i%len(sample)], sample[rand.IntN(len(sample))]
		if pick != nil { // skew: the sampled rows are the keys
			a = sample[pick(len(sample), i)%len(sample)]
		}
		body := base()
		for j, f := range fs {
			fld := api.Request.Field(f.Field)
			lo, hi := a[j], b[j]
			if less(hi, lo, f.ColumnType) {
				lo, hi = hi, lo
			}
			var v string
			switch f.Op {
			case "eq", "ne":
				v = a[j]
			case "prefix", "contains":
				v = string([]rune(a[j])[:min(2, len([]rune(a[j])))])
			case "gt", "gte":
				v = lo
			case "lt", "lte":
				v = hi
				if k, ok := kindOfSQL(f.ColumnType); ok && (k == core.Date || k == core.Timestamp) {
					v = plusDays(lo, 7, k) // a week: within "at most N days" rules, like a real screen's period
				}
				if v == lo { // from == to would be empty (or invalid): keep the base request's bound
					continue
				}
			case "in":
				x, err := jsonValue(plan, api, f, fld, a[j])
				if err != nil {
					return nil, err
				}
				y, err := jsonValue(plan, api, f, fld, b[j])
				if err != nil {
					return nil, err
				}
				body[f.Field] = []any{x, y}
				continue
			}
			x, err := jsonValue(plan, api, f, fld, v)
			if err != nil {
				return nil, err
			}
			if fld.List {
				x = []any{x}
			}
			body[f.Field] = x
		}
		out = append(out, Request{API: api.ID, Path: path, Body: body, Kind: "read"})
	}
	return out, nil
}

// plusDays: an ISO date / RFC 3339 timestamp + d days (unparsable: unchanged).
func plusDays(s string, d int, k core.Kind) string {
	layout := "2006-01-02"
	if k == core.Timestamp {
		layout = time.RFC3339
	}
	t, err := time.Parse(layout, s)
	if err != nil {
		return s
	}
	return t.AddDate(0, 0, d).Format(layout)
}

func less(a, b, sqlType string) bool {
	if k, ok := kindOfSQL(sqlType); ok && (k == core.Int32 || k == core.Int64 || k == core.Decimal) {
		x, e1 := strconv.ParseFloat(a, 64)
		y, e2 := strconv.ParseFloat(b, 64)
		if e1 == nil && e2 == nil {
			return x < y
		}
	}
	return a < b // ISO dates / timestamps, text
}

// jsonValue: a database value (text) as the request field's protojson value (enums: DB value → name, plan.Enums).
func jsonValue(plan *ir.Plan, api *ir.API, f ir.Filter, fld *ir.Field, s string) (any, error) {
	switch fld.Kind {
	case "int32", "int64":
		n, err := strconv.ParseInt(strings.SplitN(s, ".", 2)[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %q: %w", fld.Name, s, err)
		}
		return n, nil
	case "enum":
		db, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %q: %w", fld.Name, s, err)
		}
		if en := plan.Enum(api.Table, f.Column); en != nil {
			for _, v := range en.Values {
				if v.DB == db {
					return v.Name, nil
				}
			}
		}
		return nil, fmt.Errorf("%s: no enum value for %d", fld.Name, db)
	}
	return s, nil // string (text, dates, decimals), timestamp (RFC 3339)
}
