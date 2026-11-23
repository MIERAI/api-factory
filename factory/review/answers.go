package review

// 已经回答过的问题(X.审查.yaml / 领域.审查.yaml): 给审查员看(编号 A1… / D1…),也给收敛规则用。
//
// 以前"回答过没有"只按问题原文判断: CI 换了个说法问同一件事,本地的回答就不算(R1-F1 still unanswered)。
// 现在一个问题算"回答过"的条件(任一):
//   - 回答记录的 问题编号 里有它(factory answer 写入: <轮次 ID>#F2;轮次 ID 本地 / CI 都一样,见 rounds.go)
//   - 问题原文 = 记录的 问题 / 同一个问题(以前的记录只有这个)
//   - 位置 + 选项 和某条记录相同(同一个地方、同一组选择 = 同一个问题,换了说法也一样)
// 审查员的 same_as 写 A3 / D1(回答记录的编号)→ 编号存在才算(Gate 校验),这个问题丢弃(重复提问)。

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Answered: 一条回答记录。
type Answered struct {
	Ref      string   `json:"ref"` // A3(API 的记录)/ D1(领域的记录)
	Question string   `json:"question"`
	Also     []string `json:"also,omitempty"` // 同一个问题(之前轮次的说法)
	Location string   `json:"location,omitempty"`
	Options  []string `json:"options,omitempty"`
	Answer   string   `json:"answer"`
	Rule     string   `json:"rule,omitempty"` // 补充规则: 情况 → 结果
	IDs      []string `json:"ids,omitempty"`  // 问题编号: <轮次 ID>#F2
}

// LoadAnswers: API 的审查记录(A1…)+ 领域的审查记录(D1…)。文件不存在 = 没有。
func LoadAnswers(apiReview, domainReview string) ([]Answered, error) {
	var out []Answered
	for _, x := range []struct{ path, prefix string }{{apiReview, "A"}, {domainReview, "D"}} {
		if x.path == "" {
			continue
		}
		as, err := loadAnswerFile(x.path, x.prefix)
		if err != nil {
			return nil, err
		}
		out = append(out, as...)
	}
	return out, nil
}

func loadAnswerFile(path, prefix string) ([]Answered, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc yaml.Node // 节点: 记录里 同一个问题 可以有多行(同名的键),解码成 map 会报错
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	list := mapGet(doc.Content[0], "问答")
	if list == nil || list.Kind != yaml.SequenceNode {
		return nil, nil
	}
	var out []Answered
	for i, e := range list.Content {
		if e.Kind != yaml.MappingNode {
			continue
		}
		a := Answered{Ref: fmt.Sprintf("%s%d", prefix, i+1)}
		for j := 0; j+1 < len(e.Content); j += 2 {
			k, v := e.Content[j].Value, e.Content[j+1]
			switch k {
			case "问题":
				a.Question = v.Value
			case "同一个问题":
				a.Also = append(a.Also, scalars(v)...)
			case "位置":
				a.Location = v.Value
			case "选项":
				a.Options = scalars(v)
			case "回答":
				a.Answer = v.Value
			case "问题编号":
				a.IDs = append(a.IDs, scalars(v)...)
			case "补充规则":
				var parts []string
				for _, r := range seqOrOne(v) {
					parts = append(parts, strings.TrimSpace(mapStr(r, "情况")+" → "+mapStr(r, "结果")))
				}
				a.Rule = strings.Join(parts, "; ")
			}
		}
		out = append(out, a)
	}
	return out, nil
}

func scalars(n *yaml.Node) []string {
	if n.Kind == yaml.ScalarNode {
		return []string{n.Value}
	}
	var out []string
	for _, c := range n.Content {
		if c.Kind == yaml.ScalarNode {
			out = append(out, c.Value)
		}
	}
	return out
}

func seqOrOne(n *yaml.Node) []*yaml.Node {
	if n.Kind == yaml.SequenceNode {
		return n.Content
	}
	return []*yaml.Node{n}
}

func mapStr(n *yaml.Node, k string) string {
	if v := mapGet(n, k); v != nil {
		return v.Value
	}
	return ""
}

// norm: 比较用(空白不算)。
func norm(s string) string { return strings.Join(strings.Fields(s), "") }

// SameTopic: 同一个位置 + 同一组选项(至少 2 个)= 同一个问题。位置为空(以前的记录)不比较。
func SameTopic(loc1 string, opts1 []string, loc2 string, opts2 []string) bool {
	if norm(loc1) == "" || norm(loc1) != norm(loc2) || len(opts1) < 2 || len(opts1) != len(opts2) {
		return false
	}
	a, b := normSet(opts1), normSet(opts2)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func normSet(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = norm(x)
	}
	sort.Strings(out)
	return out
}

func labels(f Finding) []string {
	var out []string
	for _, o := range f.Options {
		out = append(out, o.Label)
	}
	return out
}

// covers: 这条记录回答了这个问题吗(ids = 这个问题的绝对编号 <轮次 ID>#Fn)。
func (a Answered) covers(f Finding, ids []string) bool {
	for _, id := range ids {
		for _, x := range a.IDs {
			if id == x {
				return true
			}
		}
	}
	q := norm(f.Question)
	if q != "" {
		if norm(a.Question) == q {
			return true
		}
		for _, x := range a.Also {
			if norm(x) == q {
				return true
			}
		}
	}
	return SameTopic(f.Location, labels(f), a.Location, a.Options)
}

// AnsweredBy: 回答了这个问题的记录的编号(没有 = "")。
func AnsweredBy(as []Answered, f Finding, ids ...string) string {
	for _, a := range as {
		if a.covers(f, ids) {
			return a.Ref
		}
	}
	return ""
}
