package perf

// The perf database's schema as the data generator sees it: every base table of the current schema (the platform's
// factory_* tables excluded: their data belongs to the runtime), all columns, keys, CHECK constraints (table and
// domain), foreign keys and identity columns. Then the plan adds what only the design knows: JSON-versioned storage
// (natural keys, versions, logical delete), enums, request-field formats and the joins of the pipelines (a lookup's
// key = a reference even without a FOREIGN KEY, so joins hit in the generated data).
//
// Rule: a constraint the generator does not understand is an error (it would insert rows that violate it, or rows that
// only satisfy it by chance). Teach checks.go the shape, never skip it.

import (
	"context"
	"fmt"
	"hash/fnv"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"apifactory/factory/core"
	"apifactory/factory/ir"
)

// ref: the value of a column / JSON key comes from another table's key (FOREIGN KEY, or a pipeline's join).
type ref struct {
	parent *table
	slot   string // the parent's physical column or JSON key
	group  string // the columns of one multi-column reference share a row of the parent (same group = same salt)
	hard   bool   // FOREIGN KEY (insertion order matters)
}

// loadSchema: every base table of the current schema with its columns and constraints.
func loadSchema(ctx context.Context, conn *pgx.Conn) (map[string]*table, error) {
	rows, err := conn.Query(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	tables := map[string]*table{}
	for _, n := range names {
		if n == "goose_db_version" || strings.HasPrefix(n, "factory_") {
			continue
		}
		t := newTable(n, nil)
		if err := loadKeys(ctx, conn, t); err != nil {
			return nil, err
		}
		if err := allColumns(ctx, conn, t); err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		tables[n] = t
	}
	if err := loadForeignKeys(ctx, conn, tables); err != nil {
		return nil, err
	}
	nameRefs(tables)
	return tables, nil
}

// nameRefs: a column named like another table's single-column primary key (debits.account_id ↔ accounts.account_id,
// reservations.sku ↔ stock.sku; same kind, no FOREIGN KEY) refers to it — the usual convention when the schema declares
// no constraint. Not "id", and only when exactly one table has that primary key.
func nameRefs(tables map[string]*table) {
	pk := map[string][]*table{} // column name → tables whose single-column primary key it is
	for _, t := range sortedTables(tables) {
		if len(t.primary) == 1 && t.primary[0] != "id" {
			pk[t.primary[0]] = append(pk[t.primary[0]], t)
		}
	}
	for _, t := range sortedTables(tables) {
		for _, c := range t.cols {
			ps := pk[c.Name]
			if len(ps) != 1 || ps[0] == t || t.unique[c.Name] {
				continue
			}
			if k, ok := ps[0].kindOf(c.Name); ok && k == c.Type.Kind {
				t.addRef(c.Name, ref{parent: ps[0], slot: c.Name, group: "name:" + c.Name})
			}
		}
	}
}

// loadForeignKeys: FOREIGN KEY (a, b) REFERENCES p (x, y) → a ← p.x, b ← p.y (one group).
func loadForeignKeys(ctx context.Context, conn *pgx.Conn, tables map[string]*table) error {
	rows, err := conn.Query(ctx, `
		SELECT c.conname, c.conrelid::regclass::text, c.confrelid::regclass::text,
		       array(SELECT a.attname::text FROM unnest(c.conkey) WITH ORDINALITY k(n, o) JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.n ORDER BY k.o),
		       array(SELECT a.attname::text FROM unnest(c.confkey) WITH ORDINALITY k(n, o) JOIN pg_attribute a ON a.attrelid = c.confrelid AND a.attnum = k.n ORDER BY k.o)
		FROM pg_constraint c JOIN pg_namespace ns ON ns.oid = c.connamespace
		WHERE c.contype = 'f' AND ns.nspname = current_schema()`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, child, parent string
		var cols, keys []string
		if err := rows.Scan(&name, &child, &parent, &cols, &keys); err != nil {
			return err
		}
		c, p := tables[strings.Trim(child, `"`)], tables[strings.Trim(parent, `"`)]
		if c == nil || p == nil {
			continue
		}
		for i, col := range cols {
			c.refs[col] = ref{parent: p, slot: keys[i], group: "fk:" + name, hard: true}
		}
	}
	return rows.Err()
}

// applyPlan: what the design knows about the tables (storages, enums, formats, joins).
func applyPlan(plan *ir.Plan, tables map[string]*table) error {
	for _, t := range tables { // enums of physical tables
		for _, c := range t.cols {
			if en := plan.Enum(t.name, c.Name); en != nil {
				t.enums[c.Name] = enumValues(en)
			}
		}
	}
	for i := range plan.Storages {
		st := &plan.Storages[i]
		t := tables[st.Table]
		if t == nil {
			return fmt.Errorf("storage %s: table %s is not in the schema", st.View, st.Table)
		}
		t.action = st.ActionColumn
		for _, c := range st.OrderBy {
			for _, x := range t.cols {
				if x.Name == c && x.Type.Kind == core.Timestamp {
					t.orderTs = c
				}
			}
		}
		if st.LogicalDeleteKey != "" {
			t.delKey[st.LogicalDeleteKey] = st.LogicalDeleteValue
		}
		keys := t.json[st.DataColumn]
		if keys == nil {
			keys = map[string]core.Kind{}
			t.json[st.DataColumn] = keys
		}
		for _, f := range st.Fields {
			if f.Column != "" { // 外壳列: a physical column
				continue
			}
			k, ok := kindOfSQL(f.Type)
			if !ok {
				return fmt.Errorf("storage %s: field %s: type %q is not known to the data generator", st.View, f.Name, f.Type)
			}
			keys[f.Key] = k
			if en := plan.Enum(st.View, f.Name); en != nil {
				t.enums[f.Key] = enumValues(en)
			}
			for pos, nk := range st.NaturalKey {
				if nk == f.Name {
					t.nkKeys[f.Key] = [2]int{pos, len(st.NaturalKey)}
				}
			}
		}
	}
	for i := range plan.APIs {
		api := &plan.APIs[i]
		if api.Pipeline == nil {
			continue
		}
		lin := lineage(plan, api.Pipeline.Steps, tables, true)
		for _, f := range api.Filters { // request fields with a format (digits / zip): the data has the format too
			fld := api.Request.Field(f.Field)
			if fld == nil || fld.Rules == nil {
				continue
			}
			if p := textPattern(string(fld.Rules.Raw)); p != "" {
				if o, ok := lin[f.Column]; ok {
					o.t.pattern[o.slot] = p
				}
			}
		}
	}
	return nil
}

// origin: where a pipeline column's values come from (a physical column or JSON key of a table).
type origin struct {
	t    *table
	slot string
}

// lineage: the pipeline's columns at the end of steps → their origins. join = also record the joins as references
// (the joined table's key ← the current row's column for lookup; the child's column ← the current row's for nest /
// child_agg), so the generated data joins.
func lineage(plan *ir.Plan, steps []core.Step, tables map[string]*table, join bool) map[string]origin {
	m := map[string]origin{}
	for _, st := range steps {
		if join { // integers read as YYYYMMDD dates (ymd(col)): the data has valid dates
			for _, e := range stepExprs(st) {
				walkExpr(e, func(x *core.Expr) {
					if x.Op == "ymd" && len(x.Args) == 1 && x.Args[0].Op == "col" {
						if o, ok := m[x.Args[0].Name]; ok {
							o.t.ymd[o.slot] = true
						}
					}
				})
			}
		}
		switch st.Kind {
		case "scan":
			m = map[string]origin{}
			if t := tables[st.Table]; t != nil {
				for _, c := range st.Columns {
					m[c.Name] = origin{t, c.Name}
				}
			}
		case "project":
			next := map[string]origin{}
			for _, x := range st.Project {
				switch {
				case x.Expr == nil:
				case x.Expr.Op == "col":
					if o, ok := m[x.Expr.Name]; ok {
						next[x.Name] = o
					}
				case x.Expr.Op == "json_field" && len(x.Expr.Args) == 1 && x.Expr.Args[0].Op == "col":
					if o, ok := m[x.Expr.Args[0].Name]; ok {
						next[x.Name] = origin{o.t, x.Expr.Name}
					}
				}
			}
			m = next
		case "group":
			next := map[string]origin{}
			for _, k := range st.Keys {
				if o, ok := m[k]; ok {
					next[k] = o
				}
			}
			m = next
		case "lookup", "nest", "child_agg":
			var other map[string]origin
			if len(st.Source) > 0 {
				other = lineage(plan, st.Source, tables, join)
			} else if t := tables[st.Table]; t != nil {
				other = map[string]origin{}
				for _, c := range st.Columns {
					other[c.Name] = origin{t, c.Name}
				}
			}
			if join {
				group := "join:" + st.Kind + ":" + st.Table + ":" + st.From
				for _, on := range st.On {
					cur, ok1 := m[on.Col]
					oth, ok2 := other[on.Key]
					if !ok1 || !ok2 || cur.t == oth.t {
						continue
					}
					if st.Kind == "lookup" { // the current row points at the looked-up row
						cur.t.addRef(cur.slot, ref{parent: oth.t, slot: oth.slot, group: group})
					} else { // the child rows point at the current row
						oth.t.addRef(oth.slot, ref{parent: cur.t, slot: cur.slot, group: group})
					}
				}
			}
			if st.Kind == "lookup" {
				for _, tk := range st.Take {
					if o, ok := other[tk.Col]; ok {
						m[tk.Name] = o
					}
				}
			}
		}
	}
	return m
}

// addRef: a join's reference, unless the slot already has one (FOREIGN KEY first, then the first join). Not on a
// column with CHECKs of its own (the parent's values might violate them), unless they are the parent's too.
func (t *table) addRef(slot string, r ref) {
	if _, ok := t.refs[slot]; ok {
		return
	}
	if t.checked[slot] && !sameChecks(t, slot, r.parent, r.slot) {
		return
	}
	t.refs[slot] = r
}

// sameChecks: the child column's CHECKs are the parent column's (so the parent's values satisfy them).
func sameChecks(t *table, c string, p *table, pc string) bool {
	if t.regex[c] != p.regex[pc] || !slices.Equal(t.literals[c], p.literals[pc]) {
		return false
	}
	a, b := t.bounds[c], p.bounds[pc]
	if a == nil || b == nil {
		return a == nil // an unbounded child accepts the parent's values
	}
	eq := func(x, y *float64) bool { return (x == nil && y == nil) || (x != nil && y != nil && *x == *y) }
	return eq(a.lo, b.lo) && eq(a.hi, b.hi) && a.loStrict == b.loStrict && a.hiStrict == b.hiStrict && a.minLen == b.minLen && a.maxLen == b.maxLen
}

// fillOrder: parents of FOREIGN KEYs first. A cycle is an error (the generator cannot satisfy it in one pass).
func fillOrder(tables map[string]*table) ([]*table, error) {
	names := make([]string, 0, len(tables))
	for n := range tables {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []*table
	state := map[string]int{} // 1 = visiting, 2 = done
	var visit func(t *table, path []string) error
	visit = func(t *table, path []string) error {
		switch state[t.name] {
		case 2:
			return nil
		case 1:
			return fmt.Errorf("foreign keys form a cycle: %s → %s (the data generator cannot fill it)", strings.Join(path, " → "), t.name)
		}
		state[t.name] = 1
		var parents []string
		for col, r := range t.refs {
			if r.hard && r.parent != t {
				parents = append(parents, r.parent.name)
			} else if r.hard && !t.nullable[col] {
				return fmt.Errorf("%s.%s: a NOT NULL self reference (the data generator cannot fill it)", t.name, col)
			}
		}
		sort.Strings(parents)
		for _, p := range slices.Compact(parents) {
			if err := visit(tables[p], append(path, t.name)); err != nil {
				return err
			}
		}
		state[t.name] = 2
		out = append(out, t)
		return nil
	}
	for _, n := range names {
		if err := visit(tables[n], nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// saltOf: a stable per-name salt (values of different columns are not correlated; a reference's parent and child
// compute the same value for the same name).
func saltOf(s string) int {
	h := fnv.New32a()
	h.Write([]byte(s))
	return int(h.Sum32() % 997)
}

func stepExprs(st core.Step) []*core.Expr {
	out := []*core.Expr{st.Pred}
	if st.AsOf != nil {
		out = append(out, st.AsOf.Ref)
	}
	for _, x := range st.Project {
		out = append(out, x.Expr)
	}
	return out
}

func walkExpr(e *core.Expr, f func(*core.Expr)) {
	if e == nil {
		return
	}
	f(e)
	for _, a := range e.Args {
		walkExpr(a, f)
	}
}
