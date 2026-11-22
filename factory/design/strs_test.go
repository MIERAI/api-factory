package design

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// 列表的键(固定条件 / 条件 / 排序 …)写成一个字符串 = 一项的列表;别的形状报错,不能悄悄丢掉。
// 以前顶层的 固定条件 只认列表: 写成一个字符串时条件没了,查询照样编译通过(H5)。
func TestListKeysAcceptStringOrList(t *testing.T) {
	const head = "API: ListX\n说明: x\n负责人: x\n风险: 低\n种类: 多条\n数据来源: t\n排序: [id 小到大]\n上限: 10\n输出:\n  id: 编号\n"
	load := func(body string) (*API, error) {
		p := filepath.Join(t.TempDir(), "ListX.yaml")
		if err := os.WriteFile(p, []byte(head+body), 0o644); err != nil {
			t.Fatal(err)
		}
		return loadAPI(p)
	}
	for _, c := range []struct {
		body string
		want []string
	}{
		{"固定条件: status = 1\n", []string{"status = 1"}},
		{"固定条件: [status = 1, qty > 0]\n", []string{"status = 1", "qty > 0"}},
		{"固定条件:\n  - status = 1\n", []string{"status = 1"}},
		{"固定条件:\n", nil},
	} {
		a, err := load(c.body)
		if err != nil {
			t.Errorf("%q: %v", c.body, err)
			continue
		}
		if !reflect.DeepEqual(a.Where, c.want) {
			t.Errorf("%q: 固定条件 = %q,期待 %q", c.body, a.Where, c.want)
		}
	}
	for _, body := range []string{
		"固定条件: { status: 1 }\n",
		"固定条件:\n  - status: 1\n",
		"固定条件: [[status = 1]]\n",
		"嵌套:\n  items: { 表: t2, 条件: [id = pid], 固定条件: { a: 1 }, 排序: [id 小到大], 上限: 5, 列: [id] }\n",
		"子聚合:\n  n: { 表: t2, 条件: { id: pid }, 计算: 件数 }\n",
	} {
		_, err := load(body)
		if err == nil || !strings.Contains(err.Error(), "要写成一个字符串") && !strings.Contains(err.Error(), "每一项要写成一个字符串") {
			t.Errorf("%q: 应该报形状的错,得到 %v", body, err)
		}
	}
}
