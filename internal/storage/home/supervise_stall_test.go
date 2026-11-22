package home

import (
	"context"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/tasks"
)

// Диск записи, выпавший к моменту отката, — предупреждение helper
// (остатки не стёрты), а не молчаливое «диски свободны».
func TestCreateRollbackWarnsMissingDisk(t *testing.T) {
	h := newHost(t)
	h.part("sdb", 1)
	h.write("sys/devices/sdb/sdb1/uevent", "PARTNAME=dn-p1-0\n")
	gone := "wwn-0x50000000000001ff"
	r, err := h.e.CreateRollback(context.Background(), CreateRollbackParams{Pool: "p1", DiskIDs: []string{ref101.ID, ref102.ID, gone}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], gone) || !strings.Contains(r.Warnings[0], "освободите") {
		t.Fatalf("предупреждения %q", r.Warnings)
	}
	// Все диски на месте — без предупреждений.
	r, err = h.e.CreateRollback(context.Background(), CreateRollbackParams{Pool: "p1", DiskIDs: []string{ref101.ID, ref102.ID}})
	if err != nil || len(r.Warnings) != 0 {
		t.Fatalf("%v %q", err, r.Warnings)
	}
}

// Задача create: предупреждение отката попадает в причину, «диски свободны» не пишется.
func TestCreateRollbackPartialMessage(t *testing.T) {
	e := newSvc(t)
	e.s.DegradedWait = 30 * time.Millisecond
	e.h.results = map[string]Result{OpCreateRollback: {Pool: "p1", Warnings: []string{"диск X недоступен: остатки пула на нём не стёрты — после возврата освободите диск"}}}
	ctx := context.Background()
	p, err := e.s.Plan(ctx, createReq)
	if err != nil {
		t.Fatal(err)
	}
	e.h.setStatus(degradedIdle(LevelRAID1, 2, gib-3*Align))
	tk, err := e.s.Confirm(ctx, p.PlanID, p.ConfirmToken)
	if err != nil {
		t.Fatal(err)
	}
	done := e.wait(t, tk.ID)
	if done.State != tasks.Failed || done.Error == nil {
		t.Fatalf("задача %+v", done)
	}
	m := done.Error.Message
	if !strings.Contains(m, "частичный откат") || !strings.Contains(m, "диск X недоступен") || strings.Contains(m, "диски свободны") {
		t.Fatalf("сообщение %q", m)
	}
	if rollbackDone(Result{}) != "выполнен откат: пул разобран, диски свободны" {
		t.Fatal("без предупреждений — полный откат")
	}
}
