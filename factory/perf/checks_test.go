package perf

import (
	"strings"
	"testing"
)

func TestParseCheck(t *testing.T) {
	for _, c := range []struct {
		def, col string
		lo, hi   string // "" = none
		minLen   int
		maxLen   int
		regex    string
		literals int
	}{
		{def: "CHECK ((weight > (0)::numeric))", col: "weight", lo: ">0"},
		{def: "CHECK ((amount >= (0)::numeric))", col: "amount", lo: ">=0"},
		{def: "CHECK (((revision >= 1) AND (revision <= 1000)))", col: "revision", lo: ">=1", hi: "<=1000"},
		{def: "CHECK (((length(title) >= 1) AND (length(title) <= 100)))", col: "title", minLen: 1, maxLen: 100},
		{def: "CHECK ((char_length(sku) <= 32))", col: "sku", maxLen: 32},
		{def: "CHECK ((account_id ~ '^[A-Z0-9-]{3,32}$'::text))", col: "account_id", regex: "^[A-Z0-9-]{3,32}$"},
		{def: "CHECK ((category <> ''::text))", col: "category"},
		{def: "CHECK (isfinite(created_at))", col: "created_at"},
		{def: "CHECK (((finished_at IS NULL) OR isfinite(finished_at)))", col: "finished_at"},
		{def: "CHECK ((status = ANY (ARRAY[1, 2, 3])))", col: "status", literals: 3},
		{def: "CHECK (((v)::text = ANY ((ARRAY['YES'::character varying, 'NO'::character varying])::text[])))", col: "v", literals: 2},
	} {
		tb := newTable("t", nil)
		if err := tb.parseCheck(c.def); err != nil {
			t.Errorf("%s: %v", c.def, err)
			continue
		}
		if !tb.checked[c.col] {
			t.Errorf("%s: %s not marked checked", c.def, c.col)
		}
		b := tb.bounds[c.col]
		bstr := func(v *float64, strict bool, a, s string) string {
			if v == nil {
				return ""
			}
			op := a
			if strict {
				op = s
			}
			return op + fmtNum(*v)
		}
		if b != nil {
			if got := bstr(b.lo, b.loStrict, ">=", ">"); got != c.lo {
				t.Errorf("%s: lo %q want %q", c.def, got, c.lo)
			}
			if got := bstr(b.hi, b.hiStrict, "<=", "<"); got != c.hi {
				t.Errorf("%s: hi %q want %q", c.def, got, c.hi)
			}
			if b.minLen != c.minLen || b.maxLen != c.maxLen {
				t.Errorf("%s: len [%d, %d] want [%d, %d]", c.def, b.minLen, b.maxLen, c.minLen, c.maxLen)
			}
		} else if c.lo != "" || c.hi != "" || c.minLen != 0 || c.maxLen != 0 {
			t.Errorf("%s: no bounds", c.def)
		}
		if tb.regex[c.col] != c.regex {
			t.Errorf("%s: regex %q want %q", c.def, tb.regex[c.col], c.regex)
		}
		if len(tb.literals[c.col]) != c.literals {
			t.Errorf("%s: literals %v want %d", c.def, tb.literals[c.col], c.literals)
		}
	}
}

// Shapes the generator does not understand are errors, never skipped.
func TestParseCheckUnknown(t *testing.T) {
	for _, def := range []string{
		"CHECK ((valid_to > valid_from))",
		"CHECK ((lower(code) = code))",
		"CHECK (((a > 0) OR (b > 0)))",
		"CHECK ((code ~ '^(a|b)$'::text))",
	} {
		if err := newTable("t", nil).parseCheck(def); err == nil || !strings.Contains(err.Error(), "not") {
			t.Errorf("%s: want an error, got %v", def, err)
		}
	}
}

func TestRegexSQL(t *testing.T) {
	atoms, err := parseRegex(`^[A-Z]{2}[0-9]{6}$`)
	if err != nil || len(atoms) != 2 || len(atoms[0].chars) != 26 || atoms[1].min != 6 {
		t.Fatalf("parse: %v %+v", err, atoms)
	}
	if _, err := regexSQL(`^[AB]$`, "i", 3); err == nil {
		t.Error("2 values cannot hold 3 distinct keys")
	}
	if _, err := regexSQL(`^[A-Z0-9-]{3,32}$`, "i", 200001); err != nil {
		t.Error(err)
	}
	for _, re := range []string{`[a-z]+`, `^\s+$`, `^.+$`, `^[^a]$`} {
		if _, err := parseRegex(re); err == nil {
			t.Errorf("%s: want an error", re)
		}
	}
}
