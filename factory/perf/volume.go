package perf

import (
	"fmt"
	"regexp"
	"strings"

	"apifactory/factory/core"
)

// dirtySQL: 在正常值 e 上掺生产里常见的"脏"值(只用约束允许的值: 唯一 / 组合键 / 枚举 / 操作列不碰)。
// 比例按行号取模,不同的列用 salt 错开: 空值 5%(只有可为空的列)、各种边界值各 1~2%。
// checked: 列在 CHECK 约束里 → 只掺空值(边界值可能违反约束,约束本身就是数据的保证)。
func dirtySQL(k core.Kind, e string, nullable, checked bool, salt int) string {
	var cases []string
	if nullable {
		cases = append(cases, fmt.Sprintf("WHEN (i + %d) %% 20 = 0 THEN NULL", salt))
	}
	m := func(mod, rem int, v string) {
		cases = append(cases, fmt.Sprintf("WHEN (i + %d) %% %d = %d THEN %s", salt, mod, rem, v))
	}
	if checked {
		k = ""
	}
	switch k {
	case core.Text:
		m(50, 1, "''")                    // 空字符串
		m(50, 2, "'  ' || "+e+" || '  '") // 前后空格
		m(100, 3, "repeat('长', 300)")     // 超长
		m(100, 4, "'带''引号\"和\\\\反斜杠🙂'")   // 特殊字符
	case core.Int32, core.Int64:
		m(50, 1, "0")
		m(50, 2, "-("+e+")")
	case core.Decimal:
		m(50, 1, "0")
		m(50, 2, "-("+e+")")
		m(100, 3, "("+e+") + 0.005") // 舍入的边界
	case core.Date:
		m(100, 1, "date '1970-01-01'")
		m(100, 2, "date '2099-12-31'")
	case core.Timestamp:
		m(100, 1, "timestamptz '1970-01-01 00:00:00+00'")
	}
	if len(cases) == 0 {
		return e
	}
	return "CASE " + joinSpace(cases) + " ELSE " + e + " END"
}

func joinSpace(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += " "
		}
		out += x
	}
	return out
}

var (
	checkAnyRe = regexp.MustCompile(`^CHECK \(\(\(?(\w+)\)?(?:::\w+)? = ANY \(\(?ARRAY\[(.*)\]\)?(?:::\w+\[\])?\)\)\)$`)
	checkEqRe  = regexp.MustCompile(`^CHECK \(\((\w+) = ('[^']*'(?:::\w+)?|-?\d+)\)\)$`)
	literalRe  = regexp.MustCompile(`'((?:[^']|'')*)'(?:::[\w ]+)?|(-?\d+(?:\.\d+)?)`)
)

// checkValues: CHECK (col = ANY (ARRAY[...])) / CHECK (col = 'x') 的取值列表(SQL 字面量,原样用在 INSERT 里)。
func checkValues(col, def string) []string {
	var list string
	if m := checkAnyRe.FindStringSubmatch(def); m != nil && m[1] == col {
		list = m[2]
	} else if m := checkEqRe.FindStringSubmatch(def); m != nil && m[1] == col {
		list = m[2]
	} else {
		return nil
	}
	var out []string
	for _, m := range literalRe.FindAllStringSubmatch(list, -1) {
		if m[2] != "" {
			out = append(out, m[2])
		} else {
			out = append(out, quote(strings.ReplaceAll(m[1], "''", "'")))
		}
	}
	return out
}

// textPattern: the request field's string pattern as a value format the generator knows ("" = free text).
func textPattern(rules string) string {
	switch {
	case strings.Contains(rules, `^[0-9]+$`):
		return "digits"
	case strings.Contains(rules, `^\\d{3}-\\d{4}$`):
		return "zip"
	}
	return ""
}

// patternSQL: values in the request field's format (unique: the row number; otherwise ~5000 distinct values).
func patternSQL(p string, unique bool, salt int, idx string) string {
	n := hashSQL(idx, salt, 5000)
	if unique {
		n = "(" + idx + ")"
	}
	if p == "zip" {
		return "lpad((" + n + " / 10000 % 1000)::text, 3, '0') || '-' || lpad((" + n + " % 10000)::text, 4, '0')"
	}
	return "(" + n + ")::text"
}
