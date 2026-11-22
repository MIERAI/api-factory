package where

import (
	"encoding/json"
	"strings"
	"testing"

	"apifactory/factory/core"
)

func types(name string) (core.Type, bool) {
	t, ok := map[string]core.Type{
		"a": {Kind: core.Int32, Nullable: true}, "b": {Kind: core.Int32}, "s": {Kind: core.Text},
		"d": {Kind: core.Date, Nullable: true}, "m": {Kind: core.Decimal},
	}[name]
	return t, ok
}

func TestParse(t *testing.T) {
	ok := map[string]string{
		"a <> b 或者 b = 99999999":               `{"op":"or","args":[{"op":"cmp","name":"\u003c\u003e","args":[{"op":"col","name":"a"},{"op":"col","name":"b"}]},{"op":"cmp","name":"=","args":[{"op":"col","name":"b"},{"op":"const","value":99999999,"type":{"kind":"int32"}}]}]}`,
		"不是 (a = b 并且 b <> 1)":                 `{"op":"not","args":[{"op":"and","args":[{"op":"cmp","name":"=","args":[{"op":"col","name":"a"},{"op":"col","name":"b"}]},{"op":"cmp","name":"\u003c\u003e","args":[{"op":"col","name":"b"},{"op":"const","value":1,"type":{"kind":"int32"}}]}]}]}`,
		"s = 'x' 并且 d >= '2026-01-01' 或者 a 为空": "",
		"1 < b":   "",
		"m > 1.5": "",
		"（a 不为空）": "",
		"s <> ''": "",
		"a=b":     "",
	}
	for src, want := range ok {
		e, err := Parse(src, types)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		if want != "" {
			b, _ := json.Marshal(e)
			if string(b) != want {
				t.Errorf("%s:\n got %s\nwant %s", src, b, want)
			}
		}
	}
	bad := map[string]string{
		"x = 1":         "至少一边要是列",
		"1 = 2":         "至少一边要是列",
		"s = x":         "单引号",
		"b = 'x'":       "不是整数",
		"a ~ b":         "需要 比较",
		"a => b":        "无法识别",
		"a = b 或者":      "不完整",
		"(a = b":        "括号",
		"a = b c":       "多余",
		"d = '2026/01'": "日期",
		"x 为空":          "不是列",
		"s = 'abc":      "引号",
	}
	for src, want := range bad {
		_, err := Parse(src, types)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", src, err, want)
		}
	}
}

// 今天 / 现在: 只有 Options.Time 时能写;列的类型要对得上(日期 / 整数编码的日期 ↔ 今天,时间 ↔ 现在)。
func TestParseTime(t *testing.T) {
	ty := func(name string) (core.Type, bool) {
		t, ok := map[string]core.Type{"vf": {Kind: core.Date}, "vt": {Kind: core.Date, Nullable: true}, "at": {Kind: core.Timestamp},
			"ymd": {Kind: core.Int32}, "n": {Kind: core.Int32}}[name]
		return t, ok
	}
	opt := Options{Time: true, Zone: "Asia/Tokyo", YMD: func(c string) bool { return c == "ymd" }}
	for src, want := range map[string]string{
		"vf <= 今天 并且 (vt 为空 或者 vt >= 今天)": "and(cmp(<=, vf, local_date(now()) [Asia/Tokyo]), or(is_null(vt), cmp(>=, vt, local_date(now()) [Asia/Tokyo])))",
		"今天 < vf":   "cmp(<, local_date(now()) [Asia/Tokyo], vf)",
		"at < 现在":   "cmp(<, at, now())",
		"ymd <= 今天": "cmp(<=, ymd(ymd), local_date(now()) [Asia/Tokyo])",
	} {
		e, err := ParseOpt(src, ty, opt)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		if got := show(e); got != want {
			t.Errorf("%s: %s,期待 %s", src, got, want)
		}
	}
	for src, msg := range map[string]string{"vf <= 现在": "现在 只能和时间", "at < 今天": "今天 只能和日期", "n = 今天": "今天 只能和日期", "今天 = 现在": "要和列比较"} {
		if _, err := ParseOpt(src, ty, opt); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s: 应该报错 %q,实际 %v", src, msg, err)
		}
	}
	if _, err := Parse("vf <= 今天", ty); err == nil || !strings.Contains(err.Error(), "不能写 今天") {
		t.Errorf("没有 Options.Time 时不能写 今天: %v", err)
	}
}

func show(e *core.Expr) string {
	switch e.Op {
	case "col":
		return e.Name
	case "now":
		return "now()"
	}
	var args []string
	for _, a := range e.Args {
		args = append(args, show(a))
	}
	if e.Op == "cmp" {
		args = append([]string{e.Name}, args...)
	}
	s := e.Op + "(" + strings.Join(args, ", ") + ")"
	if e.Zone != "" {
		s += " [" + e.Zone + "]"
	}
	return s
}
