package design

// 消息契约(R4,docs/design/r4-events-messages.md §2):
//   - 每个服务的 design/<领域>/消息.yaml 声明它拥有的契约(事件 = 发送方,命令 = 接收方)
//   - 工厂把拥有的契约写成 <contracts>/<type>.v<N>.json(JSON Schema 2020-12: envelope + data),放在系统级的共享目录
//     (factory.yaml 的 contracts:,默认 contracts/;storefront 样例: ../contracts)。别的服务从这里读(x-factory = 字段的声明)
//   - 版本规则(DRC + 锁文件 <contracts>/contracts.lock.yaml): 同一个版本里只能新增可选字段;
//     删字段、改类型、可选改必填 → 编译失败(发布新版本 vN+1)
//
//	消息:
//	  inventory.reserved:
//	    种类: 事件              # 事件 / 命令
//	    版本: 1
//	    主体: order_id          # envelope.subject 取哪个字段
//	    说明: 预留成功
//	    字段:
//	      order_id: { 类型: 整数64, 必填: 是, 说明: 订单号 }
//	      items:    { 类型: 列表, 必填: 是, 个数: 1~50, 唯一: [sku], 元素: { sku: { 类型: 文本, 长度: 3~32 }, qty: { 类型: 整数32, 范围: 1~1000 } } }

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/MIERAI/api-factory-runtime/outbox"

	"apifactory/factory/constraints"
	"apifactory/factory/core"
	"apifactory/factory/kinds"
	"apifactory/factory/spec"
)

// ContractsFile: 领域里声明消息契约的文件。ListType: 契约字段的列表类型(元素 = 平铺的字段)。
const (
	ContractsFile = "消息.yaml"
	ListType      = "列表"
	lockFile      = "contracts.lock.yaml"
)

var contractKinds = map[string]string{"事件": "event", "命令": "command"}

// contractSet: 这个领域能用的契约(自己拥有的 + 共享目录里别人的)。
type contractSet struct {
	dir   string                    // 共享目录
	own   []*spec.Contract          // 这个领域拥有的(按类型名排序)
	byTyp map[string]*spec.Contract // 类型 → 契约
}

// loadContracts: dir = design/<领域>。
func loadContracts(dir string, cfg Config) (*contractSet, error) {
	cs := &contractSet{dir: cfg.Contracts, byTyp: map[string]*spec.Contract{}}
	files, _ := filepath.Glob(filepath.Join(cs.dir, "*.json"))
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var doc struct {
			X *spec.Contract `json:"x-factory"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if doc.X == nil {
			return nil, fmt.Errorf("%s: 没有 x-factory(不是工厂生成的契约)", f)
		}
		if prev := cs.byTyp[doc.X.Type]; prev == nil || prev.Version < doc.X.Version {
			cs.byTyp[doc.X.Type] = doc.X
		}
	}
	path := filepath.Join(dir, ContractsFile)
	if _, err := os.Stat(path); err != nil {
		return cs, nil
	}
	root, err := readYAML(path)
	if err != nil {
		return nil, err
	}
	e := &errs{file: path}
	checkKeys(e, root, "消息.yaml", "消息")
	for _, p := range pairs(get(root, "消息")) {
		checkKeys(e, p.Val, "消息 "+p.Key, "种类", "版本", "主体", "说明", "字段")
		c := &spec.Contract{Type: p.Key, Kind: contractKinds[str(p.Val, "种类")], Subject: str(p.Val, "主体"), Desc: str(p.Val, "说明")}
		if c.Kind == "" {
			e.add(p.Line, "消息 %s: 种类 只能是 事件 / 命令", p.Key)
		}
		if _, err := fmt.Sscan(str(p.Val, "版本"), &c.Version); err != nil || c.Version < 1 {
			e.add(p.Line, "消息 %s: 版本 写正整数", p.Key)
		}
		if !validType(p.Key) {
			e.add(p.Line, "消息 %s: 类型名写成 小写字母 / 数字 / _ 用 . 分隔(例: inventory.reserved)", p.Key)
		}
		c.Fields = contractFields(e, "消息 "+p.Key, get(p.Val, "字段"), false)
		var subj *spec.ContractField
		for i := range c.Fields {
			if c.Fields[i].Name == c.Subject {
				subj = &c.Fields[i]
			}
		}
		switch {
		case subj == nil:
			e.add(p.Line, "消息 %s: 主体 %q 不是字段", p.Key, c.Subject)
		case subj.Type == ListType || !subj.Required:
			e.add(p.Line, "消息 %s: 主体 %s 必须是必填的单值字段", p.Key, c.Subject)
		}
		cs.own = append(cs.own, c)
		cs.byTyp[c.Type] = c
	}
	if err := e.err(); err != nil {
		return nil, err
	}
	sort.Slice(cs.own, func(i, j int) bool { return cs.own[i].Type < cs.own[j].Type })
	return cs, nil
}

func validType(s string) bool {
	if s == "" || strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") || strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func contractFields(e *errs, where string, n *yaml.Node, elem bool) []spec.ContractField {
	var out []spec.ContractField
	for _, p := range pairs(n) {
		keys := append([]string{"类型", "必填", "说明", "元素", "唯一"}, constraints.Keys()...)
		checkKeys(e, p.Val, where+" 的字段 "+p.Key, keys...)
		f := spec.ContractField{Name: p.Key, Type: str(p.Val, "类型"), Required: elem || str(p.Val, "必填") == "是", Desc: str(p.Val, "说明")}
		if r := str(p.Val, "必填"); r != "" && r != "是" && r != "否" {
			e.add(p.Line, "%s 的字段 %s: 必填 只能是 是 / 否", where, p.Key)
		}
		for _, k := range constraints.Keys() {
			if v := str(p.Val, k); v != "" {
				if f.Rules == nil {
					f.Rules = map[string]string{}
				}
				f.Rules[k] = v
			}
		}
		switch {
		case f.Type == ListType:
			if elem {
				e.add(p.Line, "%s 的字段 %s: 列表的元素里不能再有列表", where, p.Key)
			}
			if f.Rules["个数"] == "" {
				e.add(p.Line, "%s 的字段 %s: 列表必须写 个数(上限,例: 1~50)", where, p.Key)
			}
			f.Elem = contractFields(e, where+" 的字段 "+p.Key+" 的元素", get(p.Val, "元素"), true)
			if len(f.Elem) < 2 {
				e.add(p.Line, "%s 的字段 %s: 元素至少 2 个字段", where, p.Key)
			}
			f.Unique = strs(e, get(p.Val, "唯一"), where+" 的字段 "+p.Key+" 的唯一")
			for _, u := range f.Unique {
				ok := false
				for _, x := range f.Elem {
					ok = ok || x.Name == u
				}
				if !ok {
					e.add(p.Line, "%s 的字段 %s: 唯一 %s 不是元素的字段", where, p.Key, u)
				}
			}
		default:
			k, ok := kinds.ByName(f.Type)
			if !ok || k.Core == "" {
				e.add(p.Line, "%s 的字段 %s: 类型 %q 不能用于消息(%s / 列表)", where, p.Key, f.Type, kinds.Names())
			}
			if get(p.Val, "元素") != nil || get(p.Val, "唯一") != nil {
				e.add(p.Line, "%s 的字段 %s: 元素 / 唯一 只能用于 列表", where, p.Key)
			}
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		e.add(1, "%s: 缺少 字段", where)
	}
	return out
}

// inputsOf: 契约的字段 → 设计稿的输入(请求 message 的生成 / 例子的值都走输入的路径)。列表 = 组列表。
func inputsOf(fields []spec.ContractField) []*Input {
	var out []*Input
	for _, f := range fields {
		in := &Input{Name: f.Name, Type: f.Type, Required: f.Required, Desc: f.Desc, Constraints: map[string]string{}, Line: 1}
		for k, v := range f.Rules {
			in.Constraints[k] = v
		}
		if f.Type == ListType {
			in.Type = GroupListType
			in.Group = inputsOf(f.Elem)
		}
		out = append(out, in)
	}
	return out
}

// protoName: 发出的消息的 data 的 proto 消息名(inventory.reserved v1 → InventoryReservedV1)。
func protoName(c *spec.Contract) string {
	var b strings.Builder
	for _, part := range strings.FieldsFunc(c.Type, func(r rune) bool { return r == '.' || r == '_' }) {
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return fmt.Sprintf("%sV%d", b.String(), c.Version)
}

// ---------------------------------------------------------------------------
// JSON Schema + 锁文件
// ---------------------------------------------------------------------------

// writeContracts: 拥有的契约 → <dir>/<type>.v<N>.json;锁文件检查版本规则并更新。
func (cs *contractSet) write() ([]string, error) {
	if len(cs.own) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(cs.dir, 0o755); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(cs.dir, lockFile)
	lock := map[string][]lockField{}
	if b, err := os.ReadFile(lockPath); err == nil {
		if err := yaml.Unmarshal(b, &lock); err != nil {
			return nil, fmt.Errorf("%s: %w", lockPath, err)
		}
	}
	var written []string
	for _, c := range cs.own {
		key := fmt.Sprintf("%s.v%d", c.Type, c.Version)
		cur := lockFields(c.Fields)
		if old, ok := lock[key]; ok {
			if msg := compatible(old, cur); msg != "" {
				return nil, fmt.Errorf("消息 %s v%d: %s —— 同一个版本里只能新增可选字段;请发布新版本 v%d(%s)", c.Type, c.Version, msg, c.Version+1, lockPath)
			}
		}
		lock[key] = cur
		path := filepath.Join(cs.dir, key+".json")
		b, err := json.MarshalIndent(schemaOf(c), "", "  ")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			return nil, err
		}
		written = append(written, path)
	}
	b, err := yaml.Marshal(lock)
	if err != nil {
		return nil, err
	}
	head := "# Code generated by factory: 消息契约的锁文件(版本规则: 同一个版本里只能新增可选字段)。不要手改\n"
	if err := os.WriteFile(lockPath, append([]byte(head), b...), 0o644); err != nil {
		return nil, err
	}
	return append(written, lockPath), nil
}

type lockField struct {
	Name     string      `yaml:"name"`
	Type     string      `yaml:"type"`
	Required bool        `yaml:"required,omitempty"`
	Elem     []lockField `yaml:"elem,omitempty"`
}

func lockFields(fs []spec.ContractField) []lockField {
	var out []lockField
	for _, f := range fs {
		out = append(out, lockField{Name: f.Name, Type: f.Type, Required: f.Required, Elem: lockFields(f.Elem)})
	}
	return out
}

// compatible: 旧的字段 → 新的字段是否只新增了可选字段("" = 兼容)。
func compatible(old, cur []lockField) string {
	byName := map[string]lockField{}
	for _, f := range cur {
		byName[f.Name] = f
	}
	seen := map[string]bool{}
	for _, o := range old {
		n, ok := byName[o.Name]
		seen[o.Name] = true
		switch {
		case !ok:
			return "删除了字段 " + o.Name
		case n.Type != o.Type:
			return fmt.Sprintf("字段 %s 的类型 %s → %s", o.Name, o.Type, n.Type)
		case n.Required && !o.Required:
			return "字段 " + o.Name + " 从可选改成了必填"
		case !n.Required && o.Required:
			return "字段 " + o.Name + " 从必填改成了可选(老的消费者依赖它)"
		}
		if msg := compatible(o.Elem, n.Elem); msg != "" {
			return "字段 " + o.Name + " 的元素: " + msg
		}
	}
	for _, f := range cur {
		if !seen[f.Name] && f.Required {
			return "新增的字段 " + f.Name + " 是必填(老的发送方不会发)"
		}
	}
	return ""
}

func schemaOf(c *spec.Contract) map[string]any {
	data := objectSchema(c.Fields)
	return map[string]any{
		"$schema":     "https://json-schema.org/draft/2020-12/schema",
		"$id":         fmt.Sprintf("%s.v%d.json", c.Type, c.Version),
		"title":       c.Type,
		"description": fmt.Sprintf("%s(%s,版本 %d,主体 = data.%s)。Code generated by factory from 消息.yaml", c.Desc, map[string]string{"event": "事件", "command": "命令"}[c.Kind], c.Version, c.Subject),
		"type":        "object",
		"required":    []string{"id", "type", "version", "occurred_at", "source", "subject", "data"},
		"properties": map[string]any{
			"id":          map[string]any{"type": "string", "format": "uuid", "description": "UUIDv7(平台生成;重发时不变)"},
			"type":        map[string]any{"const": c.Type},
			"version":     map[string]any{"const": c.Version},
			"occurred_at": map[string]any{"type": "string", "format": "date-time"},
			"source":      map[string]any{"type": "string"},
			"subject":     map[string]any{"type": "string"},
			"traceparent": map[string]any{"type": "string"},
			"data":        data,
		},
		"x-factory": c, // Proto 不写(json:"-")
	}
}

func objectSchema(fs []spec.ContractField) map[string]any {
	props := map[string]any{}
	var req []string
	for _, f := range fs {
		if f.Required {
			req = append(req, f.Name)
		}
		if f.Type == ListType {
			s := map[string]any{"type": "array", "items": objectSchema(f.Elem)}
			if lo, hi, ok := strings.Cut(f.Rules["个数"], "~"); ok {
				var a, b int
				fmt.Sscan(lo, &a)
				fmt.Sscan(hi, &b)
				s["minItems"], s["maxItems"] = a, b
			}
			if f.Desc != "" {
				s["description"] = f.Desc
			}
			props[f.Name] = s
			continue
		}
		s := map[string]any{}
		k, _ := kinds.ByName(f.Type)
		switch {
		case k == nil:
		case k.Core == core.Int32:
			s["type"] = "integer"
		case k.Core == core.Int64: // protojson: 64 位整数是字符串
			s["type"], s["pattern"] = "string", "^-?[0-9]+$"
		case k.Core == core.Bool:
			s["type"] = "boolean"
		case k.Core == core.Timestamp:
			s["type"], s["format"] = "string", "date-time"
		case k.Core == core.Date:
			s["type"], s["format"] = "string", "date"
		default:
			s["type"] = "string"
			if k.Pattern != "" {
				s["pattern"] = k.Pattern
			}
		}
		if f.Desc != "" {
			s["description"] = f.Desc
		}
		props[f.Name] = s
	}
	out := map[string]any{"type": "object", "properties": props}
	if len(req) > 0 {
		out["required"] = req
	}
	return out
}

// writePlatformSQL: 运行时的平台表(outbox.PlatformSQL)原样写进 db/schema/zz_factory_platform.sql。
func writePlatformSQL() (string, error) {
	path := filepath.Join("db", "schema", outbox.PlatformFile)
	if b, err := os.ReadFile(path); err == nil && string(b) == outbox.PlatformSQL {
		return path, nil
	}
	return path, os.WriteFile(path, []byte(outbox.PlatformSQL), 0o644)
}
