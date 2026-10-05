package tests

import (
	"net/http"
	"testing"
	"time"

	"twindemo/internal/twin"
)

var powerBase = time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)

func (c testClient) setPowerLimit(kw float64) twin.PowerPlanView {
	var plan twin.PowerPlanView
	c.do(http.MethodPut, "/site/power-limit", map[string]any{"power_limit_kw": kw}, &plan)
	return plan
}

func (c testClient) createPowerReservation(req twin.PowerReservationRequest) twin.PowerReservation {
	var res twin.PowerReservation
	c.do(http.MethodPost, "/power/reservations", req, &res)
	return res
}

func (c testClient) powerPlan() twin.PowerPlanView {
	var plan twin.PowerPlanView
	c.do(http.MethodGet, "/power/plan", nil, &plan)
	return plan
}

func (c testClient) powerReservation(id string) twin.PowerReservation {
	var res twin.PowerReservation
	c.do(http.MethodGet, "/power/reservations/"+id, nil, &res)
	return res
}

func powerReq(device string, ws, we time.Time, dur time.Duration, kw float64, prio int) twin.PowerReservationRequest {
	return twin.PowerReservationRequest{
		DeviceID:        device,
		Target:          twin.Config{"mode": "run"},
		WindowStart:     ws,
		WindowEnd:       we,
		DurationSeconds: int64(dur.Seconds()),
		PowerKW:         kw,
		Priority:        prio,
	}
}

func findReservation(t *testing.T, plan twin.PowerPlanView, id string) twin.PowerReservation {
	t.Helper()
	for _, r := range plan.Reservations {
		if r.ID == id {
			return *r
		}
	}
	t.Fatalf("计划中找不到预约 %s", id)
	return twin.PowerReservation{}
}

func assertPlanWithinLimit(t *testing.T, plan twin.PowerPlanView, limit float64) {
	t.Helper()
	for _, seg := range plan.Load {
		if seg.PowerKW > limit+1e-9 {
			t.Fatalf("负载区间 %v~%v 功率 %v 超过上限 %v", seg.Start, seg.End, seg.PowerKW, limit)
		}
	}
}

func assertDurationIntact(t *testing.T, res twin.PowerReservation) {
	t.Helper()
	if res.PlannedStart == nil || res.PlannedEnd == nil {
		t.Fatalf("预约 %s 缺少计划时间", res.ID)
	}
	got := res.PlannedEnd.Sub(*res.PlannedStart)
	if got != time.Duration(res.DurationSeconds)*time.Second {
		t.Fatalf("预约 %s 计划时长 %v 被静默改变（声明 %ds）", res.ID, got, res.DurationSeconds)
	}
}

func (c testClient) applyAndConfirm(id string) {
	rev, cfg := c.pull(id)
	c.report(id, twin.ReportRequest{Rev: rev, Applied: cfg})
}

// 可行场景：多设备交叉窗口下高优先级先排、低优先级偏移，
// 计划进入现有下发与确认流程，任一时刻不超过站点上限，时长不被截断。
func TestPowerPlanFeasibleCrossWindows(t *testing.T) {
	c, _, dbPath := newHarness(t)
	c.clock(powerBase)
	c.setPowerLimit(10)
	ws := powerBase.Add(30 * time.Minute)
	we := powerBase.Add(90 * time.Minute)
	r2 := c.createPowerReservation(powerReq("dev-2", ws, we, 30*time.Minute, 6, 2))
	r1 := c.createPowerReservation(powerReq("dev-1", ws, we, 30*time.Minute, 6, 1))

	plan := c.powerPlan()
	p1 := findReservation(t, plan, r1.ID)
	p2 := findReservation(t, plan, r2.ID)
	if p2.PlannedStart == nil || !p2.PlannedStart.Equal(ws) {
		t.Fatalf("高优先级预约应在窗口起点开始，实际 %v", p2.PlannedStart)
	}
	if p1.PlannedStart == nil || !p1.PlannedStart.Equal(ws.Add(30*time.Minute)) {
		t.Fatalf("低优先级预约应偏移到 11:00，实际 %v", p1.PlannedStart)
	}
	assertDurationIntact(t, p1)
	assertDurationIntact(t, p2)
	assertPlanWithinLimit(t, plan, 10)

	// 高优先级预约到点进入下发与确认流程。
	c.clock(ws)
	c.applyAndConfirm("dev-2")
	if v := c.twin("dev-2"); v.Status != twin.StatusEffective {
		t.Fatalf("dev-2 应已生效，实际 %s", v.Status)
	}

	// 11:00：dev-2 的停止目标已生成但未确认，dev-1 不得提前超额启动。
	c.clock(ws.Add(30 * time.Minute))
	if rev, _ := c.pull("dev-1"); rev != 0 {
		t.Fatalf("dev-2 停止未确认前，dev-1 不应收到新目标")
	}
	c.applyAndConfirm("dev-2") // 确认停止，容量真正释放
	rev, cfg := c.pull("dev-1")
	if rev == 0 {
		t.Fatalf("dev-2 停止确认后，dev-1 应收到目标")
	}
	c.report("dev-1", twin.ReportRequest{Rev: rev, Applied: cfg})
	if v := c.twin("dev-1"); v.Status != twin.StatusEffective {
		t.Fatalf("dev-1 应已生效，实际 %s", v.Status)
	}
	assertPlanWithinLimit(t, c.powerPlan(), 10)

	// 服务重启后计划与运行状态完整保留。
	c2 := newHarnessFromPath(t, dbPath)
	plan2 := c2.powerPlan()
	if findReservation(t, plan2, r1.ID).Status != twin.PowerStatusRunning {
		t.Fatalf("重启后 dev-1 预约应仍在运行")
	}
	if findReservation(t, plan2, r2.ID).Status != twin.PowerStatusDone {
		t.Fatalf("重启后 dev-2 预约应已完成")
	}

	// 11:30：dev-1 任务结束并确认恢复后，两个预约都完成。
	c2.clock(ws.Add(time.Hour))
	c2.applyAndConfirm("dev-1")
	plan3 := c2.powerPlan()
	if findReservation(t, plan3, r1.ID).Status != twin.PowerStatusDone {
		t.Fatalf("dev-1 预约应已完成")
	}
}

// 不可行场景：容量只够一个任务时，低优先级预约必须如实标注冲突预约与超限区间，
// 已安排任务的时长不被静默缩短。
func TestPowerPlanInfeasibleReportsConflicts(t *testing.T) {
	c, _, _ := newHarness(t)
	c.clock(powerBase)
	c.setPowerLimit(10)
	mk := func(device string, prio int) twin.PowerReservationRequest {
		return powerReq(device, powerBase, powerBase, 30*time.Minute, 6, prio)
	}
	r1 := c.createPowerReservation(mk("dev-a", 3))
	r2 := c.createPowerReservation(mk("dev-b", 2))
	r3 := c.createPowerReservation(mk("dev-c", 1))

	plan := c.powerPlan()
	winner := findReservation(t, plan, r1.ID)
	if winner.Status != twin.PowerStatusRunning && winner.Status != twin.PowerStatusPlanned {
		t.Fatalf("最高优先级预约应被安排，实际 %s", winner.Status)
	}
	assertDurationIntact(t, winner)
	for _, id := range []string{r2.ID, r3.ID} {
		loser := findReservation(t, plan, id)
		if loser.Status != twin.PowerStatusUnschedulable {
			t.Fatalf("预约 %s 应无法安排，实际 %s", id, loser.Status)
		}
		if loser.Conflict == nil {
			t.Fatalf("预约 %s 应给出冲突说明", id)
		}
		if len(loser.Conflict.ReservationIDs) == 0 || loser.Conflict.ReservationIDs[0] != r1.ID {
			t.Fatalf("预约 %s 的冲突预约应包含 %s，实际 %v", id, r1.ID, loser.Conflict.ReservationIDs)
		}
		if len(loser.Conflict.Overloads) == 0 {
			t.Fatalf("预约 %s 应给出超限区间", id)
		}
		ov := loser.Conflict.Overloads[0]
		if ov.RequiredKW != 12 || ov.LimitKW != 10 {
			t.Fatalf("超限区间应为 12kW/上限10kW，实际 %v/%v", ov.RequiredKW, ov.LimitKW)
		}
		if loser.PlannedStart != nil {
			t.Fatalf("无法安排的预约 %s 不应有计划时间", id)
		}
	}
	// 时间推进后，未安排的预约不会被悄悄下发或缩短。
	c.clock(powerBase.Add(5 * time.Minute))
	for _, dev := range []string{"dev-b", "dev-c"} {
		if rev, _ := c.pull(dev); rev != 0 {
			t.Fatalf("设备 %s 不应收到任何目标", dev)
		}
	}
	assertPlanWithinLimit(t, c.powerPlan(), 10)
}

// 贪心排序会错失可行安排的场景：按优先级贪心放置会让高优先级任务占满窗口起点，
// 低优先级任务被挤掉；全局求解让高优先级任务偏移，两者都可行。
func TestPowerPlanGreedyWouldMiss(t *testing.T) {
	c, _, _ := newHarness(t)
	c.clock(powerBase.Add(-time.Hour)) // 窗口在未来，创建时不会立即下发
	c.setPowerLimit(10)
	high := c.createPowerReservation(powerReq("dev-1", powerBase, powerBase.Add(50*time.Minute), 50*time.Minute, 7, 5))
	low := c.createPowerReservation(powerReq("dev-2", powerBase, powerBase.Add(10*time.Minute), 10*time.Minute, 5, 1))

	plan := c.powerPlan()
	ph := findReservation(t, plan, high.ID)
	pl := findReservation(t, plan, low.ID)
	if pl.PlannedStart == nil || !pl.PlannedStart.Equal(powerBase) {
		t.Fatalf("低优先级任务应在窗口起点开始，实际 %v", pl.PlannedStart)
	}
	if ph.PlannedStart == nil || !ph.PlannedStart.Equal(powerBase.Add(10*time.Minute)) {
		t.Fatalf("高优先级任务应偏移到 10:10 以容纳低优先级任务，实际 %v", ph.PlannedStart)
	}
	assertDurationIntact(t, ph)
	assertDurationIntact(t, pl)
	assertPlanWithinLimit(t, plan, 10)
}

// 停止确认迟到：停止目标已下发但设备未确认时，容量不得提前分给新任务；
// 不受影响的任务继续执行。
func TestPowerStopConfirmationHoldsCapacity(t *testing.T) {
	c, _, _ := newHarness(t)
	c.clock(powerBase)
	c.setPowerLimit(10)
	t1 := c.createPowerReservation(powerReq("dev-1", powerBase, powerBase, 30*time.Minute, 8, 1))
	t3 := c.createPowerReservation(powerReq("dev-3", powerBase, powerBase, 60*time.Minute, 2, 1))
	c.applyAndConfirm("dev-1")
	c.applyAndConfirm("dev-3")
	t2 := c.createPowerReservation(powerReq("dev-2", powerBase.Add(30*time.Minute), powerBase.Add(2*time.Hour), 30*time.Minute, 8, 1))
	if got := c.powerReservation(t2.ID); got.PlannedStart == nil || !got.PlannedStart.Equal(powerBase.Add(30*time.Minute)) {
		t.Fatalf("t2 应计划在 10:30，实际 %v", got.PlannedStart)
	}

	// 10:31：t1 的停止目标已生成但设备尚未确认，t2 不得提前超额启动。
	c.clock(powerBase.Add(31 * time.Minute))
	blocked := c.powerReservation(t2.ID)
	if blocked.Status == twin.PowerStatusRunning {
		t.Fatalf("停止未确认前 t2 不应启动")
	}
	if blocked.Status != twin.PowerStatusUnschedulable || blocked.Conflict == nil {
		t.Fatalf("t2 应标记为不可安排并给出冲突，实际 %s %+v", blocked.Status, blocked.Conflict)
	}
	if len(blocked.Conflict.ReservationIDs) == 0 || blocked.Conflict.ReservationIDs[0] != t1.ID {
		t.Fatalf("t2 的冲突预约应包含 t1，实际 %v", blocked.Conflict.ReservationIDs)
	}
	if rev, _ := c.pull("dev-2"); rev != 0 {
		t.Fatalf("容量被停止确认占用时，dev-2 不应收到目标")
	}
	assertPlanWithinLimit(t, c.powerPlan(), 10)

	// 不受影响的 t3 继续执行。
	if got := c.powerReservation(t3.ID); got.Status != twin.PowerStatusRunning {
		t.Fatalf("t3 应继续运行，实际 %s", got.Status)
	}
	if v := c.twin("dev-3"); v.Status != twin.StatusEffective {
		t.Fatalf("dev-3 应保持已生效，实际 %s", v.Status)
	}

	// dev-1 确认停止后容量释放，t2 才从当前时刻开始，绝不提前。
	c.applyAndConfirm("dev-1")
	started := c.powerReservation(t2.ID)
	if started.Status != twin.PowerStatusRunning {
		t.Fatalf("停止确认后 t2 应启动，实际 %s", started.Status)
	}
	if started.StartedAt == nil || !started.StartedAt.Equal(powerBase.Add(31*time.Minute)) {
		t.Fatalf("t2 应在确认后的 10:31 启动，实际 %v", started.StartedAt)
	}
	rev, cfg := c.pull("dev-2")
	if rev == 0 {
		t.Fatalf("t2 启动后 dev-2 应能拉取到目标")
	}
	c.report("dev-2", twin.ReportRequest{Rev: rev, Applied: cfg})
	assertPlanWithinLimit(t, c.powerPlan(), 10)
}

// 临时降低站点上限：只中断声明了中断许可的已启动任务；
// 未声明许可的已启动任务不受影响；尚未开始的普通任务被重新安排；
// 紧急预约加入后仍只重排未开始任务。
func TestPowerLimitLoweredInterruptAndEmergency(t *testing.T) {
	c, _, _ := newHarness(t)
	c.clock(powerBase)
	c.setPowerLimit(10)
	aReq := powerReq("dev-1", powerBase, powerBase, 60*time.Minute, 6, 1)
	a := c.createPowerReservation(aReq)
	cReq := powerReq("dev-3", powerBase, powerBase, 60*time.Minute, 4, 1)
	cReq.AllowInterrupt = true
	cRes := c.createPowerReservation(cReq)
	c.applyAndConfirm("dev-1")
	c.applyAndConfirm("dev-3")
	b := c.createPowerReservation(powerReq("dev-2", powerBase, powerBase.Add(2*time.Hour), 30*time.Minute, 6, 1))
	if got := c.powerReservation(b.ID); got.PlannedStart == nil || !got.PlannedStart.Equal(powerBase.Add(time.Hour)) {
		t.Fatalf("b 应计划在 a 结束后的 11:00，实际 %v", got.PlannedStart)
	}

	// 上限临时降到 6：运行中的 a(6kW) 未声明中断许可，保持不动；
	// c(4kW) 声明了中断许可，被按许可中断并进入停止确认流程。
	c.setPowerLimit(6)
	if got := c.powerReservation(a.ID); got.Status != twin.PowerStatusRunning || got.Interrupted {
		t.Fatalf("a 未声明中断许可，应继续运行，实际 %s interrupted=%v", got.Status, got.Interrupted)
	}
	cStopped := c.powerReservation(cRes.ID)
	if !cStopped.Interrupted || cStopped.Status != twin.PowerStatusStopping {
		t.Fatalf("c 声明了中断许可，应被中断并等待停止确认，实际 %s interrupted=%v", cStopped.Status, cStopped.Interrupted)
	}
	if v := c.twin("dev-1"); v.Status != twin.StatusEffective {
		t.Fatalf("dev-1 应保持已生效，实际 %s", v.Status)
	}
	// c 的停止确认前容量仍被占用，b 无法安排。
	if got := c.powerReservation(b.ID); got.Status != twin.PowerStatusUnschedulable {
		t.Fatalf("c 停止确认前 b 应无法安排，实际 %s", got.Status)
	}
	// c 确认停止后，b 被重新安排到 a 结束之后。
	c.applyAndConfirm("dev-3")
	if got := c.powerReservation(b.ID); got.PlannedStart == nil || !got.PlannedStart.Equal(powerBase.Add(time.Hour)) {
		t.Fatalf("c 确认后 b 应重新计划到 11:00，实际 %v", got.PlannedStart)
	}

	// 紧急预约加入：只重排尚未开始的任务，a 继续运行。
	eReq := powerReq("dev-4", powerBase.Add(5*time.Minute), powerBase.Add(2*time.Hour), 30*time.Minute, 6, 0)
	eReq.Emergency = true
	e := c.createPowerReservation(eReq)
	plan := c.powerPlan()
	pe := findReservation(t, plan, e.ID)
	pb := findReservation(t, plan, b.ID)
	if pe.Status != twin.PowerStatusPlanned || pe.PlannedStart == nil {
		t.Fatalf("紧急预约应被安排，实际 %s", pe.Status)
	}
	if pb.PlannedStart == nil || !pb.PlannedStart.Equal(powerBase.Add(time.Hour)) {
		t.Fatalf("b 与紧急预约偏移相同，应保持原计划 11:00 不变，实际 %v", pb.PlannedStart)
	}
	if !pe.PlannedStart.Equal(powerBase.Add(90 * time.Minute)) {
		t.Fatalf("紧急预约应排在 11:30，实际 %v", pe.PlannedStart)
	}
	if got := c.powerReservation(a.ID); got.Status != twin.PowerStatusRunning {
		t.Fatalf("a 应继续运行，实际 %s", got.Status)
	}
	assertPlanWithinLimit(t, plan, 6)
}

// 正常时钟与虚拟时钟走同一套规则：不设置虚拟时钟时，
// 窗口包含当前时间的预约同样立即进入下发与确认流程。
func TestPowerPlanRealClockSameRules(t *testing.T) {
	c, _, _ := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	c.setPowerLimit(10)
	res := c.createPowerReservation(powerReq("dev-rt", now.Add(-time.Minute), now.Add(time.Hour), 30*time.Minute, 5, 1))
	got := c.powerReservation(res.ID)
	if got.Status != twin.PowerStatusRunning {
		t.Fatalf("真实时钟下到点预约应立即下发，实际 %s", got.Status)
	}
	rev, cfg := c.pull("dev-rt")
	if rev == 0 {
		t.Fatalf("设备应能拉取到目标")
	}
	c.report("dev-rt", twin.ReportRequest{Rev: rev, Applied: cfg})
	if v := c.twin("dev-rt"); v.Status != twin.StatusEffective {
		t.Fatalf("dev-rt 应已生效，实际 %s", v.Status)
	}
	assertPlanWithinLimit(t, c.powerPlan(), 10)
}
