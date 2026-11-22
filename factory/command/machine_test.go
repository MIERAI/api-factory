package command

import (
	"testing"
	"time"

	"apifactory/factory/ir"
)

// 订单 saga 的形状: 迟到 = 能从有这个消息的转移的 从 到达(经过期限的边也算)。
func sagaIR() *ir.Machine {
	emit := func(typ string) *ir.CommandEmit { return &ir.CommandEmit{Type: typ} }
	return &ir.Machine{Name: "S", Table: "orders", Key: ir.Column{Name: "id", Type: "int8"}, State: ir.Column{Name: "status", Type: "int4"},
		Entered: "updated_at", Version: "version", Attempts: "attempts",
		States: []ir.MachineState{
			{Name: "RESERVING", DB: 2, Command: emit("reserve"), Timeout: 30, Every: 60, Deadline: 900, DeadlineTo: "RELEASING"},
			{Name: "CHARGING", DB: 3, Command: emit("charge"), Timeout: 30, Every: 60},
			{Name: "CONFIRMING", DB: 4, Command: emit("commit"), Timeout: 30, Every: 60},
			{Name: "RELEASING", DB: 5, Command: emit("release"), Timeout: 30, Every: 60},
			{Name: "CONFIRMED", DB: 6, Final: true},
			{Name: "CANCELLED", DB: 7, Final: true},
		},
		Transitions: []ir.MachineTransition{
			{From: "RESERVING", On: "reserved", To: "CHARGING"},
			{From: "RESERVING", On: "failed", To: "CANCELLED"},
			{From: "CHARGING", On: "paid", To: "CONFIRMING"},
			{From: "CHARGING", On: "declined", To: "RELEASING"},
			{From: "CONFIRMING", On: "committed", To: "CONFIRMED"},
			{From: "RELEASING", On: "released", To: "CANCELLED"},
		},
		Contracts: []ir.Contract{{Type: "reserved", Subject: "order_id"}, {Type: "committed", Subject: "order_id"}, {Type: "released", Subject: "order_id"}},
	}
}

func TestMachineLate(t *testing.T) {
	m := sagaIR()
	cases := []struct {
		typ, cur string
		late     bool
	}{
		{"reserved", "CHARGING", true}, {"reserved", "CONFIRMED", true}, {"reserved", "RELEASING", true}, // 期限的边: RESERVING → RELEASING
		{"reserved", "RESERVING", false},
		{"committed", "RESERVING", false}, {"committed", "CANCELLED", false}, {"committed", "CONFIRMED", true},
		{"released", "CONFIRMED", false}, {"released", "CANCELLED", true},
		{"paid", "CANCELLED", true}, // CHARGING → RELEASING → CANCELLED
	}
	for _, c := range cases {
		if got := MachineLate(m, c.typ, c.cur); got != c.late {
			t.Errorf("%s in %s: late = %v", c.typ, c.cur, got)
		}
	}
}

func TestMachineDeliverAndTimeouts(t *testing.T) {
	m := sagaIR()
	tbl := &Table{Name: "orders", PK: []string{"id"}, Cols: []Column{{Name: "id", Type: "int8"}, {Name: "status", Type: "int4"},
		{Name: "version", Type: "int4"}, {Name: "attempts", Type: "int4"}, {Name: "updated_at", Type: "timestamptz"}}}
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ts0, _ := Canon("timestamptz", t0)
	s := &State{Tables: map[string]*TableState{"orders": {T: tbl, Rows: []Row{
		{"id": "1", "status": "2", "version": "1", "attempts": "0", "updated_at": ts0},
		{"id": "2", "status": "6", "version": "4", "attempts": "0", "updated_at": ts0},
	}}}}
	now, _ := Canon("timestamptz", t0.Add(time.Second))
	if r := s.Deliver(m, "reserved", Inputs{"order_id": int64(9)}, now); r.Reason != "not_found" {
		t.Errorf("not found: %+v", r)
	}
	if r := s.Deliver(m, "committed", Inputs{"order_id": int64(1)}, now); r.Outcome != MachineRejected || r.Reason != "impossible" {
		t.Errorf("impossible: %+v", r)
	}
	if r := s.Deliver(m, "reserved", Inputs{"order_id": int64(2)}, now); r.Outcome != MachineIgnored {
		t.Errorf("late: %+v", r)
	}
	r := s.Deliver(m, "reserved", Inputs{"order_id": int64(1)}, now)
	if r.Outcome != MachineTransition || r.To != "CHARGING" || len(r.Emits) != 1 || r.Emits[0].Type != "charge" {
		t.Fatalf("transition: %+v", r)
	}
	row := s.Tables["orders"].Rows[0]
	if row["status"] != "3" || row["version"] != "2" || row["attempts"] != "0" || row["updated_at"] != now {
		t.Fatalf("row after the transition: %v", row)
	}
	// 超时: 进入之后 30s(正好)重发,之后每 60s
	e := t0.Add(time.Second)
	if got := s.Timeouts(m, e.Add(30*time.Second-time.Microsecond), 100); len(got) != 0 {
		t.Errorf("just before: %+v", got)
	}
	if got := s.Timeouts(m, e.Add(30*time.Second), 100); len(got) != 1 || got[0].Outcome != MachineRetry || s.Tables["orders"].Rows[0]["attempts"] != "1" {
		t.Errorf("first resend: %+v", got)
	}
	if got := s.Timeouts(m, e.Add(89*time.Second), 100); len(got) != 0 {
		t.Errorf("before the second resend: %+v", got)
	}
	if got := s.Timeouts(m, e.Add(90*time.Second), 100); len(got) != 1 || got[0].Emits[0].Type != "charge" {
		t.Errorf("second resend: %+v", got)
	}
	// RESERVING 的期限(期限优先于重发;进入早的在前,1 号同时到了下一次重发)
	s.Tables["orders"].Rows = append(s.Tables["orders"].Rows, Row{"id": "3", "status": "2", "version": "1", "attempts": "14", "updated_at": ts0})
	got := s.Timeouts(m, t0.Add(900*time.Second), 100)
	if len(got) != 2 || got[1].Outcome != MachineRetry || got[0].Outcome != MachineDeadline || got[0].To != "RELEASING" || got[0].Emits[0].Type != "release" {
		t.Errorf("deadline: %+v", got)
	}
}
