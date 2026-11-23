package perf

// CHECK constraints the data generator understands (pg_get_constraintdef's normalized text). Each conjunct of a
// constraint must be one of these shapes, or loading the schema fails (a constraint the generator would only satisfy
// by chance is a bug that shows up as "perf is broken" months later — 2026-11: discount > 0 after R2).
//
//	col = ANY (ARRAY[…]) / col = literal        a list of values (literals: taken in turn)
//	col  >|>=|<|<=  number                       a numeric range (bounds)
//	length(col) / char_length(col)  op  number   a text length range
//	col <> ''                                    not empty (generated text never is)
//	col ~ 'regex'                                values generated from the regex (regex.go: classes, literals, {m,n})
//	isfinite(col), col IS NULL OR …              no constraint on generated values
//
// Domains: CHECK (VALUE …) of the column's domain, the same shapes with VALUE = the column.

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// bound: what the CHECK constraints allow for a column (nil = no bound).
type bound struct {
	lo, hi             *float64
	loStrict, hiStrict bool
	minLen, maxLen     int // 0 = no bound
}

var (
	cmpRe    = regexp.MustCompile(`^(\w+|(?:char_)?length\((\w+)\)) (>=|<=|>|<|<>|=) (.+)$`)
	numRe    = regexp.MustCompile(`^\(?(-?\d+(?:\.\d+)?)\)?(?:::[\w ]+)?$`)
	emptyRe  = regexp.MustCompile(`^''(?:::[\w ]+)?$`)
	regexRe  = regexp.MustCompile(`^(\w+) ~ '((?:[^']|'')*)'(?:::text)?$`)
	finiteRe = regexp.MustCompile(`^isfinite\((\w+)\)$`)
	nullRe   = regexp.MustCompile(`^(\w+) IS (?:NOT )?NULL$`)
	castRe   = regexp.MustCompile(`\((\w+)\)::(?:character varying|double precision|\w+)`)
)

// parseCheck: one CHECK constraint (def = pg_get_constraintdef) on table t. Columns it constrains are marked checked.
func (t *table) parseCheck(def string) error {
	body := strings.TrimPrefix(def, "CHECK ")
	if strings.HasSuffix(body, " NOT VALID") {
		body = strings.TrimSuffix(body, " NOT VALID")
	}
	body = castRe.ReplaceAllString(body, "$1") // (VALUE)::text = … / (col)::text → the column
	for _, conj := range splitTop(stripParens(body), " AND ") {
		if err := t.parseAtom(stripParens(conj), def); err != nil {
			return err
		}
	}
	return nil
}

func (t *table) parseAtom(a, def string) error {
	if ors := splitTop(a, " OR "); len(ors) > 1 { // col IS NULL OR …: generated values are never NULL unless the column is nullable
		var rest []string
		for _, o := range ors {
			if !nullRe.MatchString(stripParens(o)) {
				rest = append(rest, o)
			}
		}
		if len(rest) == 1 {
			return t.parseAtom(stripParens(rest[0]), def)
		}
		return fmt.Errorf("CHECK not understood by the data generator: %s (teach factory/perf/checks.go this shape)", def)
	}
	if m := finiteRe.FindStringSubmatch(a); m != nil {
		t.checked[m[1]] = true
		return nil
	}
	if m := nullRe.FindStringSubmatch(a); m != nil {
		t.checked[m[1]] = true
		return nil
	}
	if strings.Contains(a, " = ANY ") || (strings.Contains(a, " = ") && !strings.Contains(a, "length(")) {
		col := strings.Fields(a)[0]
		if vals := checkValues(col, "CHECK (("+a+"))"); len(vals) > 0 {
			t.checked[col] = true
			t.literals[col] = vals
			return nil
		}
	}
	if m := regexRe.FindStringSubmatch(a); m != nil {
		re := strings.ReplaceAll(m[2], "''", "'")
		if _, err := parseRegex(re); err != nil {
			return fmt.Errorf("CHECK %s: %w (teach factory/perf/regex.go)", def, err)
		}
		t.checked[m[1]] = true
		t.regex[m[1]] = re
		return nil
	}
	if m := cmpRe.FindStringSubmatch(a); m != nil {
		col, isLen, op, rhs := m[1], m[2] != "", m[3], m[4]
		if isLen {
			col = m[2]
		}
		b := t.bounds[col]
		if b == nil {
			b = &bound{}
			t.bounds[col] = b
		}
		t.checked[col] = true
		if op == "<>" && emptyRe.MatchString(rhs) && !isLen {
			return nil // generated text is never empty (dirty data: only NULL for checked columns)
		}
		nm := numRe.FindStringSubmatch(rhs)
		if nm == nil || op == "<>" || op == "=" {
			return fmt.Errorf("CHECK not understood by the data generator: %s (teach factory/perf/checks.go this shape)", def)
		}
		v, _ := strconv.ParseFloat(nm[1], 64)
		if isLen {
			n := int(v)
			switch op {
			case ">":
				b.minLen = max(b.minLen, n+1)
			case ">=":
				b.minLen = max(b.minLen, n)
			case "<":
				b.maxLen = minPos(b.maxLen, n-1)
			case "<=":
				b.maxLen = minPos(b.maxLen, n)
			}
			return nil
		}
		switch op {
		case ">", ">=":
			if b.lo == nil || v > *b.lo || (v == *b.lo && op == ">") {
				b.lo, b.loStrict = &v, op == ">"
			}
		case "<", "<=":
			if b.hi == nil || v < *b.hi || (v == *b.hi && op == "<") {
				b.hi, b.hiStrict = &v, op == "<"
			}
		}
		return nil
	}
	return fmt.Errorf("CHECK not understood by the data generator: %s (teach factory/perf/checks.go this shape)", def)
}

func minPos(a, b int) int {
	if a == 0 {
		return b
	}
	return min(a, b)
}

// stripParens: remove parentheses that enclose the whole expression.
func stripParens(s string) string {
	s = strings.TrimSpace(s)
	for len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' && closing(s, 0) == len(s)-1 {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// closing: the index of the parenthesis closing s[open] (quotes respected).
func closing(s string, open int) int {
	depth, quoted := 0, false
	for i := open; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// splitTop: split at sep outside parentheses and quotes.
func splitTop(s, sep string) []string {
	var out []string
	depth, quoted, last := 0, false, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0 && strings.HasPrefix(s[i:], sep):
			out = append(out, s[last:i])
			last = i + len(sep)
			i += len(sep) - 1
		}
	}
	return append(out, s[last:])
}

// numRange: the integer / decimal interval [lo, hi] a bound allows (step: the smallest difference used for strict bounds).
func (b *bound) numRange(step float64) (lo, hi float64, hasLo, hasHi bool) {
	if b == nil {
		return 0, 0, false, false
	}
	if b.lo != nil {
		lo, hasLo = *b.lo, true
		if b.loStrict {
			lo += step
		}
		if step == 1 {
			lo = math.Ceil(lo)
		}
	}
	if b.hi != nil {
		hi, hasHi = *b.hi, true
		if b.hiStrict {
			hi -= step
		}
		if step == 1 {
			hi = math.Floor(hi)
		}
	}
	return
}
