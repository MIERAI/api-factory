package review

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fpDomain = `领域: shop
说明: テスト
时区: Asia/Tokyo
枚举:
  OrderStatus:
    值:
      OPEN:   { 数据库值: 1 }
      CLOSED: { 数据库值: 2 }
  Color:
    值:
      RED: { 数据库值: 1 }
表:
  orders:
    说明: 注文
    主键: [id]
    列:
      id:     注文ID
      status: { 说明: 状態, 枚举: OrderStatus }
      amount: 金額
    规则:
      - { 规则: amount は 0 以上 }
  products:
    说明: 商品
    主键: [code]
    列:
      code:  商品コード
      color: { 说明: 色, 枚举: Color }
`

const fpAPI = `API: ListOrders
说明: 注文の一覧
种类: 一览
数据来源: orders
输入:
  status: { 类型: OrderStatus, 必填: 否, 筛选: status 等于 }
输出:
  id: 注文ID
`

const fpPath = "design/shop/ListOrders.yaml"

func memReader(files map[string]string) Reader {
	return func(p string) ([]byte, error) {
		s, ok := files[p]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return []byte(s), nil
	}
}

func fpFiles() map[string]string {
	return map[string]string{fpPath: fpAPI, "design/shop/领域.yaml": fpDomain}
}

func fp(t *testing.T, files map[string]string) string {
	t.Helper()
	k, err := DesignFingerprint(fpPath, memReader(files))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestFingerprintScope(t *testing.T) {
	base := fp(t, fpFiles())
	edit := func(path, old, new string) map[string]string {
		fs := fpFiles()
		if !strings.Contains(fs[path], old) {
			t.Fatalf("%q not in %s", old, path)
		}
		fs[path] = strings.Replace(fs[path], old, new, 1)
		return fs
	}
	newTable := "\n  # 新しい表(上の表の脚注にならないこと)\n  stores:\n    说明: 店舗\n    主键: [code]\n    列:\n      code: 店舗コード\n"
	same := map[string]map[string]string{
		"无关的新表(带注释)": func() map[string]string { fs := fpFiles(); fs["design/shop/领域.yaml"] += newTable; return fs }(),
		"无关的表的列":     edit("design/shop/领域.yaml", "      code:  商品コード", "      code:  商品コード\n      name:  商品名"),
		"无关的枚举":      edit("design/shop/领域.yaml", "      RED: { 数据库值: 1 }", "      RED: { 数据库值: 1 }\n      BLUE: { 数据库值: 2 }"),
		"领域的说明":      edit("design/shop/领域.yaml", "说明: テスト", "说明: テスト(改)"),
		"对齐的空格":      edit("design/shop/领域.yaml", "      id:     注文ID", "      id: 注文ID"),
		"表的顺序": func() map[string]string {
			fs := fpFiles()
			d := fs["design/shop/领域.yaml"]
			i := strings.Index(d, "  orders:")
			j := strings.Index(d, "  products:")
			fs["design/shop/领域.yaml"] = d[:i] + d[j:] + d[i:j]
			return fs
		}(),
	}
	for name, files := range same {
		if got := fp(t, files); got != base {
			t.Errorf("%s: 指纹变了(不应该重审)", name)
		}
	}
	differ := map[string]map[string]string{
		"引用的表的列":   edit("design/shop/领域.yaml", "      amount: 金額", "      amount: 金額(税込)"),
		"引用的表加列":   edit("design/shop/领域.yaml", "      amount: 金額", "      amount: 金額\n      note: 備考"),
		"引用的表的规则":  edit("design/shop/领域.yaml", "amount は 0 以上", "amount は 1 以上"),
		"引用的表的主键":  edit("design/shop/领域.yaml", "主键: [id]", "主键: [id, status]"),
		"列用到的枚举":   edit("design/shop/领域.yaml", "CLOSED: { 数据库值: 2 }", "CLOSED: { 数据库值: 2 }\n      VOID: { 数据库值: 3 }"),
		"时区":       edit("design/shop/领域.yaml", "时区: Asia/Tokyo", "时区: UTC"),
		"API 的设计书": edit(fpPath, "说明: 注文の一覧", "说明: 注文の一覧(改)"),
		"API 的审查记录": func() map[string]string {
			fs := fpFiles()
			fs["design/shop/ListOrders.审查.yaml"] = "问答: []\n"
			return fs
		}(),
		"领域的审查记录": func() map[string]string {
			fs := fpFiles()
			fs["design/shop/领域.审查.yaml"] = "问答: []\n"
			return fs
		}(),
		"新的领域级的段落(保守)": func() map[string]string {
			fs := fpFiles()
			fs["design/shop/领域.yaml"] += "关系:\n  - x\n"
			return fs
		}(),
		"领域.yaml 不存在":    func() map[string]string { fs := fpFiles(); delete(fs, "design/shop/领域.yaml"); return fs }(),
		"准备数据里用到的表 → 引用": edit(fpPath, "输出:", "例子:\n  准备数据:\n    products: []\n输出:"),
	}
	for name, files := range differ {
		if got := fp(t, files); got == base {
			t.Errorf("%s: 指纹没变(应该重审)", name)
		}
	}
	// 准备数据里引用 products 之后,products 的改动(和它的枚举)也要重审
	withProducts := edit(fpPath, "输出:", "例子:\n  准备数据:\n    products: []\n输出:")
	k1 := fp(t, withProducts)
	withProducts["design/shop/领域.yaml"] = strings.Replace(withProducts["design/shop/领域.yaml"], "RED: { 数据库值: 1 }", "RED: { 数据库值: 9 }", 1)
	if fp(t, withProducts) == k1 {
		t.Error("引用的表(准备数据)的列的枚举变了,指纹没变")
	}
}

func TestKeyModelPrompt(t *testing.T) {
	r := memReader(fpFiles())
	a, _ := Key(fpPath, r, "p1", "claude")
	b, _ := Key(fpPath, r, "p2", "claude")
	c, _ := Key(fpPath, r, "p1", "other")
	a2, _ := Key(fpPath, r, "p1", "claude")
	if a == b || a == c || a != a2 {
		t.Fatalf("%s %s %s %s", a, b, c, a2)
	}
}

func TestFingerprintNoDesign(t *testing.T) {
	_, err := DesignFingerprint("design/shop/Nope.yaml", memReader(fpFiles()))
	if !errors.Is(err, ErrNoDesign) {
		t.Fatalf("err = %v", err)
	}
}

// 仓库里真的设计书: 都能解析;在 领域.yaml 末尾加一张无关的表,任何 API 的指纹都不变
func TestFingerprintRepoDesigns(t *testing.T) {
	root := "../.."
	files, _ := filepath.Glob(filepath.Join(root, "design", "*", "*.yaml"))
	disk := func(extra string) Reader {
		return func(p string) ([]byte, error) {
			b, err := os.ReadFile(filepath.Join(root, p))
			if err == nil && filepath.Base(p) == "领域.yaml" {
				b = append(b, extra...)
			}
			return b, err
		}
	}
	n := 0
	for _, f := range files {
		base := filepath.Base(f)
		if strings.HasPrefix(base, "领域") || strings.HasSuffix(base, ".审查.yaml") || strings.HasSuffix(base, ".lock.yaml") {
			continue
		}
		rel, _ := filepath.Rel(root, f)
		a, err := DesignFingerprint(rel, disk(""))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		b, err := DesignFingerprint(rel, disk("  zz_unrelated_table:\n    主键: [id]\n    列:\n      id: ID\n"))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if a != b {
			t.Errorf("%s: 无关的新表改变了指纹", rel)
		}
		n++
	}
	if n == 0 {
		t.Fatal("没有找到设计书")
	}
}
