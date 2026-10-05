package twin

import (
	"math"
	"sort"
	"time"
)

// powerLimitLocked 返回当前站点功率上限；未设置时视为不限。
func (s *Store) powerLimitLocked() float64 {
	if !s.powerLimitSet {
		return math.MaxFloat64 / 16
	}
	return s.powerLimitKW
}

// committedPowerIntervalsLocked 汇总当前已承诺的功率占用：
// 运行中任务占到计划结束；已发停止目标但未确认的任务按保守规则一直占用，
// 绝不把这部分容量提前分给新任务；已计划任务占整个计划区间。
func (s *Store) committedPowerIntervalsLocked() []solveInterval {
	out := []solveInterval{}
	for _, res := range s.reservations {
		switch res.Status {
		case PowerStatusRunning:
			if res.StartedAt != nil && res.PlannedEnd != nil {
				out = append(out, solveInterval{start: res.StartedAt.Unix(), end: res.PlannedEnd.Unix(), power: res.PowerKW, device: res.DeviceID, id: res.ID})
			}
		case PowerStatusStopping:
			if res.StartedAt != nil {
				out = append(out, solveInterval{start: res.StartedAt.Unix(), end: infiniteEnd, power: res.PowerKW, device: res.DeviceID, id: res.ID})
			}
		case PowerStatusPlanned:
			if res.PlannedStart != nil && res.PlannedEnd != nil {
				out = append(out, solveInterval{start: res.PlannedStart.Unix(), end: res.PlannedEnd.Unix(), power: res.PowerKW, device: res.DeviceID, id: res.ID})
			}
		}
	}
	return out
}

// syncPowerStatesLocked 根据下发/确认流程的真实进度推进预约状态：
// 停止目标未确认前是 STOPPING，确认后才 DONE 并释放容量。
func (s *Store) syncPowerStatesLocked(now time.Time) {
	for _, res := range s.reservations {
		if res.Status != PowerStatusRunning && res.Status != PowerStatusStopping {
			continue
		}
		ch := s.changes[res.ChangeID]
		if ch == nil {
			continue
		}
		result, ok := ch.DeviceResults[res.DeviceID]
		if !ok || result.EndReason == "" {
			continue
		}
		if result.WaitReason == "" {
			res.Status = PowerStatusDone
			finished := now
			if result.EndAt != nil {
				finished = *result.EndAt
			}
			res.FinishedAt = &finished
			res.UpdatedAt = now
		} else {
			res.Status = PowerStatusStopping
			res.UpdatedAt = now
		}
	}
}

// firstFixedOverload 返回固定占用（已启动任务）中第一个超限区间。
func firstFixedOverload(fixed []solveInterval, cap float64) *OverloadInterval {
	pts := []int64{}
	for _, iv := range fixed {
		pts = append(pts, iv.start, iv.end)
	}
	if len(pts) == 0 {
		return nil
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i] < pts[j] })
	pts = uniqueInts(pts)
	for i := 0; i+1 < len(pts); i++ {
		a, b := pts[i], pts[i+1]
		if b <= a {
			continue
		}
		load := 0.0
		ids := []string{}
		for _, iv := range fixed {
			if iv.start <= a && a < iv.end {
				load += iv.power
				ids = append(ids, iv.id)
			}
		}
		if load > cap+1e-9 {
			sort.Strings(ids)
			return &OverloadInterval{Start: a, End: b, RequiredKW: load, LimitKW: cap, SourceIDs: ids}
		}
	}
	return nil
}

// interruptReservationLocked 按明确的中断许可中断一个已启动任务：
// 通过现有流程生成恢复（停止）目标，容量在设备确认前仍被占用。
func (s *Store) interruptReservationLocked(res *PowerReservation, now time.Time) error {
	ch := s.changes[res.ChangeID]
	if ch != nil {
		if _, ok := ch.DeviceResults[res.DeviceID]; ok {
			if err := s.finishReservationDeviceLocked(ch, res.DeviceID, now, "INTERRUPTED"); err != nil {
				return err
			}
		}
	}
	res.Interrupted = true
	res.Status = PowerStatusStopping
	res.UpdatedAt = now
	return nil
}

// replanPowerLocked 重新安排尚未实际开始的任务。已启动的连续运行任务保持不动，
// 只有声明了中断许可的任务才会在站点超限时被中断。
func (s *Store) replanPowerLocked(now time.Time) error {
	s.syncPowerStatesLocked(now)
	cap := s.powerLimitLocked()
	s.powerOverloads = nil

	fixed := []solveInterval{}
	for _, res := range s.reservations {
		switch res.Status {
		case PowerStatusRunning:
			if res.StartedAt != nil && res.PlannedEnd != nil {
				fixed = append(fixed, solveInterval{start: res.StartedAt.Unix(), end: res.PlannedEnd.Unix(), power: res.PowerKW, device: res.DeviceID, id: res.ID})
			}
		case PowerStatusStopping:
			if res.StartedAt != nil {
				fixed = append(fixed, solveInterval{start: res.StartedAt.Unix(), end: infiniteEnd, power: res.PowerKW, device: res.DeviceID, id: res.ID})
			}
		}
	}
	// 站点上限被临时降低等情况下，已启动任务可能超限：
	// 只中断声明了中断许可的任务（低优先级优先），其余如实报告超限区间。
	for step := 0; step < len(s.reservations)+1; step++ {
		seg := firstFixedOverload(fixed, cap)
		if seg == nil {
			break
		}
		var candidate *PowerReservation
		for _, res := range s.reservations {
			if res.Status != PowerStatusRunning || !res.AllowInterrupt || res.StartedAt == nil || res.PlannedEnd == nil {
				continue
			}
			if res.StartedAt.Unix() < seg.End && seg.Start < res.PlannedEnd.Unix() {
				if candidate == nil || res.effectivePriority() < candidate.effectivePriority() {
					candidate = res
				}
			}
		}
		if candidate == nil {
			s.powerOverloads = append(s.powerOverloads, overloadView(*seg))
			break
		}
		if err := s.interruptReservationLocked(candidate, now); err != nil {
			return err
		}
		fixed = append(fixed, solveInterval{start: candidate.StartedAt.Unix(), end: infiniteEnd, power: candidate.PowerKW, device: candidate.DeviceID, id: candidate.ID})
	}

	tasks := []solveTask{}
	order := []string{}
	for id, res := range s.reservations {
		if res.Status != PowerStatusPlanned && res.Status != PowerStatusUnschedulable {
			continue
		}
		order = append(order, id)
	}
	sort.Strings(order)
	skipped := []string{}
	for _, id := range order {
		res := s.reservations[id]
		ws := res.WindowStart.Unix()
		if ws < now.Unix() {
			ws = now.Unix()
		}
		we := res.WindowEnd.Unix()
		if we < ws {
			res.Status = PowerStatusUnschedulable
			res.PlannedStart = nil
			res.PlannedEnd = nil
			res.Conflict = &PowerConflictView{Reason: "预约窗口已过，无法安排"}
			res.UpdatedAt = now
			skipped = append(skipped, id)
			continue
		}
		t := solveTask{id: res.ID, device: res.DeviceID, ws: ws, we: we, dur: res.DurationSeconds, power: res.PowerKW, prio: res.effectivePriority()}
		if res.PlannedStart != nil {
			t.keep = res.PlannedStart.Unix()
			t.hasKeep = true
		}
		tasks = append(tasks, t)
	}
	result := solvePowerPlan(tasks, fixed, cap)
	dropped := map[string]DroppedTask{}
	for _, d := range result.dropped {
		dropped[d.ID] = d
	}
	for _, id := range order {
		res := s.reservations[id]
		if contains(skipped, id) {
			continue
		}
		if start, ok := result.placement[id]; ok {
			st := time.Unix(start, 0).UTC()
			en := time.Unix(start+res.DurationSeconds, 0).UTC()
			res.PlannedStart = &st
			res.PlannedEnd = &en
			res.Status = PowerStatusPlanned
			res.Conflict = nil
		} else if d, ok := dropped[id]; ok {
			res.Status = PowerStatusUnschedulable
			res.PlannedStart = nil
			res.PlannedEnd = nil
			view := &PowerConflictView{ReservationIDs: d.Conflicts, Reason: "站点功率约束下无可行开始时间"}
			for _, ov := range d.Overloads {
				view.Overloads = append(view.Overloads, overloadView(ov))
			}
			res.Conflict = view
		}
		res.UpdatedAt = now
	}
	return nil
}

func overloadView(ov OverloadInterval) PowerOverloadView {
	view := PowerOverloadView{
		Start:      time.Unix(ov.Start, 0).UTC(),
		RequiredKW: ov.RequiredKW,
		LimitKW:    ov.LimitKW,
		SourceIDs:  append([]string(nil), ov.SourceIDs...),
	}
	if ov.End < infiniteEnd {
		end := time.Unix(ov.End, 0).UTC()
		view.End = end
	}
	return view
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
