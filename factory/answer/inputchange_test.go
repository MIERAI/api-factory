package answer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const designSample = `API: X
输入:                    # 注释保留
  division_code: { 类型: 整数32, 必填: 否, 说明: 区分コード, 筛选: division_code 等于 }   # 行尾注释
  codes:   { 类型: 文本列表, 必填: 否, 范围: "> 0", 说明: "a: b" }
输出:
  x: y
`

func TestApplyInputChanges(t *testing.T) {
	p := filepath.Join(t.TempDir(), "X.yaml")
	if err := os.WriteFile(p, []byte(designSample), 0o644); err != nil {
		t.Fatal(err)
	}
	err := ApplyInputChanges(p, []InputChange{
		{Input: "division_code", Key: "必填", Value: "是"},
		{Input: "division_code", Key: "范围", Value: ">= 1"},
		{Input: "codes", Key: "个数", Value: "0~1000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	got := string(b)
	for _, want := range []string{
		`  division_code: { 类型: 整数32, 必填: "是", 范围: ">= 1", 说明: 区分コード, 筛选: division_code 等于 }   # 行尾注释`,
		`  codes:   { 类型: 文本列表, 必填: 否, 个数: "0~1000", 范围: "> 0", 说明: "a: b" }`,
		`输入:                    # 注释保留`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("没有找到\n%s\n---\n%s", want, got)
		}
	}
}

func TestApplyInputChangesRejects(t *testing.T) {
	p := filepath.Join(t.TempDir(), "X.yaml")
	_ = os.WriteFile(p, []byte(designSample), 0o644)
	for _, c := range []InputChange{
		{Input: "division_code", Key: "筛选", Value: "x"},     // 不能自动改的键
		{Input: "division_code", Key: "范围", Value: "正数"},    // 写法不对
		{Input: "nope", Key: "必填", Value: "是"},              // 没有这个输入
		{Input: "division_code", Key: "个数", Value: "1~2~3"}, // 写法不对
	} {
		if err := ApplyInputChanges(p, []InputChange{c}); err == nil {
			t.Errorf("%v: 应该拒绝", c)
		}
	}
	b, _ := os.ReadFile(p)
	if string(b) != designSample {
		t.Error("拒绝时不能改文件")
	}
}

func TestApplyRuleAdditions(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "X.yaml")
	_ = os.WriteFile(p, []byte(designSample), 0o644)
	if err := ApplyRuleAdditions(p, []string{"division_code 等于 codes"}); err == nil {
		t.Error("不支持的写法应该拒绝")
	}
	if err := ApplyRuleAdditions(p, []string{"division_code、codes 至少指定一个"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "  codes:   { 类型: 文本列表, 必填: 否, 范围: \"> 0\", 说明: \"a: b\" }\n输入之间的规则:\n  - division_code、codes 至少指定一个\n\n输出:") {
		t.Fatalf("新的一节的位置不对:\n%s", b)
	}
	if err := ApplyRuleAdditions(p, []string{"division_code 早于 codes"}); err != nil { // 已有这一节: 追加
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if !strings.Contains(string(b), "  - division_code、codes 至少指定一个\n  - division_code 早于 codes\n\n输出:") {
		t.Fatalf("追加的位置不对:\n%s", b)
	}
}

func TestApplyKeySets(t *testing.T) {
	p := filepath.Join(t.TempDir(), "X.yaml")
	src := "API: X\n种类: 多条\n上限: 5000   \n排序: [a 小到大]\n输入:\n  a: { 类型: 文本, 必填: 否 }\n"
	_ = os.WriteFile(p, []byte(src), 0o644)
	if err := ApplyKeySets(p, []KeySet{{"上限", "10000"}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "\n上限: 10000\n排序: [a 小到大]") {
		t.Fatalf("替换不对:\n%s", b)
	}
	if err := ApplyKeySets(p, []KeySet{{"条数", "3"}}); err != nil { // 没有这一行: 加在 种类 后面
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if !strings.Contains(string(b), "种类: 多条\n条数: 3\n") {
		t.Fatalf("新加的位置不对:\n%s", b)
	}
	for _, k := range []KeySet{{"API", "Y"}, {"上限", "很多"}, {"上限", "-1"}, {"排序", "x"}} {
		before, _ := os.ReadFile(p)
		if err := ApplyKeySets(p, []KeySet{k}); err == nil {
			t.Errorf("%v: 应该拒绝", k)
		}
		after, _ := os.ReadFile(p)
		if string(before) != string(after) {
			t.Errorf("%v: 拒绝时不能改文件", k)
		}
	}
}
