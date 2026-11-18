package answer

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 已经直接改了设计书: 记录问题原文(下一轮据此判断"已回答"),不写补充规则,也不改设计稿。
func TestAnswerByDesign(t *testing.T) {
	fs := []Finding{
		{ID: "F1", Severity: "major", Category: "矛盾", Question: "上限 62 和背景矛盾?"},
		{ID: "F2", Severity: "major", Category: "空值", Question: "折扣为空时排在哪?"},
	}
	ds, err := FromArgs(fs, []string{"F1=@design", "F2=@design:加了 折扣额 不为空"})
	if err != nil || len(ds) != 2 {
		t.Fatalf("ds=%v err=%v", ds, err)
	}
	dir := t.TempDir()
	design := filepath.Join(dir, "X.yaml")
	if err := os.WriteFile(design, []byte("名称: X\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	touched, err := Write(design, ds, filepath.Join(dir, "r1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(touched) != 1 || !strings.HasSuffix(touched[0], "X.审查.yaml") {
		t.Fatalf("touched=%v (设计稿不应被改)", touched)
	}
	b, _ := os.ReadFile(touched[0])
	s := string(b)
	for _, want := range []string{strconv.Quote(fs[0].Question), strconv.Quote(fs[1].Question),
		`改了设计: "见这次的设计书改动"`, `改了设计: "加了 折扣额 不为空"`, `回答: "已经直接修改了设计书"`} {
		if !strings.Contains(s, want) {
			t.Errorf("缺少 %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, "补充规则") {
		t.Errorf("不应写补充规则:\n%s", s)
	}
	// "@designer" 不是这个模式,是自己写的回答
	ds, _ = FromArgs(fs, []string{"F1=@designer"})
	if ds[0].ByDesign != "" {
		t.Errorf("@designer 被当成了 @design")
	}
}
