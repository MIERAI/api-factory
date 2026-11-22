package review

// 审查输入的指纹: "这个 API 需不需要(重新)审查"只取决于审查员判断这个 API 时真正用到的东西。
//
// 以前把整个 领域.yaml 放进指纹: 领域里加一张新表,这个领域的每个 API 都要重审。审查是不确定的,
// 已经通过、一个字没改的设计书在重审里又冒出新的挡流水线的问题(PR #25 / #26 实测)。
//
// 现在的指纹(确定性: 解析设计书,只取引用到的部分,规范化后 hash):
//   - API 自己的设计书(原文)和它的审查记录(X.审查.yaml)
//   - 领域里它引用的部分: 引用到的表(数据来源 / 准备数据 / 以后的查找・嵌套・子表聚合 … 设计书里出现的表名)
//     的整个定义(列 / 主键 / 规则 / 存储 …),这些表的列用到的枚举 + 设计书里直接出现的枚举,
//     领域名、时区(注释不算),以及 表 / 枚举 / 说明 以外的顶层键(以后新增的领域级的段落: 保守地全部算进去)
//   - 领域的审查记录(领域.审查.yaml): 回答适用于领域的全部 API,本来就要每个 API 都确认
//   - (Key 才加)审查的 prompt 模板 + 模型
//
// "引用到的表"不按键名找(查找 / 嵌套 … 由别的分支在加),而是把 API 设计书(和审查记录)里所有标量拆成标识符,
// 和领域的表名 / 枚举名求交集。多算(说明里恰好写了表名)只会多审一次,不会漏审。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Reader: 读一个文件(工作区,或某个 git 提交里的)。不存在时返回的错误要满足 errors.Is(err, fs.ErrNotExist)。
type Reader func(path string) ([]byte, error)

// ErrNoDesign: 设计书在这个版本里不存在(新的 API)。
var ErrNoDesign = errors.New("设计书不存在")

var identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// DesignInputs: 一个 API 的审查输入里来自 design/ 的部分的规范化文本(Fingerprint 的原料,测试 / 调查用)。
func DesignInputs(designPath string, read Reader) ([]byte, error) {
	api, err := read(designPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoDesign
	}
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(designPath)
	reviewPath := strings.TrimSuffix(designPath, ".yaml") + ".审查.yaml"
	apiReview, err := optional(read, reviewPath)
	if err != nil {
		return nil, err
	}
	tokens := map[string]bool{}
	for _, src := range [][]byte{api, apiReview} {
		if err := collectTokens(src, tokens); err != nil {
			return nil, fmt.Errorf("%s: %w", designPath, err)
		}
	}
	domainPath := filepath.Join(dir, "领域.yaml")
	domain, err := optional(read, domainPath)
	if err != nil {
		return nil, err
	}
	domainPart, err := domainSlice(domain, tokens)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", domainPath, err)
	}
	domainReview, err := optional(read, filepath.Join(dir, "领域.审查.yaml"))
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	section := func(name string, body []byte) {
		fmt.Fprintf(&b, "== %s\n", name)
		b.Write(body)
		if len(body) > 0 && body[len(body)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	section("api "+designPath, api)
	section("api-review", apiReview)
	section("domain "+domainPath, domainPart)
	section("domain-review", domainReview)
	return b.Bytes(), nil
}

// DesignFingerprint: 只看设计书的指纹(和 base 比较"这个 API 的设计有没有变"用,不含 prompt / 模型)。
func DesignFingerprint(designPath string, read Reader) (string, error) {
	in, err := DesignInputs(designPath, read)
	if err != nil {
		return "", err
	}
	return short(in), nil
}

// Key: 沿用上次审查结果的键 = 设计书的部分 + prompt 模板 + 模型。
func Key(designPath string, read Reader, prompt, model string) (string, error) {
	in, err := DesignInputs(designPath, read)
	if err != nil {
		return "", err
	}
	return short(append(in, []byte(fmt.Sprintf("== prompt\n%s\n== model %s\n", prompt, model))...)), nil
}

func short(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:16]
}

func optional(read Reader, path string) ([]byte, error) {
	b, err := read(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// collectTokens: YAML 里所有标量(键和值)中的标识符。
func collectTokens(src []byte, out map[string]bool) error {
	if len(bytes.TrimSpace(src)) == 0 {
		return nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal(src, &root); err != nil {
		return err
	}
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.ScalarNode {
			for _, t := range identRe.FindAllString(n.Value, -1) {
				out[t] = true
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&root)
	return nil
}

// domainSlice: 领域.yaml 里这个 API 引用到的部分,规范化(重新编码: 对齐的空格之类不影响)。
func domainSlice(src []byte, tokens map[string]bool) ([]byte, error) {
	if len(bytes.TrimSpace(src)) == 0 {
		return []byte("(不存在)\n"), nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("领域.yaml 不是映射")
	}
	stripComments(&doc) // 注释不算: yaml 会把"新表上面的注释"挂到前一张表的脚注上,新表就会改变旧表的指纹
	root := doc.Content[0]
	tz := "UTC" // 和设计编译器一样: 不写 = UTC
	out := &yaml.Node{Kind: yaml.MappingNode}
	add := func(k, v *yaml.Node) { out.Content = append(out.Content, k, v) }
	var tables, enums *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		switch k.Value {
		case "表":
			tables = v
		case "枚举":
			enums = v
		case "说明": // 领域的说明: 一般性的描述,不影响任何 API 的判断
		case "时区":
			tz = v.Value
		default: // 领域 + 以后新增的领域级的段落: 保守地算进去
			add(k, v)
		}
	}
	add(scalar("时区"), scalar(tz))

	wantEnum := map[string]bool{}
	for t := range tokens {
		wantEnum[t] = true
	}
	refTables := &yaml.Node{Kind: yaml.MappingNode}
	for _, p := range sortedPairs(tables) {
		if !tokens[p[0].Value] {
			continue
		}
		refTables.Content = append(refTables.Content, p[0], p[1])
		for _, c := range sortedPairs(mapGet(p[1], "列")) { // 列用到的枚举
			if en := mapGet(c[1], "枚举"); en != nil && en.Kind == yaml.ScalarNode {
				wantEnum[en.Value] = true
			}
		}
	}
	refEnums := &yaml.Node{Kind: yaml.MappingNode}
	for _, p := range sortedPairs(enums) {
		if wantEnum[p[0].Value] {
			refEnums.Content = append(refEnums.Content, p[0], p[1])
		}
	}
	add(scalar("表"), refTables)
	add(scalar("枚举"), refEnums)

	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	_ = enc.Close()
	return b.Bytes(), nil
}

func stripComments(n *yaml.Node) {
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	for _, c := range n.Content {
		stripComments(c)
	}
}

func scalar(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: s} }

func mapGet(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// sortedPairs: 映射的 键/值 按键排序(表 / 枚举 的书写顺序不影响)。
func sortedPairs(n *yaml.Node) [][2]*yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	var ps [][2]*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		ps = append(ps, [2]*yaml.Node{n.Content[i], n.Content[i+1]})
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i][0].Value < ps[j][0].Value })
	return ps
}
