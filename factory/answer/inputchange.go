package answer

// 回答直接改设计稿的输入定义(P5)。
//
// 声明式 API 的补充规则只是文字,不会被执行。"商品ID 改成必填""最多 1000 个""区分コード ≥ 1"这类回答,
// 只写成规则的话,下一轮审查会(正确地)指出"输入定义里没有,规则没有生效",一直挡流水线(实测)。
// 所以选项可以带结构化的 input_change,选中后工厂直接改设计稿里那一行输入。

import (
	"apifactory/factory/constraints"
	"fmt"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// InputChange: 改一个输入的一个键。
type InputChange struct {
	Input string `json:"input"` // 输入的名字
	Key   string `json:"key"`   // 必填 / 范围 / 长度 / 个数
	Value string `json:"value"` // 是 / "> 0" / "1~64" / "1~1000"
}

// Valid: 只允许 必填 和约束的注册表(factory/constraints)里的键和写法(其他的修改要人改设计稿)。
func (c InputChange) Valid() error {
	if c.Key == "必填" {
		if c.Value != "是" && c.Value != "否" {
			return fmt.Errorf("输入 %s: 必填 只能是 是 / 否", c.Input)
		}
		return nil
	}
	if !slices.Contains(constraints.Keys(), c.Key) {
		return fmt.Errorf("输入 %s: 不能自动修改的键 %q(只能是 必填 / %s)", c.Input, c.Key, strings.Join(constraints.Keys(), " / "))
	}
	if !constraints.ValueOK(c.Key, c.Value) {
		return fmt.Errorf("输入 %s: %s 的值 %q 不对", c.Input, c.Key, c.Value)
	}
	return nil
}

func (c InputChange) String() string {
	return fmt.Sprintf("输入 %s 的 %s → %s", c.Input, c.Key, c.Value)
}

// ApplyInputChanges: 改设计稿。输入的定义是一行的 { 键: 值, ... },只改那一行(保留缩进和行尾注释)。
func ApplyInputChanges(designFile string, cs []InputChange) error {
	if len(cs) == 0 {
		return nil
	}
	b, err := os.ReadFile(designFile)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return err
	}
	if len(doc.Content) == 0 {
		return fmt.Errorf("%s: 空文件", designFile)
	}
	var inputs *yaml.Node
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "输入" {
			inputs = root.Content[i+1]
		}
	}
	if inputs == nil || inputs.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: 没有 输入", designFile)
	}
	lines := strings.Split(string(b), "\n")
	for _, c := range cs {
		if err := c.Valid(); err != nil {
			return err
		}
		var val *yaml.Node
		for i := 0; i+1 < len(inputs.Content); i += 2 {
			if inputs.Content[i].Value == c.Input {
				val = inputs.Content[i+1]
			}
		}
		if val == nil {
			return fmt.Errorf("%s: 没有输入 %s", designFile, c.Input)
		}
		if val.Kind != yaml.MappingNode || val.Style&yaml.FlowStyle == 0 || val.Line < 1 || val.Line > len(lines) {
			return fmt.Errorf("%s: 输入 %s 不是一行的 { … },请手动修改: %s", designFile, c.Input, c)
		}
		set := false
		for i := 0; i+1 < len(val.Content); i += 2 {
			if val.Content[i].Value == c.Key {
				val.Content[i+1].Value, val.Content[i+1].Style, set = c.Value, yaml.DoubleQuotedStyle, true
			}
		}
		if !set { // 新的键放在 类型 / 必填 之后
			at := 0
			for i := 0; i+1 < len(val.Content); i += 2 {
				if k := val.Content[i].Value; k == "类型" || k == "必填" {
					at = i + 2
				}
			}
			kv := []*yaml.Node{
				{Kind: yaml.ScalarNode, Value: c.Key},
				{Kind: yaml.ScalarNode, Value: c.Value, Style: yaml.DoubleQuotedStyle},
			}
			val.Content = append(val.Content[:at], append(kv, val.Content[at:]...)...)
		}
		line := lines[val.Line-1]
		open, close := strings.Index(line, "{"), strings.LastIndex(line, "}")
		if open < 0 || close < open {
			return fmt.Errorf("%s:%d: 找不到 { … }", designFile, val.Line)
		}
		lines[val.Line-1] = line[:open] + flowMap(val) + line[close+1:]
	}
	out := strings.Join(lines, "\n")
	var check yaml.Node
	if err := yaml.Unmarshal([]byte(out), &check); err != nil {
		return fmt.Errorf("修改后的设计稿不是合法的 YAML(没有写入): %w", err)
	}
	return os.WriteFile(designFile, []byte(out), 0o644)
}

func flowMap(n *yaml.Node) string {
	var parts []string
	for i := 0; i+1 < len(n.Content); i += 2 {
		v := n.Content[i+1]
		s := v.Value
		switch {
		case v.Style&yaml.DoubleQuotedStyle != 0:
			s = fmt.Sprintf("%q", s)
		case v.Style&yaml.SingleQuotedStyle != 0:
			s = "'" + strings.ReplaceAll(s, "'", "''") + "'"
		}
		parts = append(parts, n.Content[i].Value+": "+s)
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// ApplyRuleAdditions: 往设计稿的"输入之间的规则"追加规则(没有这一节就加在 输入 的后面)。
func ApplyRuleAdditions(designFile string, rules []string) error {
	if len(rules) == 0 {
		return nil
	}
	for _, r := range rules {
		if !constraints.MatchRelation(r) {
			return fmt.Errorf("输入之间的规则 %q: 不是工厂支持的写法(%s)", r, strings.Join(constraints.RelationForms(), " / "))
		}
	}
	b, err := os.ReadFile(designFile)
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	var add []string
	for _, r := range rules {
		add = append(add, "  - "+r)
	}
	at := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "输入之间的规则:") {
			at = i + 1
			for at < len(lines) && strings.HasPrefix(lines[at], "  ") {
				at++
			}
			break
		}
	}
	if at < 0 { // 新加一节: 放在 输入 这一节的后面
		for i, l := range lines {
			if strings.HasPrefix(l, "输入:") {
				at = i + 1
				for at < len(lines) && (strings.HasPrefix(lines[at], "  ") || strings.TrimSpace(lines[at]) == "") {
					at++
				}
				add = append(append([]string{"输入之间的规则:"}, add...), "")
				break
			}
		}
	}
	if at < 0 {
		return fmt.Errorf("%s: 没有 输入", designFile)
	}
	// 去掉要插入位置前面多余的空行(新的一节和上一节之间留一个空行)
	out := append(append(append([]string{}, lines[:at]...), add...), lines[at:]...)
	s := strings.Join(out, "\n")
	var check yaml.Node
	if err := yaml.Unmarshal([]byte(s), &check); err != nil {
		return fmt.Errorf("修改后的设计稿不是合法的 YAML(没有写入): %w", err)
	}
	return os.WriteFile(designFile, []byte(s), 0o644)
}
