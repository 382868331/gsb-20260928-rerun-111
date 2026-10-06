package tests

import (
	"net/http"
	"testing"
	"time"

	"twindemo/internal/twin"
)

var powerBase = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

func pAt(sec int) time.Time { return powerBase.Add(time.Duration(sec) * time.Second) }

func (c testClient) setClock(t time.Time) {
	c.do(http.MethodPost, "/clock", map[string]any{"now": t}, nil)
}

func (c testClient) setPowerLimit(limit float64) twin.PowerOverview {
	var ov twin.PowerOverview
	c.do(http.MethodPut, "/site/power-limit", map[string]any{"limit": limit}, &ov)
	return ov
}

func (c testClient) submitPower(res []twin.PowerReservationRequest) twin.PowerOverview {
	var ov twin.PowerOverview
	c.do(http.MethodPost, "/power/reservations", map[string]any{"reservations": res}, &ov)
	return ov
}

func (c testClient) powerOverview() twin.PowerOverview {
	var ov twin.PowerOverview
	c.do(http.MethodGet, "/site/power", nil, &ov)
	return ov
}

func findReservation(ov twin.PowerOverview, id string) *twin.PowerReservation {
	for i := range ov.Reservations {
		if ov.Reservations[i].ID == id {
			return ov.Reservations[i]
		}
	}
	return nil
}

// applyLatest 让设备拉取当前期望并精确上报，走完整下发/确认流程。
func (c testClient) applyLatest(id string) {
	rev, cfg := c.pull(id)
	c.report(id, twin.ReportRequest{Rev: rev, Applied: cfg})
}

// 多台设备交叉窗口的可行场景：求解器错开高功率任务，
// 计划进入既有下发/确认流程，停止确认迟到时容量不提前释放。
func TestPowerScheduleFeasibleCrossDevice(t *testing.T) {
	c, _, _ := newHarness(t)
	c.setClock(powerBase)
	c.setPowerLimit(10)

	ov := c.submitPower([]twin.PowerReservationRequest{
		{ID: "r1", DeviceID: "d1", Config: twin.Config{"mode": "charge"}, EarliestStart: pAt(0), LatestStart: pAt(120), DurationSec: 60, Power: 5, Priority: 5},
		{ID: "r2", DeviceID: "d2", Config: twin.Config{"mode": "charge"}, EarliestStart: pAt(0), LatestStart: pAt(120), DurationSec: 60, Power: 6, Priority: 4},
		{ID: "r3", DeviceID: "d3", Config: twin.Config{"mode": "heat"}, EarliestStart: pAt(0), LatestStart: pAt(120), DurationSec: 60, Power: 5, Priority: 3},
	})
	// r1+r2=11、r2+r3=11 都超限，只有 r1+r3 能并行；r2 必须后移。
	if got := findReservation(ov, "r1").PlannedStart; got == nil || !got.Equal(pAt(0)) {
		t.Fatalf("r1 应在 0 开始: %+v", got)
	}
	if got := findReservation(ov, "r3").PlannedStart; got == nil || !got.Equal(pAt(0)) {
		t.Fatalf("r3 应在 0 开始: %+v", got)
	}
	if got := findReservation(ov, "r2").PlannedStart; got == nil || !got.Equal(pAt(60)) {
		t.Fatalf("r2 应后移到 60s: %+v", got)
	}
	if st := findReservation(ov, "r1").Status; st != twin.PowerStatusDispatched {
		t.Fatalf("r1 到点应立即进入下发流程: %s", st)
	}
	if st := findReservation(ov, "r2").Status; st != twin.PowerStatusPlanned {
		t.Fatalf("r2 未到点应等待: %s", st)
	}

	// r1、r3 走完整设备确认流程。
	c.applyLatest("d1")
	c.applyLatest("d3")
	if v := c.twin("d1"); v.Status != twin.StatusEffective {
		t.Fatalf("d1 应已生效: %s", v.Status)
	}

	// 到 r2 的计划开始时间：r1/r3 的停止目标已发出但设备尚未确认，
	// 容量不能提前分给 r2，r2 不得超额启动。
	c.setClock(pAt(60))
	ov = c.powerOverview()
	if st := findReservation(ov, "r2").Status; st != twin.PowerStatusHeld {
		t.Fatalf("停止确认未完成前 r2 不得启动: %s", st)
	}
	if ov.CurrentLoad != 10 {
		t.Fatalf("未确认停止的功率仍占用容量, 当前负载应为 10: %v", ov.CurrentLoad)
	}
	if rev, _ := c.pull("d2"); rev != 0 {
		t.Fatalf("容量不足时不应向 d2 下发新目标")
	}

	// d1 确认恢复后容量仍不够（r3 占 5，余 5 < 6），r2 继续等待。
	c.applyLatest("d1")
	ov = c.powerOverview()
	if st := findReservation(ov, "r2").Status; st != twin.PowerStatusHeld {
		t.Fatalf("只有部分容量释放时 r2 仍不得启动: %s", st)
	}
	// d3 确认恢复后容量足够，r2 立即进入下发流程，时长不缩短。
	c.applyLatest("d3")
	ov = c.powerOverview()
	r2 := findReservation(ov, "r2")
	if r2.Status != twin.PowerStatusDispatched {
		t.Fatalf("容量释放后 r2 应立即下发: %s", r2.Status)
	}
	if r2.ActualStart == nil || !r2.ActualStart.Equal(pAt(60)) {
		t.Fatalf("r2 实际开始应为 60s: %+v", r2.ActualStart)
	}
	if r2.DurationSec != 60 {
		t.Fatalf("任务时长不得被缩短: %d", r2.DurationSec)
	}
	ch := c.change(r2.ChangeID)
	res := ch.DeviceResults["d2"]
	if res.EndAt == nil || res.StartAt == nil || res.EndAt.Sub(*res.StartAt) != 60*time.Second {
		t.Fatalf("下发的预约窗口必须保持完整 60s: %+v", res)
	}

	// r2 确认生效，到期后确认恢复，全部收尾完成。
	c.applyLatest("d2")
	c.setClock(pAt(120))
	c.applyLatest("d2")
	ov = c.powerOverview()
	for _, id := range []string{"r1", "r2", "r3"} {
		if st := findReservation(ov, id).Status; st != twin.PowerStatusDone {
			t.Fatalf("%s 应已完成: %s", id, st)
		}
	}
	if ov.CurrentLoad != 0 {
		t.Fatalf("全部确认后负载应为 0: %v", ov.CurrentLoad)
	}
}

// 停止确认迟到时，受影响的任务等待、不受影响的任务照常执行。
func TestPowerLateStopConfirmationBlocksOnlyAffected(t *testing.T) {
	c, _, _ := newHarness(t)
	c.setClock(powerBase)
	c.setPowerLimit(10)

	c.submitPower([]twin.PowerReservationRequest{
		{ID: "a", DeviceID: "d1", Config: twin.Config{"mode": "charge"}, EarliestStart: pAt(0), LatestStart: pAt(0), DurationSec: 60, Power: 6, Priority: 5},
		{ID: "b", DeviceID: "d2", Config: twin.Config{"mode": "charge"}, EarliestStart: pAt(60), LatestStart: pAt(600), DurationSec: 60, Power: 6, Priority: 9},
		{ID: "c", DeviceID: "d3", Config: twin.Config{"mode": "vent"}, EarliestStart: pAt(60), LatestStart: pAt(600), DurationSec: 60, Power: 3, Priority: 1},
	})
	c.applyLatest("d1")

	// a 到期，停止目标已发出但设备未确认：b 需要 a 的容量必须等待，
	// c 与 a 合计 9 <= 10，不受影响继续执行。
	c.setClock(pAt(60))
	ov := c.powerOverview()
	if st := findReservation(ov, "b").Status; st != twin.PowerStatusHeld {
		t.Fatalf("b 依赖 a 的容量, 停止确认迟到时不得启动: %s", st)
	}
	if st := findReservation(ov, "c").Status; st != twin.PowerStatusDispatched {
		t.Fatalf("c 不受影响应照常下发: %s", st)
	}
	c.applyLatest("d3")
	if v := c.twin("d3"); v.Status != twin.StatusEffective {
		t.Fatalf("c 的任务应正常生效: %s", v.Status)
	}

	// 时钟继续走、确认一直不到，b 始终保持等待，不会提前超额启动。
	c.setClock(pAt(90))
	ov = c.powerOverview()
	if st := findReservation(ov, "b").Status; st != twin.PowerStatusHeld {
		t.Fatalf("确认迟到期间 b 不得启动: %s", st)
	}
	if ov.CurrentLoad != 9 {
		t.Fatalf("a 未确认停止仍占 6, c 在运行占 3, 负载应为 9: %v", ov.CurrentLoad)
	}

	// a 的停止确认到达后，b 才获得容量启动；开始时间顺延但时长不缩短。
	c.applyLatest("d1")
	ov = c.powerOverview()
	b := findReservation(ov, "b")
	if b.Status != twin.PowerStatusDispatched {
		t.Fatalf("停止确认后 b 应立即下发: %s", b.Status)
	}
	if b.ActualStart == nil || !b.ActualStart.Equal(pAt(90)) {
		t.Fatalf("b 实际开始应顺延到 90s: %+v", b.ActualStart)
	}
	if b.DurationSec != 60 {
		t.Fatalf("b 的时长不得被缩短: %d", b.DurationSec)
	}
	if st := findReservation(ov, "a").Status; st != twin.PowerStatusDone {
		t.Fatalf("a 确认停止后应完成: %s", st)
	}
}

// 不可行场景：如实报告冲突预约与超限区间，绝不缩短任务来凑可行。
func TestPowerInfeasibleReportsConflicts(t *testing.T) {
	c, _, _ := newHarness(t)
	c.setClock(powerBase)
	c.setPowerLimit(10)

	ov := c.submitPower([]twin.PowerReservationRequest{
		{ID: "ra", DeviceID: "d1", Config: twin.Config{"mode": "charge"}, EarliestStart: pAt(0), LatestStart: pAt(30), DurationSec: 60, Power: 8, Priority: 5},
		{ID: "rb", DeviceID: "d2", Config: twin.Config{"mode": "charge"}, EarliestStart: pAt(0), LatestStart: pAt(30), DurationSec: 60, Power: 8, Priority: 1},
	})
	// 两台 8kW 任务的窗口完全重叠，上限 10：只有高优先级的 ra 能安排。
	if st := findReservation(ov, "ra").Status; st != twin.PowerStatusDispatched {
		t.Fatalf("ra 应正常安排: %s", st)
	}
	rb := findReservation(ov, "rb")
	if rb.Status != twin.PowerStatusUnscheduled {
		t.Fatalf("rb 不可行应明确标记: %s", rb.Status)
	}
	if rb.Conflict == nil {
		t.Fatalf("不可行预约必须给出冲突说明")
	}
	if len(rb.Conflict.ConflictsWith) != 1 || rb.Conflict.ConflictsWith[0] != "ra" {
		t.Fatalf("冲突预约应指向 ra: %+v", rb.Conflict.ConflictsWith)
	}
	if len(rb.Conflict.Overloads) == 0 {
		t.Fatalf("必须给出超限区间")
	}
	over := rb.Conflict.Overloads[0]
	if over.Load != 8 || over.Limit != 10 {
		t.Fatalf("超限区间应说明负载与上限: %+v", over)
	}
	if !over.Start.Equal(pAt(0)) || !over.End.Equal(pAt(60)) {
		t.Fatalf("超限区间应覆盖重叠时段: %+v", over)
	}
	// 任务不被悄悄缩短，也不向设备下发。
	if rb.DurationSec != 60 {
		t.Fatalf("不可行任务时长不得被修改: %d", rb.DurationSec)
	}
	if rb.ChangeID != "" {
		t.Fatalf("不可行任务不应生成变更: %s", rb.ChangeID)
	}
	if rev, _ := c.pull("d2"); rev != 0 {
		t.Fatalf("不可行任务不应向设备下发目标")
	}
}

// 临时降低站点上限：只中断有明确许可的运行任务；
// 停止确认未完成前超限如实报告，确认后超限消除。
func TestPowerLimitLoweredInterruptsOnlyPermitted(t *testing.T) {
	c, _, _ := newHarness(t)
	c.setClock(powerBase)
	c.setPowerLimit(10)

	c.submitPower([]twin.PowerReservationRequest{
		{ID: "a", DeviceID: "d1", Config: twin.Config{"mode": "charge"}, EarliestStart: pAt(0), LatestStart: pAt(0), DurationSec: 600, Power: 6, Priority: 1, AllowInterrupt: true},
		{ID: "b", DeviceID: "d2", Config: twin.Config{"mode": "heat"}, EarliestStart: pAt(0), LatestStart: pAt(0), DurationSec: 600, Power: 4, Priority: 5},
	})
	c.applyLatest("d1")
	c.applyLatest("d2")

	// 上限降到 5：a 有中断许可且优先级低，被中断；b 无许可，继续运行。
	ov := c.setPowerLimit(5)
	if st := findReservation(ov, "a").Status; st != twin.PowerStatusInterrupted {
		t.Fatalf("a 应按中断许可被中断: %s", st)
	}
	if st := findReservation(ov, "b").Status; st != twin.PowerStatusDispatched {
		t.Fatalf("b 无中断许可应继续运行: %s", st)
	}
	// a 的停止目标已发出但设备尚未确认：容量仍被占用，超限如实报告。
	if len(ov.SiteOverloads) == 0 {
		t.Fatalf("停止确认未完成前超限必须如实报告")
	}
	if ov.CurrentLoad != 10 {
		t.Fatalf("a 停止未确认前负载仍为 10: %v", ov.CurrentLoad)
	}

	// a 确认停止后容量真正释放，超限消除；b 始终未被打断。
	c.applyLatest("d1")
	ov = c.powerOverview()
	if st := findReservation(ov, "a").Status; st != twin.PowerStatusDone {
		t.Fatalf("a 确认停止后应完成: %s", st)
	}
	if ov.CurrentLoad != 4 {
		t.Fatalf("确认停止后负载应为 4: %v", ov.CurrentLoad)
	}
	if len(ov.SiteOverloads) != 0 {
		t.Fatalf("确认停止后超限应消除: %+v", ov.SiteOverloads)
	}
	if st := findReservation(ov, "b").Status; st != twin.PowerStatusDispatched {
		t.Fatalf("b 应一直在运行: %s", st)
	}
}

// 紧急预约加入：只重排尚未开始的普通任务；
// 容量不足时按中断许可中断运行中的低优先级任务，无许可的任务不动。
func TestPowerEmergencyPreemptsInterruptibleOnly(t *testing.T) {
	c, _, _ := newHarness(t)
	c.setClock(powerBase)
	c.setPowerLimit(10)

	c.submitPower([]twin.PowerReservationRequest{
		{ID: "a", DeviceID: "d1", Config: twin.Config{"mode": "charge"}, EarliestStart: pAt(0), LatestStart: pAt(0), DurationSec: 600, Power: 6, Priority: 1, AllowInterrupt: true},
		{ID: "b", DeviceID: "d2", Config: twin.Config{"mode": "heat"}, EarliestStart: pAt(0), LatestStart: pAt(0), DurationSec: 600, Power: 4, Priority: 5},
	})
	c.applyLatest("d1")
	c.applyLatest("d2")

	// 紧急预约需要 6 功率，窗口内 a+b 已占满：中断有许可的 a，b 不动。
	ov := c.submitPower([]twin.PowerReservationRequest{
		{ID: "e", DeviceID: "d4", Config: twin.Config{"mode": "emergency"}, EarliestStart: pAt(60), LatestStart: pAt(300), DurationSec: 60, Power: 6, Priority: 0, Emergency: true},
	})
	if st := findReservation(ov, "a").Status; st != twin.PowerStatusInterrupted {
		t.Fatalf("a 应按中断许可为紧急预约让位: %s", st)
	}
	if st := findReservation(ov, "b").Status; st != twin.PowerStatusDispatched {
		t.Fatalf("b 无中断许可应继续运行: %s", st)
	}
	e := findReservation(ov, "e")
	if e.Status != twin.PowerStatusPlanned || e.PlannedStart == nil || !e.PlannedStart.Equal(pAt(60)) {
		t.Fatalf("紧急预约应计划在 60s 开始: %+v", e)
	}

	// 到 e 的计划开始时间：a 的停止确认未到，容量不提前释放，e 不超额启动。
	c.setClock(pAt(60))
	ov = c.powerOverview()
	if st := findReservation(ov, "e").Status; st != twin.PowerStatusHeld {
		t.Fatalf("a 停止确认迟到时 e 不得启动: %s", st)
	}
	// a 确认停止后 e 立即进入下发流程，b 始终未受影响。
	c.applyLatest("d1")
	ov = c.powerOverview()
	e = findReservation(ov, "e")
	if e.Status != twin.PowerStatusDispatched {
		t.Fatalf("a 确认停止后 e 应立即下发: %s", e.Status)
	}
	if e.ActualStart == nil || !e.ActualStart.Equal(pAt(60)) {
		t.Fatalf("e 实际开始应为 60s: %+v", e.ActualStart)
	}
	if v := c.twin("d2"); v.Status != twin.StatusEffective {
		t.Fatalf("b 的设备应始终保持已生效: %s", v.Status)
	}
	if got := c.twin("d2").ConfirmedConfig["mode"]; got != "heat" {
		t.Fatalf("b 的配置不应被改动: %v", got)
	}
}
