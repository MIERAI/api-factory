// Package review: 式样书审查的轮次和收敛规则(P5)。
//
// 每一轮审查都是一次独立的 AI 调用,抽样噪声会让后面的轮次冒出"上一轮本来就可以问"的新问题,
// 同一个问题的严重度也会漂移(试用: 一个 API 审了 4 轮)。收敛不能靠模型自觉,靠下面的机械规则:
//
// 第 1 轮: blocker / major 挡流水线(验收覆盖除外,只是建议)。
// 第 2 轮以后(上一轮没通过,中间回答了问题):
//   - 和已经回答过的问题相同 → 丢弃(重复提问)
//   - 和上一轮不挡流水线的问题相同 → 仍然不挡(严重度不能升级)
//   - 和上一轮挡流水线、又没回答的问题相同 → 仍然挡
//   - 由上一轮回答新增的规则引起(cause = new_rule)→ 照常
//   - 其他新问题(cause = existing: 上一轮就能问)→ 只有 blocker 挡;major 降为"迟到的问题"(显示,可以回答)
//
// 轮次从 .factory/reviews 推出来(不另外存状态): 同一个 API、同一个标签,最后一次通过之后连续没通过的次数 + 1。
package review

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"apifactory/factory/answer"
)

// Finding: 审查员的输出(answer.Finding)+ 跨轮次的字段。
type Finding struct {
	answer.Finding
	SameAs string `json:"same_as,omitempty"` // 和上一轮的哪个问题相同(R1-F3)
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
	Full     Finding `json:"full"` // 原来的问题(没回答的挡流水线的问题要原样带到这一轮)
}

// Verdict: 一轮审查的结论(写进审查目录的 verdict.json)。
type Verdict struct {
	Round    int       `json:"round"`
	Passed   bool      `json:"passed"`
	Blocking []string  `json:"blocking"` // 挡流水线的问题 id
	Advisory []string  `json:"advisory"` // 覆盖建议 / minor
	Late     []string  `json:"late"`     // 迟到的问题(第 2 轮以后的新 major,上一轮就能问)
	Repeated []string  `json:"repeated"` // 重复提问(已经回答过),丢弃
	Carried  []Finding `json:"-"`        // 带过来的问题(写进这一轮的 findings.json)
	Seconds  int       `json:"seconds"`
}

// Gate: 收敛规则(纯函数)。
func Gate(round int, fs []Finding, prev []Prev) Verdict {
	v := Verdict{Round: round}
	byRef := map[string]Prev{}
	for _, p := range prev {
		byRef[p.Ref] = p
	}
	for _, f := range fs {
		blocking := f.Finding.Blocking()
		if round > 1 {
			if p, ok := byRef[f.SameAs]; ok {
				switch {
				case p.Answered:
					v.Repeated = append(v.Repeated, f.ID)
					continue
				case !p.Blocking:
					blocking = false
				default:
					blocking = true
				}
			} else if f.Cause != "new_rule" && f.Severity != "blocker" && blocking {
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

// Cycle: 当前这一轮之前、最后一次通过之后连续没通过的审查(按时间顺序)。这一次是第 len+1 轮。
func Cycle(history []string) []string {
	i := len(history)
	for i > 0 {
		v, err := ReadVerdict(history[i-1])
		if err != nil || v.Passed {
			break
		}
		i--
	}
	return history[i:]
}

// Previous: 这个周期里之前各轮的问题,标上"是否已经回答"(问题原文出现在 API 或领域的审查记录里)。
func Previous(cycle []string, answers ...string) ([]Prev, error) {
	var text strings.Builder
	for _, a := range answers {
		b, _ := os.ReadFile(a)
		text.Write(b)
	}
	var out []Prev
	seen := map[string]bool{}
	for r, dir := range cycle {
		fs, err := ReadFindings(dir)
		if err != nil {
			return nil, err
		}
		v, _ := ReadVerdict(dir)
		blocking := map[string]bool{}
		for _, id := range v.Blocking {
			blocking[id] = true
		}
		for _, f := range fs {
			ref := fmt.Sprintf("R%d-%s", r+1, f.ID)
			if strings.HasPrefix(f.ID, "R") { // 带过来的问题保留原来的编号
				ref = f.ID
			}
			if seen[ref] {
				continue
			}
			seen[ref] = true
			out = append(out, Prev{
				Ref: ref, Severity: f.Severity, Category: f.Category, Question: f.Question, Full: f,
				Blocking: blocking[f.ID], Answered: strings.Contains(text.String(), strconv.Quote(f.Question)),
			})
		}
	}
	return out, nil
}

// Resolve: 审查员写的 same_as 可能只有 F3,补成 R<n>-F3。
func Resolve(fs []Finding, round int) {
	for i := range fs {
		s := strings.TrimSpace(fs[i].SameAs)
		if s != "" && !strings.HasPrefix(s, "R") {
			fs[i].SameAs = fmt.Sprintf("R%d-%s", round-1, s)
		} else {
			fs[i].SameAs = s
		}
	}
}
