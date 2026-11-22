package review

// 审查循环的回归(2026-11-22/23 实测的三种循环),全部不调用模型:
//   - 本地回答过的问题,CI 换个说法再问 → 按编号 / 位置 + 选项 / same_as(校验过的 A 编号)认出来,丢弃
//   - 本地和 CI 的轮次各算各的 → 轮次记录(提交)+ 审查目录合成一份历史
//   - 回答一个又冒出一个 → 超过轮次上限后新的 major 只记录不挡

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"apifactory/factory/answer"
)

func opt(labels ...string) []answer.Option {
	var out []answer.Option
	for _, l := range labels {
		out = append(out, answer.Option{Label: l})
	}
	return out
}

func q(id, sev, loc, question string, opts ...string) Finding {
	return Finding{Finding: answer.Finding{ID: id, Severity: sev, Category: "边界和单位", Location: loc, Question: question, Options: opt(opts...)}}
}

// 第 1 轮也丢弃已经回答过的问题: same_as 指向存在的回答记录 / 位置 + 选项相同 / 原文相同。
// same_as 指向不存在的编号(模型编的)→ 不算,照常挡。
func TestGateDropsAnsweredTopics(t *testing.T) {
	answers := []Answered{
		{Ref: "A1", Question: "恰好 300 秒算不算超时?", Location: "业务规则 超时", Options: []string{"算", "不算"}},
		{Ref: "D1", Question: "价格为 NULL 时怎么办?"},
	}
	fs := []Finding{
		func() Finding { f := q("F1", "major", "例子", "换了说法的同一个问题"); f.SameAs = "A1"; return f }(),
		q("F2", "major", " 业务规则  超时", "300 秒整的任务是超时吗?", "不算", "算"), // 位置 + 选项相同(空白、顺序不算)
		q("F3", "blocker", "输入 price", "价格为 NULL 时怎么办?"),                // 原文和领域的记录相同
		func() Finding { f := q("F4", "major", "例子", "新问题"); f.SameAs = "A9"; return f }(), // 编号不存在
		q("F5", "major", "业务规则 超时", "超时的单位是秒还是毫秒?", "秒", "毫秒"), // 同一个位置、不同的选项 = 不同的问题
	}
	v := GateWith(1, fs, nil, Context{Answers: answers, MaxRounds: 3})
	if !reflect.DeepEqual(v.Repeated, []string{"F1", "F2", "F3"}) || !reflect.DeepEqual(v.Blocking, []string{"F4", "F5"}) {
		t.Fatalf("%+v", v)
	}
	if v.RepeatedOf["F1"] != "A1" || v.RepeatedOf["F2"] != "A1" || v.RepeatedOf["F3"] != "D1" {
		t.Fatalf("repeated_of %v", v.RepeatedOf)
	}
}

// 超过轮次上限: 新的 major(包括 new_rule 的追问)只记录不挡;blocker 照常挡;之前挡了、没回答的问题仍然挡。
func TestGateRoundCap(t *testing.T) {
	prev := []Prev{
		{Ref: "R1-F1", Blocking: true, Full: q("F1", "major", "a", "没回答的老问题")},
		{Ref: "R2-F1", Blocking: true, Full: q("F1", "major", "b", "又提了的老问题")},
	}
	fs := []Finding{
		func() Finding { f := q("F1", "major", "c", "关于回答的追问"); f.Cause = "new_rule"; return f }(),
		q("F2", "blocker", "d", "期待值算错了"),
		func() Finding { f := q("F3", "major", "b", "又提了的老问题(换了说法)"); f.SameAs = "R2-F1"; return f }(),
		q("F4", "minor", "e", "措辞"),
	}
	v := GateWith(4, fs, prev, Context{MaxRounds: 3})
	want := Verdict{Round: 4, Blocking: []string{"F2", "F3", "R1-F1"}, Advisory: []string{"F4"}, Capped: []string{"F1"}}
	v.Carried = nil
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("got  %+v\nwant %+v", v, want)
	}
	// 上限之内: 同样的 new_rule major 照常挡
	if v := GateWith(3, fs[:1], nil, Context{MaxRounds: 3}); !reflect.DeepEqual(v.Blocking, []string{"F1"}) {
		t.Fatalf("round 3: %+v", v)
	}
	// 0 = 不封顶
	if v := GateWith(9, fs[:1], nil, Context{}); !reflect.DeepEqual(v.Blocking, []string{"F1"}) {
		t.Fatalf("no cap: %+v", v)
	}
}

func TestResolveKeepsAnswerRefs(t *testing.T) {
	fs := []Finding{{SameAs: "F3"}, {SameAs: " A2 "}, {SameAs: "D1"}, {SameAs: "R1-F2"}, {SameAs: ""}}
	Resolve(fs, 3)
	var got []string
	for _, f := range fs {
		got = append(got, f.SameAs)
	}
	if want := []string{"R2-F3", "A2", "D1", "R1-F2", ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func round(id string, passed bool, blocking []string, fs ...Finding) Round {
	return Round{ID: id, Verdict: Verdict{Passed: passed, Blocking: blocking}, Findings: fs}
}

// 本地的轮次在轮次记录里(提交),CI 的轮次在 CI 缓存的审查目录里 → 合起来是一份历史:
// CI 的这一轮是第 3 轮,本地第 1 轮回答过的问题(按编号)算已回答,不会被 CI 当成"问过、没回答"带回来。
func TestSharedHistoryLocalAndCI(t *testing.T) {
	dir := t.TempDir()
	design := filepath.Join(dir, "design/shop/GetX.yaml")
	_ = os.MkdirAll(filepath.Dir(design), 0o755)
	ledger := LedgerPath(design)
	if !strings.HasSuffix(ledger, "GetX.审查轮次.jsonl") {
		t.Fatal(ledger)
	}
	// 本地: 第 1 轮(不通过)。之前那一轮通过的、更早的问题不该进历史
	local := round("20261122-100000-claude-shop.get-x", false, []string{"F1"},
		q("F1", "major", "业务规则 超时", "恰好 300 秒算不算超时?", "算", "不算"))
	old := round("20261121-090000-claude-shop.get-x", true, nil, q("F9", "minor", "x", "旧的"))
	if _, err := WriteLedger(ledger, old, local); err != nil {
		t.Fatal(err)
	}
	// CI: 第 2 轮(审查目录,CI 缓存;不通过)—— 同一个问题换了说法(CI 没看到本地的回答之前)
	reviews := filepath.Join(dir, ".factory/reviews")
	ci := filepath.Join(reviews, "20261122-110000-claude-shop.get-x")
	_ = os.MkdirAll(ci, 0o755)
	writeJSON(t, filepath.Join(ci, "verdict.json"), Verdict{Round: 2, Blocking: []string{"F1"}})
	writeJSON(t, filepath.Join(ci, "findings.json"), []Finding{q("F1", "major", "例子 case 3", "300 秒整的任务,结果是超时吗?")})
	// 别的 API 的目录: 不算
	_ = os.MkdirAll(filepath.Join(reviews, "20261122-110500-claude-shop.get-xy"), 0o755)

	rs, err := Rounds(reviews, ledger, "shop.get-x", "")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range rs {
		ids = append(ids, r.ID)
	}
	if want := []string{old.ID, local.ID, filepath.Base(ci)}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("history %v", ids)
	}
	cycle := Cycle(rs)
	if len(cycle) != 2 {
		t.Fatalf("cycle %d(这一轮应该是第 3 轮)", len(cycle))
	}
	// 设计者回答了本地的 F1 和 CI 的 F1(factory answer 写 问题编号)
	answers := []Answered{
		{Ref: "A1", Question: "恰好 300 秒算不算超时?", IDs: []string{local.ID + "#F1"}},
		{Ref: "A2", Question: "(答的时候的说法)", IDs: []string{filepath.Base(ci) + "#F1"}},
	}
	prev := Previous(cycle, answers)
	for _, p := range prev {
		if !p.Answered {
			t.Fatalf("%s 应该算已回答: %+v", p.Ref, p)
		}
	}
	// 第 3 轮: 两个老问题都不会被带过来
	if v := GateWith(3, nil, prev, Context{Answers: answers, MaxRounds: 3}); !v.Passed {
		t.Fatalf("%+v", v)
	}
	// 只回答了 CI 的那个: 本地的那个仍然挡(带过来)
	prev = Previous(cycle, answers[1:])
	if v := GateWith(3, nil, prev, Context{Answers: answers[1:]}); v.Passed || !reflect.DeepEqual(v.Blocking, []string{"R1-F1"}) {
		t.Fatalf("%+v", v)
	}
}

// 轮次记录只留"最后一次通过 + 之后的轮次";合并按 ID 去重、排序;内容不变不写。
func TestLedgerMergeTrim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "X.审查轮次.jsonl")
	r1 := round("20261120-000000-claude-a.b", false, []string{"F1"}, q("F1", "major", "l", "q1"))
	r2 := round("20261120-010000-claude-a.b", true, nil, q("F1", "minor", "l", "advisory"))
	r3 := round("20261120-020000-claude-a.b", false, []string{"F1"}, q("F1", "major", "l", "q3"))
	if wrote, err := WriteLedger(path, r3, r1); err != nil || !wrote {
		t.Fatal(wrote, err)
	}
	if wrote, _ := WriteLedger(path, r1); wrote {
		t.Fatal("内容不变也写了")
	}
	if _, err := WriteLedger(path, r2); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != r2.ID || got[1].ID != r3.ID || got[0].Findings != nil || len(got[1].Findings) != 1 {
		t.Fatalf("%+v", got)
	}
	b, _ := os.ReadFile(path)
	if n := strings.Count(string(b), "\n"); n != 2 {
		t.Fatalf("一行一轮: %d 行", n)
	}
}

// factory answer 写的记录(位置 / 问题编号 / 同一个问题 多行)能读回来,下一轮按它认出同一个问题。
func TestAnswerRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	design := filepath.Join(dir, "GetX.yaml")
	_ = os.WriteFile(design, []byte("名称: X\n"), 0o644)
	f1 := answer.Finding{ID: "F1", Severity: "major", Category: "边界和单位", Location: "业务规则 超时",
		Question: "恰好 300 秒算不算超时?", Options: opt("算", "不算"),
		Also: []string{"300 秒整算吗?", "边界 300 秒?"}, Refs: []string{"20261122-100000-claude-shop.get-x#F1", "20261122-090000-claude-shop.get-x#F2"}}
	f2 := answer.Finding{ID: "F2", Severity: "major", Category: "空和零", Question: "0 件时返回什么?", Options: opt("空列表", "NOT_FOUND")}
	ds, err := answer.FromArgs([]answer.Finding{f1, f2}, []string{`F1=keep:不算超时(> 300 秒才算)`, `F2=1:返回空列表,total = 0`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := answer.Write(design, ds, "20261122-100000-claude-shop.get-x"); err != nil {
		t.Fatal(err)
	}
	as, err := LoadAnswers(filepath.Join(dir, "GetX.审查.yaml"), filepath.Join(dir, "领域.审查.yaml"))
	if err != nil || len(as) != 2 {
		t.Fatalf("%+v %v", as, err)
	}
	a := as[0]
	if a.Ref != "A1" || a.Location != "业务规则 超时" || len(a.Also) != 2 || len(a.IDs) != 2 ||
		a.Answer != "保持现在的行为: 不算超时(> 300 秒才算)" || !strings.Contains(a.Rule, "不算超时(> 300 秒才算)") {
		t.Fatalf("%+v", a)
	}
	if b := as[1]; b.Answer != "空列表: 返回空列表,total = 0" || !strings.HasSuffix(b.Rule, "→ 返回空列表,total = 0") {
		t.Fatalf("%+v", b)
	}
	// 按之前一轮的编号认出(CI 的说法完全不同)
	other := Finding{Finding: answer.Finding{ID: "F7", Question: "完全不同的说法"}}
	if AnsweredBy(as, other, "20261122-090000-claude-shop.get-x#F2") != "A1" {
		t.Fatal("编号没认出来")
	}
	// 按 同一个问题 的原文认出
	if AnsweredBy(as, Finding{Finding: answer.Finding{Question: "边界 300 秒?"}}) != "A1" {
		t.Fatal("同一个问题没认出来")
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
