// Package review: 式样书审查的轮次和收敛规则(P5)。
//
// 每一轮审查都是一次独立的 AI 调用,抽样噪声会让后面的轮次冒出"上一轮本来就可以问"的新问题,
// 同一个问题的严重度也会漂移(试用: 一个 API 审了 4 轮)。收敛不能靠模型自觉,靠下面的机械规则:
//
// 每一轮(第 1 轮也一样):
//   - 已经回答过的问题 → 丢弃(重复提问)。"回答过" = 审查员的 same_as 指向一条回答记录(A3 / D1,编号存在才算)
//     或之前轮次里已经回答的问题;或者不靠模型: 问题原文 / 位置 + 选项 和回答记录相同(answers.go)
//
// 第 1 轮: blocker / major 挡流水线(验收覆盖除外,只是建议)。
// 第 2 轮以后(上一轮没通过,中间回答了问题):
//   - 和上一轮不挡流水线的问题相同 → 仍然不挡(严重度不能升级)
//   - 和上一轮挡流水线、又没回答的问题相同 → 仍然挡
//   - 由上一轮回答新增的规则引起(cause = new_rule)→ 照常
//   - 其他新问题(cause = existing: 上一轮就能问)→ 只有 blocker 挡;major 降为"迟到的问题"(显示,可以回答)
//
// 超过 MaxRounds 轮(默认 3,factory.yaml 的 review.max_rounds)以后: 新的问题只有 blocker 挡,major 记为
// "封顶"(显示、写进轮次记录,设计者可以回答,但不挡)。之前挡了、还没回答的问题仍然挡(回答一句就行)。
//
// 轮次从历史推出来(rounds.go: 审查目录 + 提交的轮次记录,本地和 CI 共用): 同一个 API、同一个标签,
// 最后一次通过之后连续没通过的次数 + 1。
package review

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"apifactory/factory/answer"
)

// Finding: 审查员的输出(answer.Finding)+ 跨轮次的字段。
type Finding struct {
	answer.Finding
	SameAs string `json:"same_as,omitempty"` // 和哪个问题相同: 之前的轮次 R1-F3 / 回答记录 A3、D1
	Cause  string `json:"cause,omitempty"`   // new_rule | existing
}

// Prev: 上一轮的问题(给审查员看,也给收敛规则用)。
type Prev struct {
	Ref      string  `json:"ref"` // R1-F3
	Severity string  `json:"severity"`
	Category string  `json:"category"`
	Question string  `json:"question"`
	Blocking bool    `json:"blocking"`
	Answered bool    `json:"answered"`
	By       string  `json:"by,omitempty"` // 回答它的记录(A3 / D1)
	Full     Finding `json:"full"`         // 原来的问题(没回答的挡流水线的问题要原样带到这一轮)
}

// Verdict: 一轮审查的结论(写进审查目录的 verdict.json)。
type Verdict struct {
	Round      int               `json:"round"`
	Passed     bool              `json:"passed"`
	Blocking   []string          `json:"blocking"`              // 挡流水线的问题 id
	Advisory   []string          `json:"advisory"`              // 覆盖建议 / minor
	Late       []string          `json:"late"`                  // 迟到的问题(第 2 轮以后的新 major,上一轮就能问)
	Repeated   []string          `json:"repeated"`              // 重复提问(已经回答过),丢弃
	RepeatedOf map[string]string `json:"repeated_of,omitempty"` // 重复提问 → 回答过它的(A3 / R1-F2)
	Capped     []string          `json:"capped,omitempty"`      // 超过轮次上限以后的新 major: 记下来,不挡
	Carried    []Finding         `json:"-"`                     // 带过来的问题(写进这一轮的 findings.json)
	Seconds    int               `json:"seconds"`
}

// DefaultMaxRounds: 第几轮以后新的 major 不再挡流水线(factory.yaml review.max_rounds 可以改;0 = 不封顶)。
const DefaultMaxRounds = 3

// Context: 收敛规则的其他输入。
type Context struct {
	Answers   []Answered // 回答记录(X.审查.yaml + 领域.审查.yaml)
	MaxRounds int        // 0 = 不封顶
}

// Gate: 收敛规则(纯函数),不看回答记录、不封顶(以前的签名)。
func Gate(round int, fs []Finding, prev []Prev) Verdict { return GateWith(round, fs, prev, Context{}) }

// GateWith: 收敛规则(纯函数)。
func GateWith(round int, fs []Finding, prev []Prev, ctx Context) Verdict {
	v := Verdict{Round: round}
	byRef := map[string]Prev{}
	for _, p := range prev {
		byRef[p.Ref] = p
	}
	answerRefs := map[string]bool{}
	for _, a := range ctx.Answers {
		answerRefs[a.Ref] = true
	}
	capped := ctx.MaxRounds > 0 && round > ctx.MaxRounds
	for _, f := range fs {
		if by := repeatOf(f, byRef, answerRefs, prev, ctx.Answers); by != "" {
			v.Repeated = append(v.Repeated, f.ID)
			if v.RepeatedOf == nil {
				v.RepeatedOf = map[string]string{}
			}
			v.RepeatedOf[f.ID] = by
			continue
		}
		blocking := f.Finding.Blocking()
		if p, ok := byRef[f.SameAs]; ok {
			blocking = p.Blocking // 严重度不能升级;挡过、没回答 → 仍然挡(封顶以后也挡: 之前就问过)
		} else if blocking && f.Severity != "blocker" {
			switch {
			case capped:
				v.Capped = append(v.Capped, f.ID)
				continue
			case round > 1 && f.Cause != "new_rule":
				v.Late = append(v.Late, f.ID)
				continue
			}
		}
		if blocking {
			v.Blocking = append(v.Blocking, f.ID)
		} else {
			v.Advisory = append(v.Advisory, f.ID)
		}
	}
	// 之前挡流水线、还没回答的问题: 审查员这一轮没再提,也继续挡(实测: 审查员看到"之前问过"的列表就不再提,
	// 问题没回答就消失了)。带进这一轮的问题列表,factory answer 才能回答它。
	raised := map[string]bool{}
	for _, f := range fs {
		raised[f.SameAs] = true
	}
	for _, p := range prev {
		if p.Blocking && !p.Answered && !raised[p.Ref] {
			c := p.Full
			c.ID, c.SameAs = p.Ref, ""
			v.Carried = append(v.Carried, c)
			v.Blocking = append(v.Blocking, p.Ref)
		}
	}
	v.Passed = len(v.Blocking) == 0
	return v
}

// repeatOf: 这个问题已经回答过 → 回答过它的编号。
//   - same_as 指向回答记录(编号存在)或之前轮次里已经回答的问题(模型说的,编号校验过)
//   - 不靠模型: 问题原文 / 位置 + 选项 和回答记录相同,或和之前轮次里已经回答的问题相同
func repeatOf(f Finding, byRef map[string]Prev, answerRefs map[string]bool, prev []Prev, answers []Answered) string {
	if answerRefs[f.SameAs] {
		return f.SameAs
	}
	if p, ok := byRef[f.SameAs]; ok && p.Answered {
		return f.SameAs
	}
	if by := AnsweredBy(answers, f); by != "" {
		return by
	}
	q := norm(f.Question)
	for _, p := range prev {
		if p.Answered && ((q != "" && norm(p.Question) == q) || SameTopic(f.Location, labels(f), p.Full.Location, labels(p.Full))) {
			return p.Ref
		}
	}
	return ""
}

var dirRe = regexp.MustCompile(`^\d{8}-\d{6}-([^-]+)-(.+)$`)

// History: 这个 API(和标签)过去的审查目录,按时间顺序。只算有 verdict.json 的。
func History(root, apiID, label string) []string {
	ents, _ := os.ReadDir(root)
	want := apiID
	if label != "" {
		want += "-" + label
	}
	var out []string
	for _, e := range ents {
		m := dirRe.FindStringSubmatch(e.Name())
		if m == nil || m[2] != want {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "verdict.json")); err == nil {
			out = append(out, filepath.Join(root, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

func ReadVerdict(dir string) (Verdict, error) {
	var v Verdict
	b, err := os.ReadFile(filepath.Join(dir, "verdict.json"))
	if err != nil {
		return v, err
	}
	return v, json.Unmarshal(b, &v)
}

func ReadFindings(dir string) ([]Finding, error) {
	var fs []Finding
	b, err := os.ReadFile(filepath.Join(dir, "findings.json"))
	if err != nil {
		return nil, err
	}
	return fs, json.Unmarshal(b, &fs)
}

// Cycle: 当前这一轮之前、最后一次通过之后连续没通过的轮次(按时间顺序)。这一次是第 len+1 轮。
func Cycle(history []Round) []Round {
	i := len(history)
	for i > 0 && !history[i-1].Verdict.Passed {
		i--
	}
	return history[i:]
}

var carriedRe = regexp.MustCompile(`^R(\d+)-(F\d+)$`)

// AbsIDs: 一个问题的绝对编号(<轮次 ID>#F2): 本地和 CI 都认识(回答记录的 问题编号)。
// 带过来的问题(R1-F2)= 它原来那一轮的编号 + 这一轮的编号。
func AbsIDs(cycle []Round, r int, f Finding) []string {
	ids := []string{cycle[r].ID + "#" + f.ID}
	if id := RefID(cycle, f.ID); id != "" {
		ids = append(ids, id)
	}
	return ids
}

// Previous: 这个周期里之前各轮的问题,标上"是否已经回答"(answers.go 的条件;同一个编号在任何一轮里回答过都算)。
func Previous(cycle []Round, answers []Answered) []Prev {
	var out []Prev
	at := map[string]int{}
	for r, round := range cycle {
		blocking := map[string]bool{}
		for _, id := range round.Verdict.Blocking {
			blocking[id] = true
		}
		for _, f := range round.Findings {
			ref := fmt.Sprintf("R%d-%s", r+1, f.ID)
			if strings.HasPrefix(f.ID, "R") { // 带过来的问题保留原来的编号
				ref = f.ID
			}
			by := AnsweredBy(answers, f, AbsIDs(cycle, r, f)...)
			if i, ok := at[ref]; ok {
				if by != "" && !out[i].Answered {
					out[i].Answered, out[i].By = true, by
				}
				continue
			}
			at[ref] = len(out)
			out = append(out, Prev{
				Ref: ref, Severity: f.Severity, Category: f.Category, Question: f.Question, Full: f,
				Blocking: blocking[f.ID], Answered: by != "", By: by,
			})
		}
	}
	return out
}

var shortRefRe = regexp.MustCompile(`^F\d+$`)

// Resolve: 审查员写的 same_as 可能只有 F3,补成 R<n>-F3(A3 / D1 / R1-F3 原样)。
func Resolve(fs []Finding, round int) {
	for i := range fs {
		s := strings.TrimSpace(fs[i].SameAs)
		if shortRefRe.MatchString(s) && round > 1 {
			s = fmt.Sprintf("R%d-%s", round-1, s)
		}
		fs[i].SameAs = s
	}
}

// RefID: 之前轮次的编号(R2-F1)→ 绝对编号(<轮次 ID>#F1)。认不出 = ""。
func RefID(cycle []Round, ref string) string {
	m := carriedRe.FindStringSubmatch(ref)
	if m == nil {
		return ""
	}
	var n int
	fmt.Sscanf(m[1], "%d", &n)
	if n < 1 || n > len(cycle) {
		return ""
	}
	return cycle[n-1].ID + "#" + m[2]
}
