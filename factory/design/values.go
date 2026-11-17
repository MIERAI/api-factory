package design

// 例子的值: 中文式样书的写法 → 机器用的写法。
//   时间: "2026-11-01 09:00"(领域的时区)→ "2026-11-01T00:00:00Z"
//   枚举: SUCCEEDED / 未指定 → JOB_STATUS_SUCCEEDED / JOB_STATUS_UNSPECIFIED
//   结果: 参数错误 / 找不到 / 成功 → INVALID_ARGUMENT / NOT_FOUND / OK;不设置 → $absent
//   一览的结果: [4, 3, 2] → [{id: 4}, {id: 3}, {id: 2}]
//   重复: 一行写 N 行(id: 1~19、"<时间> 起每行 +1分钟"、"<列> +60秒")

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"apifactory/factory/kinds"
	"apifactory/factory/spec"
)

var resultCodes = map[string]string{"成功": "OK", "参数错误": "INVALID_ARGUMENT", "找不到": "NOT_FOUND"}

var timeLayouts = []string{"2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"}

var (
	reIntRange = regexp.MustCompile(`^\s*(\d+)\s*~\s*(\d+)\s*$`)
	reStepTime = regexp.MustCompile(`^(.+?)\s*起每行\s*\+\s*(\d+)\s*(秒|分钟|小时|天)$`)
	reRelative = regexp.MustCompile(`^([a-z_][a-z0-9_]*)\s*\+\s*(\d+)\s*(秒|分钟|小时|天)$`)
)

var units = map[string]time.Duration{"秒": time.Second, "分钟": time.Minute, "小时": time.Hour, "天": 24 * time.Hour}

func (c *ctx) parseTime(s string) (time.Time, error) {
	for _, l := range timeLayouts {
		if t, err := time.ParseInLocation(l, strings.TrimSpace(s), c.tz); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("时间 %q 无法识别(写成 \"2026-11-01 09:00\",按领域的时区 %s 解释)", s, c.d.TZ)
}

func utc(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// value: 按种类转换一个值
func (c *ctx) value(kind string, n *yaml.Node) (any, error) {
	if n.Tag == "!!null" {
		return nil, nil
	}
	v := strings.TrimSpace(n.Value)
	if v == "不设置" {
		return "$absent", nil
	}
	switch {
	case kind == "time":
		t, err := c.parseTime(v)
		if err != nil {
			return nil, err
		}
		return utc(t), nil
	case strings.HasPrefix(kind, "enum:"):
		en := c.d.Enum(strings.TrimPrefix(kind, "enum:"))
		prefix := upperSnake(en.Name) + "_"
		if v == "未指定" {
			return prefix + "UNSPECIFIED", nil
		}
		for _, x := range en.Values {
			if x.Name == v {
				return prefix + x.Name, nil
			}
		}
		var names []string
		for _, x := range en.Values {
			names = append(names, x.Name)
		}
		return nil, fmt.Errorf("%q 不是枚举 %s 的值(可以用: %v / 未指定)", v, en.Name, names)
	default:
		if t, ok := kinds.ByValue(kind); ok && t.Parse != nil { // 类型的注册表(factory/kinds)
			return t.Parse(v)
		}
	}
	return v, nil
}

// columnKind: 表的列 → 值的种类
func (c *ctx) columnKind(table, col string) (string, error) {
	_, kind, _, err := c.columnField(table, col)
	return kind, err
}

// fixtureRows: 准备数据的一张表(展开 重复)
func (c *ctx) fixtureRows(table string, rows []*yaml.Node) ([]map[string]any, error) {
	var out []map[string]any
	for _, r := range rows {
		n := 1
		if rep := get(r, "重复"); rep != nil {
			if _, err := fmt.Sscan(rep.Value, &n); err != nil || n < 1 {
				return nil, fmt.Errorf("第 %d 行: 重复 必须是正整数", r.Line)
			}
		}
		for i := 0; i < n; i++ {
			row := map[string]any{}
			type rel struct {
				col, base string
				d         time.Duration
			}
			var rels []rel
			for _, p := range pairs(r) {
				if p.Key == "重复" {
					continue
				}
				if p.Key == "操作" { // 存储: JSON版本 的表: 这一行是删除 / 逻辑删除(不写 = 登记)
					t := c.d.Tables[table]
					if t == nil || t.Storage == nil {
						return nil, fmt.Errorf("第 %d 行: 操作 只能用于 存储: JSON版本 的表", p.Line)
					}
					switch strings.TrimSpace(p.Val.Value) {
					case "删除":
						row[spec.FixtureAction] = spec.ActionDelete
					case "逻辑删除":
						if t.Storage.LogicalDelete == nil {
							return nil, fmt.Errorf("第 %d 行: 表 %s 没有定义 逻辑删除", p.Line, table)
						}
						row[spec.FixtureAction] = spec.ActionLogicalDelete
					case "登记":
					default:
						return nil, fmt.Errorf("第 %d 行: 操作 只能是 登记 / 删除 / 逻辑删除", p.Line)
					}
					continue
				}
				kind, err := c.columnKind(table, p.Key)
				if err != nil {
					return nil, fmt.Errorf("第 %d 行: %v", p.Line, err)
				}
				v := strings.TrimSpace(p.Val.Value)
				switch {
				case n > 1 && reIntRange.MatchString(v):
					m := reIntRange.FindStringSubmatch(v)
					a, _ := strconv.ParseInt(m[1], 10, 64)
					b, _ := strconv.ParseInt(m[2], 10, 64)
					if b-a+1 != int64(n) {
						return nil, fmt.Errorf("第 %d 行: %s 的范围 %s 有 %d 个值,但 重复 是 %d", p.Line, p.Key, v, b-a+1, n)
					}
					row[p.Key] = a + int64(i)
				case reStepTime.MatchString(v):
					m := reStepTime.FindStringSubmatch(v)
					t, err := c.parseTime(m[1])
					if err != nil {
						return nil, fmt.Errorf("第 %d 行: %v", p.Line, err)
					}
					k, _ := strconv.Atoi(m[2])
					row[p.Key] = utc(t.Add(time.Duration(i*k) * units[m[3]]))
				case reRelative.MatchString(v) && kind == "time":
					m := reRelative.FindStringSubmatch(v)
					k, _ := strconv.Atoi(m[2])
					rels = append(rels, rel{p.Key, m[1], time.Duration(k) * units[m[3]]})
				default:
					x, err := c.value(kind, p.Val)
					if err != nil {
						return nil, fmt.Errorf("第 %d 行 %s: %v", p.Line, p.Key, err)
					}
					row[p.Key] = x
				}
			}
			for _, x := range rels {
				base, ok := row[x.base].(string)
				if !ok {
					return nil, fmt.Errorf("第 %d 行: %s 引用的列 %s 没有时间值", r.Line, x.col, x.base)
				}
				t, _ := time.Parse(time.RFC3339Nano, base)
				row[x.col] = utc(t.Add(x.d))
			}
			out = append(out, row)
		}
	}
	return out, nil
}

func fieldOf(m *message, name string) *field {
	for _, f := range m.Fields {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// request: 例子的输入 → 请求
func (c *ctx) request(req *message, n *yaml.Node) (map[string]any, error) {
	out := map[string]any{}
	for _, p := range pairs(n) {
		f := fieldOf(req, p.Key)
		if f == nil {
			return nil, fmt.Errorf("第 %d 行: %s 不是输入", p.Line, p.Key)
		}
		if f.Repeated && strings.HasPrefix(f.kind, "message:") { // 组的列表: [{ 字段: 值, … }, …]
			sub := c.msgByNm[strings.TrimPrefix(f.kind, "message:")]
			list := []any{}
			for _, it := range p.Val.Content {
				m, err := c.request(sub, it)
				if err != nil {
					return nil, fmt.Errorf("第 %d 行 %s: %v", it.Line, p.Key, err)
				}
				list = append(list, m)
			}
			out[p.Key] = list
			continue
		}
		if f.Repeated { // 列表输入: [a, b]
			list := []any{}
			for _, it := range p.Val.Content {
				v, err := c.value(f.kind, it)
				if err != nil {
					return nil, fmt.Errorf("第 %d 行 %s: %v", it.Line, p.Key, err)
				}
				list = append(list, v)
			}
			out[p.Key] = list
			continue
		}
		v, err := c.value(f.kind, p.Val)
		if err != nil {
			return nil, fmt.Errorf("第 %d 行 %s: %v", p.Line, p.Key, err)
		}
		out[p.Key] = v
	}
	return out, nil
}

// expect: 例子的结果 → 期待值
func (c *ctx) expect(resp *message, itemKey string, n *yaml.Node) (spec.Expect, error) {
	if n == nil {
		return spec.Expect{}, fmt.Errorf("缺少 结果")
	}
	if n.Kind == yaml.ScalarNode {
		code, ok := resultCodes[strings.TrimSpace(n.Value)]
		if !ok {
			return spec.Expect{}, fmt.Errorf("第 %d 行: 结果 %q 无法识别(成功 / 参数错误 / 找不到,或者写出输出的值)", n.Line, n.Value)
		}
		return spec.Expect{Code: code}, nil
	}
	resp2 := map[string]any{}
	for _, p := range pairs(n) {
		f := fieldOf(resp, p.Key)
		if f == nil {
			return spec.Expect{}, fmt.Errorf("第 %d 行: %s 不是输出", p.Line, p.Key)
		}
		if f.Repeated && strings.HasPrefix(f.kind, "message:") {
			item := c.msgByNm[strings.TrimPrefix(f.kind, "message:")]
			list := []any{}
			for _, it := range p.Val.Content {
				m := map[string]any{}
				if it.Kind == yaml.ScalarNode { // [4, 3, 2] = 只写排序的最后一列(唯一)
					kf := fieldOf(item, itemKey)
					v, err := c.value(kf.kind, it)
					if err != nil {
						return spec.Expect{}, fmt.Errorf("第 %d 行: %v", it.Line, err)
					}
					m[itemKey] = v
				} else {
					for _, q := range pairs(it) {
						qf := fieldOf(item, q.Key)
						if qf == nil {
							return spec.Expect{}, fmt.Errorf("第 %d 行: %s 不是 %s 的字段", q.Line, q.Key, item.Name)
						}
						v, err := c.value(qf.kind, q.Val)
						if err != nil {
							return spec.Expect{}, fmt.Errorf("第 %d 行 %s: %v", q.Line, q.Key, err)
						}
						m[q.Key] = v
					}
				}
				list = append(list, m)
			}
			resp2[p.Key] = list
			continue
		}
		v, err := c.value(f.kind, p.Val)
		if err != nil {
			return spec.Expect{}, fmt.Errorf("第 %d 行 %s: %v", p.Line, p.Key, err)
		}
		resp2[p.Key] = v
	}
	return spec.Expect{Code: "OK", Response: resp2}, nil
}

// examples: 例子 / 计算例子 → acceptance / logic_examples
func (c *ctx) examples(a *API, s *spec.Spec, req, resp *message, itemKey string) error {
	wrap := func(err error) error { return fmt.Errorf("%s: 例子: %w", a.Path, err) }
	if a.Fixtures != nil {
		s.Acceptance.Fixtures = map[string][]map[string]any{}
		for _, p := range pairs(a.Fixtures) {
			rows, err := c.fixtureRows(p.Key, p.Val.Content)
			if err != nil {
				return wrap(fmt.Errorf("准备数据 %s: %w", p.Key, err))
			}
			s.Acceptance.Fixtures[p.Key] = rows
		}
	}
	for _, n := range a.Cases {
		r := map[string]any{}
		if in := get(n, "输入"); in != nil {
			var err error
			if r, err = c.request(req, in); err != nil {
				return wrap(err)
			}
		}
		ex, err := c.expect(resp, itemKey, get(n, "结果"))
		if err != nil {
			return wrap(err)
		}
		s.Acceptance.Cases = append(s.Acceptance.Cases, spec.Case{Name: str(n, "说明"), Request: r, Expect: ex})
	}
	for _, n := range a.LogicCases {
		le := spec.LogicExample{Name: str(n, "说明")}
		if in := get(n, "输入"); in != nil {
			r, err := c.request(req, in)
			if err != nil {
				return wrap(err)
			}
			le.Request = r
		}
		for _, row := range get(n, "输入行").Content {
			m := map[string]any{}
			for _, p := range pairs(row) {
				if p.Key == "重复" {
					var k int
					fmt.Sscan(p.Val.Value, &k)
					m["count"] = k
					continue
				}
				kind, err := c.columnKind(a.Source, p.Key)
				if err != nil {
					return wrap(fmt.Errorf("计算例子 第 %d 行: %v", p.Line, err))
				}
				v, err := c.value(kind, p.Val)
				if err != nil {
					return wrap(fmt.Errorf("计算例子 第 %d 行 %s: %v", p.Line, p.Key, err))
				}
				m[p.Key] = v
			}
			le.Rows = append(le.Rows, m)
		}
		ex, err := c.expect(resp, itemKey, get(n, "结果"))
		if err != nil {
			return wrap(err)
		}
		le.Expect = ex.Response
		s.LogicExamples = append(s.LogicExamples, le)
	}
	return nil
}
