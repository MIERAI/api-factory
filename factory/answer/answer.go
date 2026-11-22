// Package answer: 回答审查的问题,把回答写进审查记录(X.审查.yaml / 领域.审查.yaml)。
// 设计者只需要选择或写一句话;写文件(包括引号这类 YAML 细节)由工厂负责。
package answer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Finding struct {
	ID           string   `json:"id"`
	Severity     string   `json:"severity"`
	Category     string   `json:"category"`
	Scope        string   `json:"scope"`
	Location     string   `json:"location"`
	Question     string   `json:"question"`
	Why          string   `json:"why"`
	Options      []Option `json:"options"`
	Recommended  int      `json:"recommended"`
	DesignChange string   `json:"design_change"`

	Blocks bool     `json:"-"` // 这一轮挡流水线(verdict.json)
	Also   []string `json:"-"` // 之前轮次的同一个问题的原文(same_as 的链): 回答时一起记下
	Refs   []string `json:"-"` // 这个问题的绝对编号(<轮次 ID>#F2,包括 same_as 的链): 写进 问题编号,本地 / CI 都按它认
}

type Option struct {
	Label string `json:"label"`
	Rule  struct {
		When string `json:"when"`
		Then string `json:"then"`
	} `json:"rule"`
	InputChange []InputChange `json:"input_change,omitempty"` // 选中后直接改设计稿的输入定义
	AddRules    []string      `json:"add_rules,omitempty"`    // 选中后追加到设计稿的"输入之间的规则"
	Set         []KeySet      `json:"set,omitempty"`          // 选中后改设计稿顶层的单值键(上限 / 条数 …)
}

// UnmarshalJSON: 旧格式的审查结果里选项是纯字符串
func (o *Option) UnmarshalJSON(b []byte) error {
	var label string
	if err := json.Unmarshal(b, &label); err == nil {
		o.Label = label
		return nil
	}
	type plain Option
	return json.Unmarshal(b, (*plain)(o))
}

// Blocking: 和 review.sh 的收敛规则一致(验收覆盖只是建议)
func (f Finding) Blocking() bool {
	return (f.Severity == "blocker" || f.Severity == "major") && f.Category != "验收覆盖"
}

// Answerable: 需要人做决定的问题 = 除了"验收覆盖"(覆盖建议: 补充例子即可)以外的全部。
// minor 的矛盾也要能回答: 不记录的话,下一轮审查会把同一个问题升级后再问一遍(试用里实际发生)。
// 挡流水线的问题(和之前没回答的问题相同,严重度不能降)即使是验收覆盖也要能回答,否则永远挡。
func (f Finding) Answerable() bool { return f.Category != "验收覆盖" || f.Blocks }

// Decision: 一个问题的回答
type Decision struct {
	Finding  Finding
	Answer   string // 选中的选项或自己写的回答
	When     string
	Then     string
	Domain   bool          // 写进 领域.审查.yaml
	Changes  []InputChange // 改设计稿的输入定义
	Rules    []string      // 追加的输入之间的规则
	Sets     []KeySet      // 改顶层的单值键
	ByDesign string        // 已经直接改了设计书(这个 PR 里): 改了什么。不写补充规则
}

// ByDesignPrefix: 命令行回答"已经直接改了设计书": F1=@design 或 F1=@design:改成了一览
const ByDesignPrefix = "@design"

func byDesign(f Finding, note string) Decision {
	note = strings.TrimSpace(note)
	if note == "" {
		note = "见这次的设计书改动"
	}
	return Decision{Finding: f, Answer: "已经直接修改了设计书", ByDesign: note}
}

// Ask: 在终端里一题一题地问。
func Ask(in io.Reader, out io.Writer, fs []Finding, domain string) []Decision {
	r := bufio.NewReader(in)
	read := func() string {
		s, _ := r.ReadString('\n')
		return strings.TrimSpace(s)
	}
	var ds []Decision
	for _, f := range fs {
		fmt.Fprintf(out, "\n[%s %s] %s\n", f.ID, f.Severity, f.Question)
		if f.Why != "" {
			fmt.Fprintf(out, "  为什么要问: %s\n", f.Why)
		}
		if f.DesignChange != "" {
			fmt.Fprintf(out, "  ⚠ 这个问题可能需要修改设计: %s\n", f.DesignChange)
		}
		for i, o := range f.Options {
			mark := ""
			if i == f.Recommended {
				mark = "  (推荐)"
			}
			fmt.Fprintf(out, "  %d) %s%s\n       → 规则: %s → %s\n", i+1, o.Label, mark, o.Rule.When, o.Rule.Then)
			for _, c := range o.InputChange {
				fmt.Fprintf(out, "       → 改设计: %s\n", c)
			}
			for _, r := range o.AddRules {
				fmt.Fprintf(out, "       → 改设计: 输入之间的规则 + %s\n", r)
			}
			for _, k := range o.Set {
				fmt.Fprintf(out, "       → 改设计: %s → %s\n", k.Key, k.Value)
			}
		}
		fmt.Fprintf(out, "  0) 跳过    8) 已经直接改了设计书    9) 自己写    keep:<现在的行为> / <n>:<实际的结果> = 写清楚行为\n  选择 [%d]: ", f.Recommended+1)
		choice := read()
		if choice == "" {
			choice = strconv.Itoa(f.Recommended + 1)
		}
		if choice == "8" && len(f.Options) < 8 {
			fmt.Fprint(out, "  改了什么(一句话,可以不写): ")
			ds = append(ds, byDesign(f, read()))
			continue
		}
		d, ok := decide(f, choice, func() string { fmt.Fprint(out, "  你的回答: "); return read() })
		if !ok {
			fmt.Fprintln(out, "  → 跳过")
			continue
		}
		if f.Scope == "domain" {
			fmt.Fprintf(out, "  这个回答适用于所有用到 %s 领域数据的 API 吗? [Y/n]: ", domain)
			d.Domain = strings.ToLower(read()) != "n"
		}
		ds = append(ds, d)
	}
	return ds
}

// FromArgs: 命令行一次性回答: F1=2 / F2=0(跳过)/ F3="自己写的回答" / F4=2@domain
// / F5=keep:"现在的行为"(保持现在的行为,规则写成这段文字)/ F6=2:"实际的结果"(选项 2,规则的结果写成这段文字)
func FromArgs(fs []Finding, args []string) ([]Decision, error) {
	byID := map[string]Finding{}
	for _, f := range fs {
		byID[f.ID] = f
	}
	var ds []Decision
	for _, a := range args {
		id, val, ok := strings.Cut(a, "=")
		if !ok {
			return nil, fmt.Errorf("参数 %q: 写成 F1=2 或 F1=\"回答\"", a)
		}
		f, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("没有问题 %s(这次没有写入任何回答)", id)
		}
		if !f.Answerable() {
			return nil, fmt.Errorf("%s 是覆盖建议(验收覆盖),不用回答: 把例子补进式样书即可(这次没有写入任何回答)", id)
		}
		toDomain := false
		if v, ok := strings.CutSuffix(val, "@domain"); ok {
			val, toDomain = v, true
		}
		if note, ok := strings.CutPrefix(val, ByDesignPrefix); ok && (note == "" || note[0] == ':') {
			d := byDesign(f, strings.TrimPrefix(note, ":"))
			d.Domain = toDomain
			ds = append(ds, d)
			continue
		}
		d, ok := decide(f, val, func() string { return val })
		if !ok {
			continue
		}
		d.Domain = toDomain || (f.Scope == "domain" && !strings.HasSuffix(a, "@api"))
		ds = append(ds, d)
	}
	return ds, nil
}

// KeepPrefix: 回答"保持现在的行为",并写清楚现在的行为是什么: F1=keep:没有库存时返回 0 件
const KeepPrefix = "keep:"

func decide(f Finding, choice string, freeText func() string) (Decision, bool) {
	// keep:<文字>: 保持现在的行为。回答 / 补充规则 = 设计者写的实际行为(选项的标签往往只是"保持现在的行为",
	// 记下来的规则没有内容,下一轮审查员又问"现在的行为是什么")
	if text, ok := strings.CutPrefix(choice, KeepPrefix); ok {
		text = strings.TrimSpace(text)
		if text == "" {
			return Decision{}, false
		}
		return Decision{Finding: f, Answer: "保持现在的行为: " + text, When: f.Question, Then: text}, true
	}
	// <n>:<文字>: 选项 n(它的输入修改等照常),规则的结果写成设计者的文字
	if num, text, ok := strings.Cut(choice, ":"); ok {
		if n, err := strconv.Atoi(num); err == nil && n >= 1 && n <= len(f.Options) && strings.TrimSpace(text) != "" {
			o := f.Options[n-1]
			text = strings.TrimSpace(text)
			when := o.Rule.When
			if when == "" {
				when = f.Question
			}
			return Decision{Finding: f, Answer: o.Label + ": " + text, When: when, Then: text, Changes: o.InputChange, Rules: o.AddRules, Sets: o.Set}, true
		}
	}
	n, err := strconv.Atoi(choice)
	switch {
	case err == nil && n == 0:
		return Decision{}, false
	case err == nil && n >= 1 && n <= len(f.Options):
		o := f.Options[n-1]
		return Decision{Finding: f, Answer: o.Label, When: o.Rule.When, Then: o.Rule.Then, Changes: o.InputChange, Rules: o.AddRules, Sets: o.Set}, true
	case err == nil && n == 9:
		text := freeText()
		if text == "" {
			return Decision{}, false
		}
		return Decision{Finding: f, Answer: text, When: f.Question, Then: text}, true
	default: // 直接写了回答
		return Decision{Finding: f, Answer: choice, When: f.Question, Then: choice}, true
	}
}

// Write: 把回答追加到审查记录。所有值都加引号(避开 YAML 的冒号等问题)。
func Write(designFile string, ds []Decision, reviewDir string) ([]string, error) {
	apiFile := strings.TrimSuffix(designFile, ".yaml") + ".审查.yaml"
	domainFile := filepath.Join(filepath.Dir(designFile), "领域.审查.yaml")
	api := strings.TrimSuffix(filepath.Base(designFile), ".yaml")
	src := fmt.Sprintf("%s 的审查 %s %s", api, time.Now().Format("2006-01-02"), filepath.Base(reviewDir))
	var touched []string
	var changes []InputChange
	for _, d := range ds {
		changes = append(changes, d.Changes...)
	}
	var rules []string
	var sets []KeySet
	for _, d := range ds {
		rules = append(rules, d.Rules...)
		sets = append(sets, d.Sets...)
	}
	if len(changes) > 0 || len(rules) > 0 || len(sets) > 0 { // 先改设计稿(失败时什么都不写)
		if err := ApplyKeySets(designFile, sets); err != nil {
			return nil, err
		}
		if err := ApplyInputChanges(designFile, changes); err != nil {
			return nil, err
		}
		if err := ApplyRuleAdditions(designFile, rules); err != nil {
			return nil, err
		}
		touched = append(touched, designFile)
	}
	for _, target := range []string{apiFile, domainFile} {
		var entries []string
		for _, d := range ds {
			if (target == domainFile) != d.Domain {
				continue
			}
			entries = append(entries, entry(d, src))
		}
		if len(entries) == 0 {
			continue
		}
		if err := appendEntries(target, entries); err != nil {
			return nil, err
		}
		touched = append(touched, target)
	}
	return touched, nil
}

func q(s string) string { return strconv.Quote(s) }

func entry(d Decision, src string) string {
	var opts []string
	for _, o := range d.Finding.Options {
		opts = append(opts, q(o.Label))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  - 问题: %s\n", q(d.Finding.Question))
	if d.Finding.Location != "" {
		fmt.Fprintf(&b, "    位置: %s\n", q(d.Finding.Location))
	}
	if len(opts) > 0 {
		fmt.Fprintf(&b, "    选项: [%s]\n", strings.Join(opts, ", "))
	}
	if len(d.Finding.Refs) > 0 {
		var refs []string
		for _, r := range d.Finding.Refs {
			refs = append(refs, q(r))
		}
		fmt.Fprintf(&b, "    问题编号: [%s]\n", strings.Join(refs, ", "))
	}
	for _, a := range d.Finding.Also {
		fmt.Fprintf(&b, "    同一个问题: %s\n", q(a))
	}
	fmt.Fprintf(&b, "    回答: %s\n", q(d.Answer))
	if d.When != "" && d.Then != "" {
		fmt.Fprintf(&b, "    补充规则: { 情况: %s, 结果: %s }\n", q(d.When), q(d.Then))
	}
	for _, c := range d.Changes {
		fmt.Fprintf(&b, "    改了设计: %s\n", q(c.String()))
	}
	for _, r := range d.Rules {
		fmt.Fprintf(&b, "    改了设计: %s\n", q("输入之间的规则 + "+r))
	}
	for _, k := range d.Sets {
		fmt.Fprintf(&b, "    改了设计: %s\n", q(k.Key+" → "+k.Value))
	}
	if d.ByDesign != "" {
		fmt.Fprintf(&b, "    改了设计: %s\n", q(d.ByDesign))
	}
	fmt.Fprintf(&b, "    来自: %s\n", q(src))
	return b.String()
}

const reviewHeader = `# ============================================================================
# 审查记录: 这个文件由工厂写入,不用自己写(factory answer)。
# ============================================================================
问答:
`

func appendEntries(path string, entries []string) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		b = []byte(reviewHeader)
	} else if err != nil {
		return err
	}
	s := string(b)
	switch {
	case strings.Contains(s, "问答: []"):
		s = strings.Replace(s, "问答: []", "问答:", 1)
	case !strings.Contains(s, "问答:"):
		s = strings.TrimRight(s, "\n") + "\n问答:\n"
	}
	// 追加到 问答: 列表的末尾(文件末尾的注释保留在最后)
	body, tail := s, ""
	if i := strings.LastIndex(s, "\n# "); i >= 0 && !strings.Contains(s[i+1:], "\n  - ") && i > strings.Index(s, "问答:") {
		body, tail = s[:i+1], s[i+1:]
	}
	body = strings.TrimRight(body, "\n") + "\n\n" + strings.Join(entries, "\n")
	if tail != "" {
		body += "\n" + tail
	}
	return os.WriteFile(path, []byte(body), 0o644)
}
