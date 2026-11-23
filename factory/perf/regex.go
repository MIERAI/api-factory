package perf

// Values from a CHECK's regex (col ~ '^…$'): the anchored concatenation of atoms — a literal character, an escape
// (\d, \w, \. …) or a class [A-Z0-9-] — each with an optional quantifier {m} {m,n} ? + *. Anything else (alternation,
// groups, unanchored patterns) is an error: the generator would not know whether its values match.
//
// A value is the number v written in the mixed radix of the atoms' alphabets (the last character varies fastest), so
// distinct v give distinct values: unique columns use the row number.

import (
	"fmt"
	"strings"
)

type reAtom struct {
	chars    string // the alphabet
	min, max int    // repetitions (max -1 = unbounded)
}

func parseRegex(re string) ([]reAtom, error) {
	if !strings.HasPrefix(re, "^") || !strings.HasSuffix(re, "$") || strings.HasSuffix(re, `\$`) {
		return nil, fmt.Errorf("regex %q: only anchored patterns (^…$) are supported", re)
	}
	s := []rune(re[1 : len(re)-1])
	var out []reAtom
	for i := 0; i < len(s); {
		var a reAtom
		switch c := s[i]; c {
		case '[':
			j := i + 1
			if j < len(s) && s[j] == '^' {
				return nil, fmt.Errorf("regex %q: negated classes are not supported", re)
			}
			var b strings.Builder
			for ; j < len(s) && s[j] != ']'; j++ {
				if s[j] == '\\' && j+1 < len(s) {
					j++
					if cls := escapeClass(s[j]); cls != "" {
						b.WriteString(cls)
						continue
					}
					b.WriteRune(s[j])
					continue
				}
				if j+2 < len(s) && s[j+1] == '-' && s[j+2] != ']' {
					for r := s[j]; r <= s[j+2]; r++ {
						b.WriteRune(r)
					}
					j += 2
					continue
				}
				b.WriteRune(s[j])
			}
			if j >= len(s) {
				return nil, fmt.Errorf("regex %q: unclosed class", re)
			}
			a.chars, i = dedupe(b.String()), j+1
		case '\\':
			if i+1 >= len(s) {
				return nil, fmt.Errorf("regex %q: trailing backslash", re)
			}
			if strings.ContainsRune("sDWSbB", s[i+1]) {
				return nil, fmt.Errorf("regex %q: \\%c is not supported by the data generator", re, s[i+1])
			}
			a.chars = escapeClass(s[i+1])
			if a.chars == "" {
				a.chars = string(s[i+1])
			}
			i += 2
		case '(', ')', '|', '.', '^', '$', '{', '}', '*', '+', '?':
			return nil, fmt.Errorf("regex %q: %q is not supported by the data generator", re, c)
		default:
			a.chars, i = string(c), i+1
		}
		a.min, a.max = 1, 1
		if i < len(s) {
			switch s[i] {
			case '?':
				a.min, a.max, i = 0, 1, i+1
			case '*':
				a.min, a.max, i = 0, -1, i+1
			case '+':
				a.min, a.max, i = 1, -1, i+1
			case '{':
				j := i + 1
				for j < len(s) && s[j] != '}' {
					j++
				}
				if j >= len(s) {
					return nil, fmt.Errorf("regex %q: unclosed quantifier", re)
				}
				q := string(s[i+1 : j])
				var n int
				if lo, hi, ok := strings.Cut(q, ","); ok {
					if _, err := fmt.Sscanf(lo, "%d", &a.min); err != nil {
						return nil, fmt.Errorf("regex %q: quantifier {%s}", re, q)
					}
					a.max = -1
					if hi != "" {
						if _, err := fmt.Sscanf(hi, "%d", &a.max); err != nil {
							return nil, fmt.Errorf("regex %q: quantifier {%s}", re, q)
						}
					}
				} else if _, err := fmt.Sscanf(q, "%d", &n); err == nil {
					a.min, a.max = n, n
				} else {
					return nil, fmt.Errorf("regex %q: quantifier {%s}", re, q)
				}
				i = j + 1
			}
		}
		out = append(out, a)
	}
	return out, nil
}

func escapeClass(r rune) string {
	switch r {
	case 'd':
		return "0123456789"
	case 'w':
		return "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_"
	}
	return ""
}

func dedupe(s string) string {
	var b strings.Builder
	seen := map[rune]bool{}
	for _, r := range s {
		if !seen[r] {
			seen[r] = true
			b.WriteRune(r)
		}
	}
	return b.String()
}

// regexSQL: SQL for the value number v (an int8 expression) in the regex's language. need = how many distinct values
// are needed (the row count for unique columns); the variable atoms grow until there are enough.
func regexSQL(re string, v string, need int) (string, error) {
	atoms, err := parseRegex(re)
	if err != nil {
		return "", err
	}
	counts := make([]int, len(atoms))
	capacity := func() float64 {
		c := 1.0
		for i, a := range atoms {
			for range counts[i] {
				c *= float64(len([]rune(a.chars)))
			}
		}
		return c
	}
	for i, a := range atoms {
		counts[i] = a.min
	}
	for capacity() < float64(need) { // grow the first atom that can still grow (and has more than one character)
		grown := false
		for i, a := range atoms {
			if len([]rune(a.chars)) > 1 && (a.max < 0 || counts[i] < a.max) && counts[i] < a.min+16 {
				counts[i]++
				grown = true
				break
			}
		}
		if !grown {
			return "", fmt.Errorf("regex %q has only %.0f values, %d needed", re, capacity(), need)
		}
	}
	// positions from the last (fastest) to the first
	var parts []string
	div := 1.0
	for i := len(atoms) - 1; i >= 0; i-- {
		a := atoms[i]
		size := len([]rune(a.chars))
		for range counts[i] {
			var p string
			if size == 1 {
				p = quote(a.chars)
			} else {
				p = fmt.Sprintf("substr(%s, (1 + ((%s) / %.0f)::int8 %% %d)::int, 1)", quote(a.chars), v, div, size)
				div *= float64(size)
			}
			parts = append([]string{p}, parts...)
		}
	}
	if len(parts) == 0 {
		return "''", nil
	}
	return "(" + strings.Join(parts, " || ") + ")", nil
}
