package review

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"apifactory/factory/answer"
)

func f(id, sev, cat, sameAs, cause string) Finding {
	return Finding{Finding: answer.Finding{ID: id, Severity: sev, Category: cat}, SameAs: sameAs, Cause: cause}
}

func TestGateRound1(t *testing.T) {
	v := Gate(1, []Finding{
		f("F1", "blocker", "矛盾", "", ""),
		f("F2", "major", "边界和单位", "", ""),
		f("F3", "major", "验收覆盖", "", ""), // 覆盖建议不挡
		f("F4", "minor", "矛盾", "", ""),
	}, nil)
	if !reflect.DeepEqual(v.Blocking, []string{"F1", "F2"}) || !reflect.DeepEqual(v.Advisory, []string{"F3", "F4"}) || v.Passed {
		t.Fatalf("%+v", v)
	}
}

func TestGateLaterRounds(t *testing.T) {
	prev := []Prev{
		{Ref: "R1-F1", Blocking: true, Answered: true},   // 回答过
		{Ref: "R1-F2", Blocking: false, Answered: false}, // 上一轮是 minor,没回答
		{Ref: "R1-F3", Blocking: true, Answered: false},  // 上一轮挡了,却没回答
	}
	v := Gate(2, []Finding{
		f("F1", "major", "矛盾", "R1-F1", "existing"),    // 重复提问 → 丢弃
		f("F2", "major", "矛盾", "R1-F2", "existing"),    // 严重度升级 → 仍然不挡
		f("F3", "major", "边界和单位", "R1-F3", "existing"), // 没回答的挡流水线的问题 → 仍然挡
		f("F4", "major", "空和零", "", "new_rule"),        // 回答新增的规则引起 → 挡
		f("F5", "major", "数据异常", "", "existing"),       // 上一轮就能问 → 迟到
		f("F6", "blocker", "验收用例的正确性", "", "existing"), // blocker 永远挡
		f("F7", "minor", "矛盾", "", "existing"),
	}, prev)
	want := Verdict{Round: 2, Blocking: []string{"F3", "F4", "F6"}, Advisory: []string{"F2", "F7"}, Late: []string{"F5"}, Repeated: []string{"F1"},
		RepeatedOf: map[string]string{"F1": "R1-F1"}}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("got  %+v\nwant %+v", v, want)
	}
}

// 之前挡流水线、没回答的问题,审查员这一轮没再提 → 仍然挡,并且带进这一轮(才能回答)。
func TestGateCarriesUnanswered(t *testing.T) {
	prev := []Prev{
		{Ref: "R1-F1", Blocking: true, Answered: false, Full: f("F1", "major", "矛盾", "", "")},
		{Ref: "R1-F2", Blocking: true, Answered: true},
		{Ref: "R1-F3", Blocking: false, Answered: false},
	}
	v := Gate(2, nil, prev)
	if v.Passed || !reflect.DeepEqual(v.Blocking, []string{"R1-F1"}) || len(v.Carried) != 1 || v.Carried[0].ID != "R1-F1" {
		t.Fatalf("%+v", v)
	}
	v = Gate(2, []Finding{f("F1", "major", "矛盾", "R1-F1", "existing")}, prev) // 审查员又提了: 不重复计
	want := Verdict{Round: 2, Blocking: []string{"F1"}}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("got  %+v\nwant %+v", v, want)
	}
}

func TestCycle(t *testing.T) {
	root := t.TempDir()
	write := func(name, verdict string) {
		d := filepath.Join(root, name)
		_ = os.MkdirAll(d, 0o755)
		if verdict != "" {
			_ = os.WriteFile(filepath.Join(d, "verdict.json"), []byte(verdict), 0o644)
		}
	}
	write("20261117-090000-default-a.b", `{"round":1,"passed":true}`)
	write("20261117-090100-default-a.b", `{"round":1,"passed":false}`)
	write("20261117-090150-default-a.b", "") // 解析失败(没有 verdict)→ 不算
	write("20261117-090200-default-a.b", `{"round":2,"passed":false}`)
	write("20261117-090300-default-a.b-x", `{"round":1,"passed":false}`) // 别的标签
	write("20261117-090400-default-a.bc", `{"round":1,"passed":false}`)  // 别的 API
	h := History(root, "a.b", "")
	if len(h) != 3 {
		t.Fatalf("history %v", h)
	}
	rs, err := Rounds(root, "", "a.b", "")
	if err != nil || len(rs) != 3 {
		t.Fatalf("rounds %v %v", rs, err)
	}
	if c := Cycle(rs); len(c) != 2 {
		t.Fatalf("cycle %v(应该是第 3 轮)", c)
	}
}
