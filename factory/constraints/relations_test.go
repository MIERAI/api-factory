package constraints

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"apifactory/factory/ir"
)

var relFields = map[string]RelField{
	"from":   {Name: "from", Kind: "time"},
	"to":     {Name: "to", Kind: "time"},
	"start":  {Name: "start", Kind: "time", Required: true},
	"end":    {Name: "end", Kind: "time", Required: true},
	"cat":    {Name: "cat", Kind: "string", Optional: true},
	"codes":  {Name: "codes", Kind: "string", List: true},
	"req":    {Name: "req", Kind: "string", Required: true},
	"plain":  {Name: "plain", Kind: "int"}, // 不能区分"没有指定"
	"d1":     {Name: "d1", Kind: "date", Required: true},
	"d2":     {Name: "d2", Kind: "date", Optional: true},
	"amount": {Name: "amount", Kind: "int", Optional: true},
}

func lookup(n string) (RelField, error) {
	if f, ok := relFields[n]; ok {
		return f, nil
	}
	return RelField{}, fmt.Errorf("%s 不是输入", n)
}

func TestCompileRelation(t *testing.T) {
	for _, c := range []struct{ text, id, expr string }{
		{"from 早于 to", "before.from.to", "!has(this.from) || !has(this.to) || this.from < this.to"},
		{"start 早于 end", "before.start.end", "this.start < this.end"}, // 都必填: 不需要放行
		{"from 到 to 不超过 7 天", "within.from.to.7", "!has(this.from) || !has(this.to) || this.to - this.from <= duration('168h')"},
		{"d1 到 d2 不超过 62 天", "within.d1.d2.62", "!has(this.d2) || !this.d1.matches('^[0-9]{4}-[0-9]{2}-[0-9]{2}$') || !this.d2.matches('^[0-9]{4}-[0-9]{2}-[0-9]{2}$') || timestamp(this.d2 + 'T00:00:00Z') - timestamp(this.d1 + 'T00:00:00Z') <= duration('1488h')"},
		{"cat、codes 至少指定一个", "at_least_one.cat.codes", "has(this.cat) || size(this.codes) > 0"},
		{"cat 和 codes 和 amount 至少指定一个", "at_least_one.cat.codes.amount", "has(this.cat) || size(this.codes) > 0 || has(this.amount)"},
	} {
		id, x, err := CompileRelation(c.text, lookup)
		if err != nil || id != c.id || x.Expr != c.expr {
			t.Errorf("%s:\n got %s %q %v\nwant %s %q", c.text, id, x.Expr, err, c.id, c.expr)
		}
	}
	for _, c := range []struct{ text, err string }{
		{"from 等于 to", "无法识别"},
		{"from 早于 nope", "不是输入"},
		{"cat 到 to 不超过 3 天", "只能用于两个时间或两个日期"},
		{"d1 到 to 不超过 3 天", "只能用于两个时间或两个日期"},
		{"cat、req 至少指定一个", "必填"},
		{"cat、plain 至少指定一个", "不能区分"},
	} {
		if _, _, err := CompileRelation(c.text, lookup); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: 应该报错 %q,实际 %v", c.text, c.err, err)
		}
	}
}

// "合法"的用例不能去掉必填字段(去掉的话请求本来就不合法)。
func TestRelationCasesSkipsRequired(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"required": true})
	m := &ir.Message{
		Fields:    []ir.Field{{Name: "start", Kind: "timestamp", Rules: &ir.Rules{Required: true, Raw: raw}}, {Name: "end", Kind: "timestamp", Rules: &ir.Rules{Required: true, Raw: raw}}},
		Relations: []string{"before.start.end", "rule_1"},
	}
	cases, labels, unknown := RelationCases(m, map[string]any{"start": "2026-01-01T00:00:00Z", "end": "2026-01-02T00:00:00Z"})
	if unknown != 1 || len(labels) != 1 {
		t.Fatalf("unknown=%d labels=%v", unknown, labels)
	}
	for _, c := range cases {
		if c.Valid && len(c.Unset) > 0 {
			t.Errorf("去掉必填字段的合法用例: %+v", c)
		}
	}
	if len(cases) != 3 {
		t.Errorf("早于: 违反 2 + 合法 1,实际 %d", len(cases))
	}
}
