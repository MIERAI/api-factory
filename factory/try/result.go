package try

import (
	"sort"
	"strings"

	"apifactory/factory/export"
)

// 状态(Result.Status)。
const (
	StatusOK          = "ok"           // 跑完了(用例可能有失败)
	StatusGateFailed  = "gate_failed"  // 设计 / 式样书闸门不通过
	StatusBuildFailed = "build_failed" // 编译不通过(服务或测试)
	StatusNeedsAI     = "needs_ai"     // 生成需要 AI(例如计算 API 没有逻辑文件): 不调用 AI,停在这里
	StatusError       = "error"        // 基础设施的错误(docker 没有等)
)

// 停在哪个阶段(Result.Stage)。跑完 = accept。
const (
	StageDesign  = "design"
	StagePrepare = "prepare"
	StageBuild   = "build"
	StageTest    = "test"
	StageAccept  = "accept"
)

// 用例的来源(Case.Source)。
const (
	SourceExample      = "example"       // 设计书的 例子.用例 / 式样书的 acceptance.cases(人写的例子)
	SourceAuto         = "auto"          // 自动生成的验收用例
	SourceDifferential = "differential"  // 差分用例(和参考求值器比)
	SourceLogicExample = "logic_example" // 从逻辑样例生成的单元测试(*_logic_examples_gen_test.go)
	SourceHandTest     = "hand_test"     // 逃生层里手写的单元测试
)

// Sources: 所有来源(summary.by_source 总是全部列出)。
var Sources = []string{SourceExample, SourceAuto, SourceDifferential, SourceLogicExample, SourceHandTest}

// Result 是 factory try 的结果。
type Result struct {
	Schema  string      `json:"schema"`
	Base    string      `json:"base"`
	Status  string      `json:"status"`
	Stage   string      `json:"stage"`
	Message string      `json:"message"`
	APIs    []APIResult `json:"apis"`
	Summary Summary     `json:"summary"`
}

// APIResult 是一个 API 的用例。
type APIResult struct {
	ID    string `json:"id"`
	Cases []Case `json:"cases"`
	// Error: 这个 API 的验收没能执行(例如服务器启动不了)。用例是空的。
	Error string `json:"error,omitempty"`
}

// Case 是一个用例(验收用例或单元测试)。
type Case struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"` // 失败的第一行
}

// Summary 是全部 API 的计数。
type Summary struct {
	Passed   int               `json:"passed"`
	Failed   int               `json:"failed"`
	BySource map[string]*Count `json:"by_source"`
}

// Count 是通过 / 失败的个数。
type Count struct {
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

// summarize 重新计算 Summary。
func (r *Result) summarize() {
	s := Summary{BySource: map[string]*Count{}}
	for _, src := range Sources {
		s.BySource[src] = &Count{}
	}
	for _, a := range r.APIs {
		for _, c := range a.Cases {
			cnt := s.BySource[c.Source]
			if cnt == nil {
				cnt = &Count{}
				s.BySource[c.Source] = cnt
			}
			if c.Passed {
				s.Passed++
				cnt.Passed++
			} else {
				s.Failed++
				cnt.Failed++
			}
		}
	}
	r.Summary = s
}

// SuiteSource: 验收结果的 suite(factory accept --results)→ 来源。和终端输出的分类一样: 空 = 人写,diff = 差分,
// 其他(auto:*)= 自动生成。
func SuiteSource(suite string) string {
	switch {
	case suite == "":
		return SourceExample
	case suite == "diff":
		return SourceDifferential
	default:
		return SourceAuto
	}
}

// firstLine: 多行的说明只取第一个非空行。
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// SelectAPIs: 改动的文件涉及的 API(清单的 source / spec / rules / examples / generated / escape / acceptance)。
// 设计书目录里不是某个 API 的来源的文件(领域.yaml 等)→ 这个目录的全部 API。一个都没有 → 全部。
// 返回值排好序、不重复。
func SelectAPIs(m *export.Manifest, changed []string) []string {
	hit := map[string]bool{}
	for _, p := range changed {
		matched := false
		for _, a := range m.APIs {
			if touches(a, p) {
				hit[a.ID] = true
				matched = true
			}
		}
		if matched {
			continue
		}
		// 设计书目录的公共文件(领域.yaml、类型等): 同一个目录的 API
		if dir, ok := designDir(p); ok {
			for _, a := range m.APIs {
				if d, ok := designDir(a.Source.Path); ok && d == dir {
					hit[a.ID] = true
				}
			}
		}
	}
	if len(hit) == 0 {
		for _, a := range m.APIs {
			hit[a.ID] = true
		}
	}
	out := make([]string, 0, len(hit))
	for id := range hit {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func touches(a export.API, p string) bool {
	if p == a.Source.Path || p == a.Spec || (a.Acceptance != "" && p == a.Acceptance) {
		return true
	}
	for _, g := range a.Generated {
		if p == g {
			return true
		}
	}
	for _, e := range a.Escape {
		if p == e.Path {
			return true
		}
	}
	for _, r := range a.Rules {
		if p == r.Path {
			return true
		}
	}
	for _, e := range a.Examples {
		if p == e.Path {
			return true
		}
	}
	return false
}

// designDir: design/<领域>/… → design/<领域>。
func designDir(p string) (string, bool) {
	parts := strings.Split(p, "/")
	if len(parts) >= 3 && parts[0] == "design" {
		return parts[0] + "/" + parts[1], true
	}
	return "", false
}

// MissingEscape: 需要 AI 才能生成的 API(逃生层的主文件 —— 计算的逻辑文件 / custom 的 handler —— 还没写)。
// 返回 "API id: 文件" 的列表(排好序)。
func MissingEscape(m *export.Manifest) []string {
	var out []string
	for _, a := range m.APIs {
		for _, e := range a.Escape {
			if !e.Exists && (e.Kind == "logic" || e.Kind == "handler") {
				out = append(out, a.ID+": "+e.Path)
			}
		}
	}
	sort.Strings(out)
	return out
}
