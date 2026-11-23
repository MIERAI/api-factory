package perf

// The data generator: one INSERT … SELECT … FROM generate_series per table. Every value is a SQL expression of the
// row number, so a reference (FOREIGN KEY or a pipeline's join) computes the parent's key for some parent row
// instead of reading the parent: joins hit, and the same expressions give the same data on every run.

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"apifactory/factory/core"
)

// table: one physical table to generate.
type table struct {
	name       string
	n          int
	cols       []core.NamedType
	nullable   map[string]bool
	hasDefault map[string]bool
	unique     map[string]bool
	composite  map[string][2]int               // composite primary key / unique constraint: column → (position, columns)
	members    map[string][]string             // composite column → the constraint's columns (in order)
	json       map[string]map[string]core.Kind // JSON column → key → kind
	nkKeys     map[string][2]int               // natural key of JSON versions → (position, keys): ~5 versions per key
	delKey     map[string]string               // logical delete key → value
	action     string                          // the action column (delete = 1)
	orderTs    string                          // the registration order (timestamp)
	enums      map[string][]int64              // enum column (physical or JSON key) → DB values
	checked    map[string]bool                 // physical column in a CHECK constraint (dirty data: NULL only)
	literals   map[string][]string             // CHECK's list of values (SQL literals): taken in turn
	pattern    map[string]string               // text column / JSON key → request field format (digits / zip)
	bounds     map[string]*bound               // CHECK ranges / lengths
	regex      map[string]string               // CHECK col ~ 'regex'
	refs       map[string]ref                  // column / JSON key → a parent's key
	ymd        map[string]bool                 // integer column / JSON key read as a YYYYMMDD date (ymd)
	serial     []string                        // identity / serial columns: the sequence continues after the data
	dirty      bool
}

func newTable(name string, cols []core.NamedType) *table {
	return &table{name: name, cols: cols, nullable: map[string]bool{}, hasDefault: map[string]bool{}, unique: map[string]bool{},
		composite: map[string][2]int{}, members: map[string][]string{}, json: map[string]map[string]core.Kind{},
		nkKeys: map[string][2]int{}, delKey: map[string]string{}, enums: map[string][]int64{}, checked: map[string]bool{},
		literals: map[string][]string{}, pattern: map[string]string{}, bounds: map[string]*bound{}, regex: map[string]string{},
		refs: map[string]ref{}, ymd: map[string]bool{}}
}

// loadKeys: primary key / unique constraints (one column → unique, several → composite).
func loadKeys(ctx context.Context, conn *pgx.Conn, t *table) error {
	rows, err := conn.Query(ctx, `
		SELECT c.conname, array(SELECT a.attname::text FROM unnest(c.conkey) WITH ORDINALITY k(n, o)
		                        JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.n ORDER BY k.o)
		FROM pg_constraint c WHERE c.conrelid = to_regclass($1) AND c.contype IN ('p', 'u') ORDER BY c.contype, c.conname`,
		pgx.Identifier{t.name}.Sanitize())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var cols []string
		if err := rows.Scan(&name, &cols); err != nil {
			return err
		}
		if len(cols) == 1 {
			t.unique[cols[0]] = true
			continue
		}
		for pos, c := range cols {
			if _, seen := t.composite[c]; !seen {
				t.composite[c] = [2]int{pos, len(cols)}
				t.members[c] = cols
			}
		}
	}
	return rows.Err()
}

var valueWord = regexp.MustCompile(`\bVALUE\b`)

// allColumns: the table's columns (types the generator knows; others must be nullable or have a default), CHECK
// constraints of the table and of the columns' domains.
func allColumns(ctx context.Context, conn *pgx.Conn, t *table) error {
	rows, err := conn.Query(ctx, `
		SELECT column_name, udt_name, is_nullable = 'YES', column_default IS NOT NULL OR is_identity = 'YES',
		       is_generated = 'ALWAYS', coalesce(column_default LIKE 'nextval(%' OR is_identity = 'YES', false), coalesce(domain_name, '')
		FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1 ORDER BY ordinal_position`, t.name)
	if err != nil {
		return err
	}
	type col struct {
		name, udt, domain           string
		nullable, def, gen, serial bool
	}
	var cols []col
	for rows.Next() {
		var c col
		if err := rows.Scan(&c.name, &c.udt, &c.nullable, &c.def, &c.gen, &c.serial, &c.domain); err != nil {
			rows.Close()
			return err
		}
		cols = append(cols, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range cols {
		if c.gen {
			continue
		}
		t.nullable[c.name], t.hasDefault[c.name] = c.nullable, c.def
		k, ok := kindOfSQL(c.udt)
		if c.udt == "jsonb" || c.udt == "json" {
			k, ok = core.JSON, true
		}
		if !ok {
			if !c.nullable && !c.def {
				return fmt.Errorf("column %s: type %s is not known to the data generator (NOT NULL without a default; teach kindOfSQL)", c.name, c.udt)
			}
			continue // left to its default / NULL
		}
		t.cols = append(t.cols, core.NamedType{Name: c.name, Type: core.Type{Kind: k, Nullable: c.nullable}})
		if c.serial {
			t.serial = append(t.serial, c.name)
		}
		if c.domain != "" {
			drows, err := conn.Query(ctx, `SELECT pg_get_constraintdef(pc.oid) FROM pg_constraint pc
				JOIN pg_type ty ON ty.oid = pc.contypid JOIN pg_namespace ns ON ns.oid = ty.typnamespace
				WHERE ty.typname = $1 AND ns.nspname = current_schema() AND pc.contype = 'c'`, c.domain)
			if err != nil {
				return err
			}
			defs, err := pgx.CollectRows(drows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			for _, d := range defs {
				if err := t.parseCheck(valueWord.ReplaceAllString(d, c.name)); err != nil {
					return fmt.Errorf("domain %s of %s: %w", c.domain, c.name, err)
				}
			}
		}
	}
	crows, err := conn.Query(ctx, `SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c
		WHERE c.contype = 'c' AND c.conrelid = to_regclass($1) ORDER BY c.conname`, pgx.Identifier{t.name}.Sanitize())
	if err != nil {
		return err
	}
	defs, err := pgx.CollectRows(crows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, d := range defs {
		if err := t.parseCheck(d); err != nil {
			return err
		}
	}
	return nil
}

func kindOfSQL(s string) (core.Kind, bool) {
	switch s {
	case "int4", "integer", "int2", "smallint", "serial":
		return core.Int32, true
	case "int8", "bigint", "bigserial":
		return core.Int64, true
	case "text", "varchar", "bpchar":
		return core.Text, true
	case "bool", "boolean":
		return core.Bool, true
	case "numeric":
		return core.Decimal, true
	case "date":
		return core.Date, true
	case "timestamptz", "timestamp":
		return core.Timestamp, true
	}
	return "", false
}

// fill: TRUNCATE (all tables, by the caller) is done; insert n rows.
func fill(ctx context.Context, conn *pgx.Conn, t *table) error {
	ident := pgx.Identifier{t.name}.Sanitize()
	var cols, exprs []string
	for _, c := range t.cols {
		e, err := t.colExpr(c, "i", 0, true)
		if err != nil {
			return fmt.Errorf("column %s: %w", c.Name, err)
		}
		cols = append(cols, pgx.Identifier{c.Name}.Sanitize())
		exprs = append(exprs, e)
	}
	sql := fmt.Sprintf("INSERT INTO %s (%s) OVERRIDING SYSTEM VALUE SELECT %s FROM generate_series(1, %d) AS i", ident, strings.Join(cols, ", "), strings.Join(exprs, ", "), t.n)
	if _, err := conn.Exec(ctx, sql); err != nil {
		return fmt.Errorf("%w\n%s", err, sql)
	}
	for _, c := range t.serial {
		if _, err := conn.Exec(ctx, fmt.Sprintf("SELECT setval(pg_get_serial_sequence(%s, %s), greatest(1, (SELECT max(%s) FROM %s)))",
			quote(ident), quote(c), pgx.Identifier{c}.Sanitize(), ident)); err != nil {
			return fmt.Errorf("sequence of %s: %w", c, err)
		}
	}
	_, err := conn.Exec(ctx, "ANALYZE "+ident)
	return err
}

func (t *table) kindOf(col string) (core.Kind, bool) {
	for _, c := range t.cols {
		if c.Name == col {
			return c.Type.Kind, true
		}
	}
	return "", false
}

// colExpr: the value of a physical column for row idx (an int expression, 1-based). top = the column itself is being
// inserted (dirty data allowed); otherwise a child computes the parent's key.
func (t *table) colExpr(c core.NamedType, idx string, depth int, top bool) (string, error) {
	if depth > 8 {
		return "", fmt.Errorf("references of %s.%s form a cycle", t.name, c.Name)
	}
	salt := saltOf(t.name + "." + c.Name)
	k := c.Type.Kind
	r, hasRef := t.refs[c.Name]
	switch {
	case k == core.JSON:
		return t.jsonExpr(c.Name, idx, depth)
	case c.Name == t.action && t.action != "": // delete: 2%
		return fmt.Sprintf("CASE WHEN (%s) %% 50 = 0 THEN 1 ELSE 0 END", idx), nil
	case len(t.enums[c.Name]) > 0:
		return enumSQL(t.enums[c.Name], salt, idx), nil
	case len(t.literals[c.Name]) > 0:
		return fmt.Sprintf("(ARRAY[%s])[1 + ((%s) + %d) %% %d]", strings.Join(t.literals[c.Name], ","), idx, salt, len(t.literals[c.Name])), nil
	case hasRef && r.hard && r.parent == t: // self reference (nullable: checked by fillOrder)
		return "NULL", nil
	case !t.unique[c.Name] && t.composite[c.Name][1] > 1:
		return t.compositeExpr(c, idx, depth)
	case hasRef:
		j := "(" + hashSQL(idx, saltOf(r.group), r.parent.n) + " + 1)"
		if t.unique[c.Name] { // one-to-one: row i → parent row i
			j = idx
		}
		return t.refExpr(r, k, j, depth)
	case t.unique[c.Name]:
		return t.uniqueExpr(c, idx)
	case c.Name == t.orderTs && t.orderTs != "":
		return fmt.Sprintf("timestamptz '2024-01-01 00:00:00+00' + make_interval(secs => (%s)::int)", idx), nil
	case t.regex[c.Name] != "":
		v := hashSQL(idx, salt, 5000)
		e, err := regexSQL(t.regex[c.Name], v, 5000)
		if err != nil {
			e, err = regexSQL(t.regex[c.Name], v, 1)
		}
		return e, err
	case k == core.Text && t.pattern[c.Name] != "":
		return patternSQL(t.pattern[c.Name], false, salt, idx), nil
	case t.ymd[c.Name]:
		return ymdSQL(salt, idx), boundsUnsupported(t.bounds[c.Name], "YYYYMMDD")
	}
	e, err := valueSQL(k, salt, false, idx, t.bounds[c.Name], t.n)
	if err != nil {
		return "", err
	}
	if top && t.dirty {
		e = dirtySQL(k, e, t.nullable[c.Name], t.checked[c.Name], salt)
	}
	return e, nil
}

// refExpr: the parent's value of r.slot at parent row j, as kind k.
func (t *table) refExpr(r ref, k core.Kind, j string, depth int) (string, error) {
	pk, isCol := r.parent.kindOf(r.slot)
	var v string
	var err error
	if isCol && pk != core.JSON {
		v, err = r.parent.colExpr(core.NamedType{Name: r.slot, Type: core.Type{Kind: pk}}, j, depth+1, false)
	} else if jc, kk, ok := r.parent.jsonKey(r.slot); ok {
		v, err = r.parent.keyExpr(jc, r.slot, kk, j, depth+1)
	} else {
		return "", fmt.Errorf("reference to %s.%s: no such column or JSON key", r.parent.name, r.slot)
	}
	if err != nil {
		return "", err
	}
	return "(" + v + ")::" + k.SQLType(), nil
}

func (t *table) jsonKey(key string) (string, core.Kind, bool) {
	cols := make([]string, 0, len(t.json))
	for c := range t.json {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	for _, c := range cols {
		if k, ok := t.json[c][key]; ok {
			return c, k, true
		}
	}
	return "", "", false
}

// uniqueExpr: a single-column key: the row number in the column's type (and range).
func (t *table) uniqueExpr(c core.NamedType, idx string) (string, error) {
	b := t.bounds[c.Name]
	switch {
	case t.regex[c.Name] != "":
		return regexSQL(t.regex[c.Name], idx, t.n+1)
	case c.Type.Kind == core.Text && t.pattern[c.Name] != "":
		return patternSQL(t.pattern[c.Name], true, 0, idx), nil
	}
	return valueSQL(c.Type.Kind, 0, true, idx, b, t.n)
}

// compositeExpr: a column of a composite key. Without references: the row number in mixed radix (tuples unique).
// With a reference among the key's columns (order_items (order_id, line_no)): the referencing columns = parent row
// (i-1)/4 + 1, the first other column = (i-1) % 4 — four children per parent, tuples unique.
func (t *table) compositeExpr(c core.NamedType, idx string, depth int) (string, error) {
	pk := t.composite[c.Name]
	members := t.members[c.Name]
	// a reference keeps the key unique only if distinct parent rows give distinct values (a FOREIGN KEY's columns
	// together, or a single-column parent key); a join to a non-key column is ignored inside a key
	keyRef := func(m string) (ref, bool) {
		r, ok := t.refs[m]
		return r, ok && r.parent != t && (r.hard || r.parent.unique[r.slot])
	}
	var refd, others []string
	for _, m := range members {
		if _, ok := keyRef(m); ok {
			refd = append(refd, m)
		} else {
			others = append(others, m)
		}
	}
	r, isRef := keyRef(c.Name)
	switch {
	case len(refd) == 0:
		return keyValue(c.Type.Kind, mixedRadix("("+idx+" - 1)", pk, t.n), t.bounds[c.Name], t.n)
	case len(others) == 0: // a link table: every column a reference, the row number in mixed radix picks the parents
		return t.refExpr(r, c.Type.Kind, "("+mixedRadix("("+idx+" - 1)", pk, t.n)+" + 1)", depth)
	case isRef:
		return t.refExpr(r, c.Type.Kind, fmt.Sprintf("(((%s - 1) / 4) %% %d + 1)", idx, r.parent.n), depth)
	case c.Name == others[0]:
		return keyValue(c.Type.Kind, fmt.Sprintf("((%s - 1) %% 4)", idx), t.bounds[c.Name], t.n)
	}
	return keyValue(c.Type.Kind, "0", t.bounds[c.Name], t.n)
}

// keyValue: a key column's value from a 0-based number (distinct numbers → distinct values).
func keyValue(k core.Kind, base string, b *bound, n int) (string, error) {
	switch k {
	case core.Text:
		e := "'k' || " + base
		if b != nil && b.maxLen > 0 && b.maxLen < len(fmt.Sprint(n))+1 {
			return "", fmt.Errorf("key values need %d characters, CHECK allows %d", len(fmt.Sprint(n))+1, b.maxLen)
		}
		if b != nil && b.minLen > 0 {
			e = fmt.Sprintf("rpad(%[1]s, greatest(length(%[1]s), %[2]d), 'x')", e, b.minLen)
		}
		return e, nil
	case core.Date:
		return "date '2024-01-01' + (" + base + ")::int", boundsUnsupported(b, k)
	case core.Timestamp:
		return "timestamptz '2024-01-01 00:00:00+00' + make_interval(mins => (" + base + ")::int)", boundsUnsupported(b, k)
	case core.Decimal, core.Int32, core.Int64:
		step := 1.0
		lo, hi, hasLo, hasHi := b.numRange(step)
		if !hasLo {
			lo = 1
			if k == core.Decimal {
				lo = 0
			}
		}
		if hasHi && lo+float64(n) > hi+1 {
			return "", fmt.Errorf("CHECK range [%v, %v] is too small for %d distinct keys", lo, hi, n)
		}
		e := fmt.Sprintf("(%s + %s)", fmtNum(lo), base)
		if k == core.Decimal {
			e += "::numeric"
		}
		return e, nil
	case core.Bool:
		return "(" + base + ") % 2 = 0", nil
	}
	return "", fmt.Errorf("kind %s cannot be a key", k)
}

func boundsUnsupported(b *bound, k core.Kind) error { // k: for the message
	if b != nil && (b.lo != nil || b.hi != nil || b.minLen > 0 || b.maxLen > 0) {
		return fmt.Errorf("CHECK range on a %s column is not supported by the data generator", k)
	}
	return nil
}

func fmtNum(f float64) string {
	if f == math.Trunc(f) {
		return fmt.Sprintf("%.0f", f)
	}
	return fmt.Sprint(f)
}

// valueSQL: a value of kind k for row idx. salt decorrelates columns; unique = the row number; b = CHECK bounds.
func valueSQL(k core.Kind, salt int, unique bool, idx string, b *bound, n int) (string, error) {
	if unique {
		return keyValue(k, "(("+idx+")::int8 - 1)", b, n)
	}
	h := hashSQL(idx, salt, 1000) // 0..999
	switch k {
	case core.Int32, core.Int64:
		lo, hi, hasLo, hasHi := b.numRange(1)
		switch {
		case hasLo && hasHi:
			span := hi - lo + 1
			if span <= 0 {
				return "", fmt.Errorf("CHECK range [%v, %v] is empty", lo, hi)
			}
			return fmt.Sprintf("(%s + %s %% %s)", fmtNum(lo), h, fmtNum(span)), nil
		case hasLo:
			return fmt.Sprintf("(%s + %s)", fmtNum(lo), h), nil
		case hasHi:
			return fmt.Sprintf("(%s - %s)", fmtNum(hi), h), nil
		}
		return "(" + h + " + 1)", nil
	case core.Decimal:
		lo, hi, hasLo, hasHi := b.numRange(0.25)
		switch {
		case hasLo && hasHi:
			steps := math.Floor((hi-lo)/1.25) + 1
			if hi < lo {
				return "", fmt.Errorf("CHECK range [%v, %v] is empty", lo, hi)
			}
			return fmt.Sprintf("(%s + (%s %% %s) * 1.25)::numeric", fmtNum(lo), h, fmtNum(steps)), nil
		case hasLo:
			return fmt.Sprintf("(%s + %s * 1.25)::numeric", fmtNum(lo), h), nil
		case hasHi:
			return fmt.Sprintf("(%s - %s * 1.25)::numeric", fmtNum(hi), h), nil
		}
		return "(" + h + " * 1.25)::numeric", nil
	case core.Text:
		e := "'v' || " + hashSQL(idx, salt+1, 5000)
		if b != nil && (b.lo != nil || b.hi != nil) {
			return "", fmt.Errorf("CHECK numeric range on a text column is not supported by the data generator")
		}
		if b != nil && b.maxLen > 0 {
			e = fmt.Sprintf("left(%s, %d)", e, b.maxLen)
		}
		if b != nil && b.minLen > 0 {
			e = fmt.Sprintf("rpad(%[1]s, greatest(length(%[1]s), %[2]d), 'x')", e, b.minLen)
		}
		return e, nil
	case core.Bool:
		return fmt.Sprintf("((%s) + %d) %% 2 = 0", idx, salt), nil
	case core.Date:
		return "date '2024-01-01' + " + h + "::int", boundsUnsupported(b, k)
	case core.Timestamp:
		return fmt.Sprintf("timestamptz '2024-01-01 00:00:00+00' + make_interval(mins => (%s)::int)", idx), boundsUnsupported(b, k)
	}
	return "NULL", nil
}

// jsonExpr: a JSON column: jsonb_build_object over the keys the storage declares (+ the logical delete key: 1%).
func (t *table) jsonExpr(col, idx string, depth int) (string, error) {
	keys := t.json[col]
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	var parts []string
	for _, key := range names {
		k := keys[key]
		v, err := t.keyExpr(col, key, k, idx, depth)
		if err != nil {
			return "", fmt.Errorf("JSON key %s: %w", key, err)
		}
		if nk := t.nkKeys[key]; nk[1] > 0 && k == core.Text && t.pattern[key] != "" {
			base := mixedRadix("(("+idx+") / 5)", nk, t.n/5)
			v = "(" + base + ")::text" // the natural key's number without the prefix
			if t.pattern[key] == "zip" {
				v = "lpad(((" + base + ") / 10000 % 1000)::text, 3, '0') || '-' || lpad(((" + base + ") % 10000)::text, 4, '0')"
			}
		}
		parts = append(parts, quote(key), jsonRepr(k, v))
	}
	dels := make([]string, 0, len(t.delKey))
	for k := range t.delKey {
		dels = append(dels, k)
	}
	sort.Strings(dels)
	for _, k := range dels { // logical delete: 1%
		parts = append(parts, quote(k), fmt.Sprintf("CASE WHEN (%s) %% 100 = 0 THEN %s ELSE '0' END", idx, quote(t.delKey[k])))
	}
	return "jsonb_build_object(" + strings.Join(parts, ", ") + ")", nil
}

// keyExpr: the typed value of a JSON key for row idx.
func (t *table) keyExpr(col, key string, k core.Kind, idx string, depth int) (string, error) {
	salt := saltOf(t.name + "." + col + "." + key)
	if en, ok := t.enums[key]; ok {
		return enumSQL(en, salt, idx), nil
	}
	if nk := t.nkKeys[key]; nk[1] > 0 { // natural key: (row / 5) in mixed radix over the key's columns: ~5 versions per key
		base := mixedRadix("(("+idx+") / 5)", nk, t.n/5)
		switch k {
		case core.Text:
			return "'k' || " + base, nil
		case core.Date:
			return "date '2024-01-01' + (" + base + ")::int", nil
		case core.Timestamp:
			return "timestamptz '2024-01-01 00:00:00+00' + make_interval(mins => (" + base + ")::int)", nil
		case core.Decimal:
			return base + "::numeric", nil
		case core.Bool:
			return base + " % 2 = 0", nil
		}
		return base + " + 1", nil
	}
	if r, ok := t.refs[key]; ok && r.parent != t {
		return t.refExpr(r, k, "(" + hashSQL(idx, saltOf(r.group), r.parent.n) + " + 1)", depth)
	}
	if p := t.pattern[key]; p != "" && k == core.Text {
		return patternSQL(p, false, salt, idx), nil
	}
	if t.ymd[key] {
		return ymdSQL(salt, idx), nil
	}
	return valueSQL(k, salt, false, idx, nil, t.n)
}

// hashSQL: a pseudo-random number in [0, mod) for row idx (hashint8: no correlation with the row number's structure —
// a multiplicative hash made every 1000th row equal, and the latest version of a key always had the same date).
func hashSQL(idx string, salt, mod int) string {
	return fmt.Sprintf("(abs(hashint8((%s)::int8 * 1000 + %d)::int8) %% %d)", idx, salt, mod)
}

// ymdSQL: a valid date as a YYYYMMDD integer (dates spread like valueSQL's).
func ymdSQL(salt int, idx string) string {
	return "to_char(date '2024-01-01' + " + hashSQL(idx, salt, 1000) + "::int, 'YYYYMMDD')::int4"
}

// jsonRepr: a typed value as JSON (json_field's casts: dates / decimals / timestamps are strings).
func jsonRepr(k core.Kind, v string) string {
	switch k {
	case core.Date:
		return "to_char(" + v + ", 'YYYY-MM-DD')"
	case core.Decimal:
		return "(" + v + ")::text"
	case core.Timestamp:
		return "to_char((" + v + ") AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')"
	}
	return v
}

// enumSQL: the legal values in turn.
func enumSQL(vals []int64, salt int, idx string) string {
	var xs []string
	for _, v := range vals {
		xs = append(xs, fmt.Sprint(v))
	}
	return fmt.Sprintf("(ARRAY[%s])[1 + ((%s) + %d) %% %d]", strings.Join(xs, ","), idx, salt, len(vals))
}

// mixedRadix: idx (0..total) in mixed radix, digit pos of cnt (the last digit is not reduced). Distinct idx → distinct tuples.
func mixedRadix(idx string, pk [2]int, total int) string {
	pos, cnt := pk[0], pk[1]
	radix := max(2, int(math.Ceil(math.Pow(float64(total), 1/float64(cnt)))))
	div := 1
	for j := 0; j < pos; j++ {
		div *= radix
	}
	if pos == cnt-1 {
		return fmt.Sprintf("(%s / %d)", idx, div)
	}
	return fmt.Sprintf("((%s / %d) %% %d)", idx, div, radix)
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
