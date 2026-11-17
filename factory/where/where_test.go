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
