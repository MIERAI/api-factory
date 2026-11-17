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
	"sort"
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
}

type Option struct {
	Label string `json:"label"`
	Rule  struct {
		When string `json:"when"`
		Then string `json:"then"`
	} `json:"rule"`
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
func (f Finding) Answerable() bool { return f.Category != "验收覆盖" }

// LatestReview: .factory/reviews 下这个 API 最新的审查结果。
func LatestReview(apiID string) (string, []Finding, error) {
	dirs, _ := filepath.Glob(filepath.Join(".factory/reviews", "*-"+apiID+"*"))
	sort.Strings(dirs)
	for i := len(dirs) - 1; i >= 0; i-- {
		b, err := os.ReadFile(filepath.Join(dirs[i], "findings.json"))
		if err != nil {
			continue
		}
		var fs []Finding
		if err := json.Unmarshal(b, &fs); err != nil {
			return "", nil, fmt.Errorf("%s: %w", dirs[i], err)
		}
		return dirs[i], fs, nil
	}
	return "", nil, fmt.Errorf("没有找到 %s 的审查结果(先运行 scripts/review.sh)", apiID)
}

// Decision: 一个问题的回答
type Decision struct {
	Finding Finding
	Answer  string // 选中的选项或自己写的回答
	When    string
	Then    string
	Domain  bool // 写进 领域.审查.yaml
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
		}
		fmt.Fprintf(out, "  0) 跳过    9) 自己写\n  选择 [%d]: ", f.Recommended+1)
		choice := read()
		if choice == "" {
			choice = strconv.Itoa(f.Recommended + 1)
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
		d, ok := decide(f, val, func() string { return val })
		if !ok {
			continue
		}
		d.Domain = toDomain || (f.Scope == "domain" && !strings.HasSuffix(a, "@api"))
		ds = append(ds, d)
	}
	return ds, nil
}

func decide(f Finding, choice string, freeText func() string) (Decision, bool) {
	n, err := strconv.Atoi(choice)
	switch {
	case err == nil && n == 0:
		return Decision{}, false
	case err == nil && n >= 1 && n <= len(f.Options):
		o := f.Options[n-1]
		return Decision{Finding: f, Answer: o.Label, When: o.Rule.When, Then: o.Rule.Then}, true
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
	if len(opts) > 0 {
		fmt.Fprintf(&b, "    选项: [%s]\n", strings.Join(opts, ", "))
	}
	fmt.Fprintf(&b, "    回答: %s\n", q(d.Answer))
	if d.When != "" && d.Then != "" {
		fmt.Fprintf(&b, "    补充规则: { 情况: %s, 结果: %s }\n", q(d.When), q(d.Then))
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
