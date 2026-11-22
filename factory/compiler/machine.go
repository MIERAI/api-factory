package compiler

// R5 的 IR: 状态机(docs/design/r5-state-machines.md)。状态的数据库值来自领域层的枚举;表达式按 spec 的规范写法重新解析、
// 加类型(和设计编译器同一套规则);每个收到的消息类型一个处理器,迟到集合由状态图算出(先于当前状态 = 能到达当前状态)。

import (
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"apifactory/factory/command"
	"apifactory/factory/ir"
	"apifactory/factory/spec"
)

const entityRef, loadRef = "实体", "载入"

func machineIR(s *spec.Spec, api *ir.API, cat *catalog, md protoreflect.MethodDescriptor) error {
	sm := s.Pattern.Command.Machine
	if sm == nil {
		return fmt.Errorf("machine: no machine")
	}
	if s.Domain == nil {
		return fmt.Errorf("machine: no domain")
	}
	ent, ok := s.Domain.Entities[sm.Entity]
	if !ok || len(ent.NaturalKey) != 1 {
		return fmt.Errorf("machine: entity %s: not a table with a one-column key", sm.Entity)
	}
	col := func(table, name string) (ir.Column, error) {
		c, ok := cat.column(table, name)
		if !ok {
			return ir.Column{}, fmt.Errorf("machine: no column %s.%s", table, name)
		}
		return ir.Column{Name: c.Name, Type: c.Type, NotNull: c.NotNull}, nil
	}
	m := &ir.Machine{Name: api.Method, Table: sm.Entity, Entered: sm.Entered, Version: sm.Version, Attempts: sm.Attempts,
		Created: sm.Created, Start: sm.Start}
	var err error
	if m.Key, err = col(sm.Entity, ent.NaturalKey[0]); err != nil {
		return err
	}
	if m.State, err = col(sm.Entity, sm.State); err != nil {
		return err
	}
	meta := ent.Columns[sm.State].Enum
	if meta == nil {
		return fmt.Errorf("machine: state column %s.%s is not an enum", sm.Entity, sm.State)
	}
	enumName := meta.Proto[strings.LastIndex(meta.Proto, ".")+1:]
	prefix := upperSnakeName(enumName) + "_"
	dbOf := map[string]int64{}
	for db, name := range meta.Values {
		dbOf[strings.TrimPrefix(name, prefix)] = db
	}

	// 契约(收到的 + 发出的): data 的 proto 消息
	contracts := map[string]spec.Contract{}
	for _, ct := range s.Pattern.Command.Contracts {
		contracts[ct.Type] = ct
		pm := md.ParentFile().Messages().ByName(protoreflect.Name(ct.Proto))
		if pm == nil {
			return fmt.Errorf("contract %s: proto message %s not found", ct.Type, ct.Proto)
		}
		m.Contracts = append(m.Contracts, ir.Contract{Type: ct.Type, Kind: ct.Kind, Version: ct.Version, Subject: ct.Subject, Message: convMessage(pm, 0)})
	}
	sort.Slice(m.Contracts, func(i, j int) bool { return m.Contracts[i].Type < m.Contracts[j].Type })

	used := map[string]bool{m.Key.Name: true, m.State.Name: true, m.Entered: true, m.Version: true, m.Attempts: true}
	if m.Created != "" {
		used[m.Created] = true
	}
	scope := func(msg *ir.Message) command.Scope {
		return command.Scope{
			Ref: func(ref, c string) (string, error) {
				if ref != entityRef {
					return "", fmt.Errorf("%s.%s: only 实体.x", ref, c)
				}
				x, err := col(sm.Entity, c)
				used[c] = err == nil
				return x.Type, err
			},
			Input: func(name string) (string, error) {
				if msg == nil {
					return "", fmt.Errorf("消息.%s: no message here", name)
				}
				f := msg.Field(name)
				if f == nil {
					return "", fmt.Errorf("消息.%s: no field %s in %s", name, name, msg.Name)
				}
				return InputSQLType(f)
			},
			RefEnum: func(ref, c string) (string, bool) {
				return sm.Entity, ref == entityRef && ent.Columns[c].Enum != nil
			},
			EnumOf: func(table, c string) bool { return s.Domain.Entities[table].Columns[c].Enum != nil },
		}
	}
	msgOf := func(typ string) *ir.Message {
		if c := m.Contract(typ); c != nil {
			return &c.Message
		}
		return nil
	}
	emits := func(list []spec.Emit, msg *ir.Message) ([]ir.CommandEmit, error) {
		var out []ir.CommandEmit
		for _, e := range list {
			ct, ok := contracts[e.Type]
			if !ok {
				return nil, fmt.Errorf("emit %s: no contract", e.Type)
			}
			em := ir.CommandEmit{Type: e.Type}
			for _, v := range e.Values {
				var cf *spec.ContractField
				for i := range ct.Fields {
					if ct.Fields[i].Name == v.Column {
						cf = &ct.Fields[i]
					}
				}
				if cf == nil {
					return nil, fmt.Errorf("emit %s: no field %s", e.Type, v.Column)
				}
				tree, err := command.ParseExpr(v.Expr, false)
				if err != nil {
					return nil, fmt.Errorf("emit %s.%s: %w", e.Type, v.Column, err)
				}
				if cf.Elem != nil { // 列表: 载入的子行
					if tree.Op != "ref" || tree.Ref != loadRef {
						return nil, fmt.Errorf("emit %s.%s: a list field takes 载入.<name>", e.Type, v.Column)
					}
					var elem []string
					for _, f := range cf.Elem {
						elem = append(elem, f.Name)
					}
					em.Values = append(em.Values, ir.EmitVal{Field: v.Column, List: true, Expr: tree, Elem: elem})
					continue
				}
				want, err := sqlOf(cf.Type)
				if err != nil {
					return nil, fmt.Errorf("emit %s.%s: %w", e.Type, v.Column, err)
				}
				if err := command.Resolve(tree, scope(msg), want, "", ""); err != nil {
					return nil, fmt.Errorf("emit %s.%s = %s: %w", e.Type, v.Column, v.Expr, err)
				}
				em.Values = append(em.Values, ir.EmitVal{Field: v.Column, Type: want, Expr: tree})
			}
			out = append(out, em)
		}
		return out, nil
	}
	for _, st := range sm.States {
		db, ok := dbOf[st.Name]
		if !ok {
			return fmt.Errorf("machine: state %s is not a value of %s", st.Name, meta.Proto)
		}
		x := ir.MachineState{Name: st.Name, DB: db, Final: st.Final, Timeout: st.Timeout, Every: st.Every, Deadline: st.Deadline, DeadlineTo: st.DeadlineTo}
		if st.Command != nil {
			cmds, err := emits([]spec.Emit{*st.Command}, nil)
			if err != nil {
				return fmt.Errorf("state %s: %w", st.Name, err)
			}
			x.Command = &cmds[0]
		}
		if x.DeadlineEmits, err = emits(st.DeadlineEmit, nil); err != nil {
			return fmt.Errorf("state %s: %w", st.Name, err)
		}
		m.States = append(m.States, x)
	}
	for _, t := range sm.Transitions {
		x := ir.MachineTransition{From: t.From, On: t.On, To: t.To}
		msg := msgOf(t.On)
		if msg == nil {
			return fmt.Errorf("transition %s --%s-->: no contract", t.From, t.On)
		}
		if t.Guard != "" {
			tree, err := command.ParseExpr(t.Guard, true)
			if err != nil {
				return fmt.Errorf("transition %s --%s--> guard: %w", t.From, t.On, err)
			}
			if err := command.Resolve(tree, scope(msg), "", "", ""); err != nil {
				return fmt.Errorf("transition %s --%s--> guard %s: %w", t.From, t.On, t.Guard, err)
			}
			x.Guard = tree
		}
		if x.Emits, err = emits(t.Emit, msg); err != nil {
			return fmt.Errorf("transition %s --%s-->: %w", t.From, t.On, err)
		}
		m.Transitions = append(m.Transitions, x)
	}
	for _, l := range sm.Loads {
		ld := ir.MachineLoad{Name: l.Name, Table: l.Table, Max: l.Max}
		if len(l.On) != 1 {
			return fmt.Errorf("载入 %s: one condition", l.Name)
		}
		_, child, _ := strings.Cut(l.On[0], "=")
		ld.Child = strings.TrimSpace(child)
		for _, o := range l.OrderBy {
			c, dir, _ := strings.Cut(o, " ")
			ld.OrderBy = append(ld.OrderBy, c)
			ld.Desc = append(ld.Desc, dir == "desc")
		}
		for _, tc := range l.Columns {
			c, err := col(l.Table, tc.From)
			if err != nil {
				return err
			}
			ld.Columns = append(ld.Columns, c)
		}
		m.Loads = append(m.Loads, ld)
	}
	for _, c := range cat.tables[sm.Entity] { // 读出的列: 表的列的顺序
		if used[c.Name] {
			m.Columns = append(m.Columns, ir.Column{Name: c.Name, Type: c.Type, NotNull: c.NotNull})
		}
	}
	// 每个收到的消息类型一个处理器;迟到 = 从有这个消息的转移的 从 能到达的状态(不含 从 自己)
	next := map[string][]string{}
	for _, t := range m.Transitions {
		next[t.From] = append(next[t.From], t.To)
	}
	for _, st := range m.States {
		if st.DeadlineTo != "" {
			next[st.Name] = append(next[st.Name], st.DeadlineTo)
		}
	}
	seen := map[string]bool{}
	for _, t := range m.Transitions {
		if seen[t.On] {
			continue
		}
		seen[t.On] = true
		ct := m.Contract(t.On)
		r := ir.MachineReceive{Type: t.On, Kind: ct.Kind, Version: ct.Version, Method: m.Name + "On" + pascalType(t.On), KeyField: ct.Subject}
		late := map[string]bool{}
		for _, u := range m.Transitions {
			if u.On != t.On {
				continue
			}
			for x := range reachable(u.From, next) {
				if x != u.From {
					late[x] = true
				}
			}
		}
		for _, st := range m.States {
			if late[st.Name] {
				r.Late = append(r.Late, st.Name)
			}
		}
		m.Receives = append(m.Receives, r)
	}
	api.Command.Machine = m
	api.Command.Contracts = m.Contracts
	return nil
}

func reachable(from string, next map[string][]string) map[string]bool {
	seen := map[string]bool{}
	stack := append([]string{}, next[from]...)
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[x] {
			continue
		}
		seen[x] = true
		stack = append(stack, next[x]...)
	}
	return seen
}

// pascalType: inventory.reservation_failed → InventoryReservationFailed。
func pascalType(t string) string {
	var b strings.Builder
	for _, p := range strings.FieldsFunc(t, func(r rune) bool { return r == '.' || r == '_' }) {
		b.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	return b.String()
}

func upperSnakeName(pascal string) string {
	var b strings.Builder
	for i, r := range pascal {
		if r >= 'A' && r <= 'Z' && i > 0 {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToUpper(b.String())
}
