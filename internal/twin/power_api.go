package twin

import (
	"fmt"
	"sort"
	"time"
)

// SetPowerLimit 设置或临时降低站点功率上限，并只重排尚未开始的任务。
func (s *Store) SetPowerLimit(kw float64, now time.Time) (*PowerPlanView, error) {
	if kw <= 0 {
		return nil, fmt.Errorf("站点功率上限必须为正数")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	if err := s.processDueLocked(now); err != nil {
		return nil, err
	}
	s.powerLimitKW = kw
	s.powerLimitSet = true
	if err := s.replanPowerLocked(now); err != nil {
		return nil, err
	}
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return s.powerPlanViewLocked(now), nil
}

// CreatePowerReservation 创建功率预约并立即求解计划；紧急预约获得最高优先级。
func (s *Store) CreatePowerReservation(req PowerReservationRequest, now time.Time) (*PowerReservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	if err := s.processDueLocked(now); err != nil {
		return nil, err
	}
	if req.DeviceID == "" {
		return nil, fmt.Errorf("设备 ID 不能为空")
	}
	if req.Target == nil {
		return nil, fmt.Errorf("预约目标配置必须是 JSON 对象")
	}
	if req.DurationSeconds <= 0 {
		return nil, fmt.Errorf("运行时长必须为正数")
	}
	if req.PowerKW <= 0 {
		return nil, fmt.Errorf("运行功率必须为正数")
	}
	ws := req.WindowStart.UTC()
	we := req.WindowEnd.UTC()
	if we.Before(ws) {
		return nil, fmt.Errorf("允许开始窗口终点不能早于起点")
	}
	if we.Before(now) {
		return nil, fmt.Errorf("预约窗口已结束，无法安排")
	}
	s.reservationSeq++
	res := &PowerReservation{
		ID:              fmt.Sprintf("P%06d", s.reservationSeq),
		DeviceID:        req.DeviceID,
		Target:          cloneConfig(req.Target),
		WindowStart:     ws,
		WindowEnd:       we,
		DurationSeconds: req.DurationSeconds,
		PowerKW:         req.PowerKW,
		Priority:        req.Priority,
		Emergency:       req.Emergency,
		AllowInterrupt:  req.AllowInterrupt,
		Status:          PowerStatusPlanned,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	s.reservations[res.ID] = res
	if err := s.replanPowerLocked(now); err != nil {
		return nil, err
	}
	if _, err := s.dispatchDuePowerLocked(now); err != nil {
		return nil, err
	}
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return res.clone(), nil
}

// CancelPowerReservation 取消尚未开始的预约；已开始的任务只有声明了
// 中断许可才允许中断，中断仍走停止目标下发与确认流程。
func (s *Store) CancelPowerReservation(id string, now time.Time) (*PowerReservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	if err := s.processDueLocked(now); err != nil {
		return nil, err
	}
	res, ok := s.reservations[id]
	if !ok {
		return nil, fmt.Errorf("功率预约 %s 不存在", id)
	}
	switch res.Status {
	case PowerStatusPlanned, PowerStatusUnschedulable:
		res.Status = PowerStatusCancelled
		res.PlannedStart = nil
		res.PlannedEnd = nil
		res.UpdatedAt = now
	case PowerStatusRunning:
		if !res.AllowInterrupt {
			return nil, fmt.Errorf("任务 %s 已开始且未声明中断许可，不能中断", id)
		}
		if err := s.interruptReservationLocked(res, now); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("任务 %s 当前状态 %s 不能取消", id, res.Status)
	}
	if err := s.replanPowerLocked(now); err != nil {
		return nil, err
	}
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return res.clone(), nil
}

// PowerReservation 查询单个功率预约。
func (s *Store) PowerReservation(id string, now time.Time) (*PowerReservation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	_ = s.processDueLocked(now)
	res, ok := s.reservations[id]
	if !ok {
		return nil, false
	}
	return res.clone(), true
}

// PowerPlan 返回当前计划、已承诺负载曲线与超限报告。
func (s *Store) PowerPlan(now time.Time) *PowerPlanView {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	_ = s.processDueLocked(now)
	return s.powerPlanViewLocked(now)
}

func (s *Store) powerPlanViewLocked(now time.Time) *PowerPlanView {
	view := &PowerPlanView{
		PowerLimitKW: s.powerLimitKW,
		LimitSet:     s.powerLimitSet,
		Reservations: []*PowerReservation{},
		Load:         []PowerLoadSegment{},
		Overloads:    append([]PowerOverloadView(nil), s.powerOverloads...),
	}
	ids := make([]string, 0, len(s.reservations))
	for id := range s.reservations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		view.Reservations = append(view.Reservations, s.reservations[id].clone())
	}
	view.Load = buildLoadSegments(s.committedPowerIntervalsLocked())
	return view
}

// buildLoadSegments 把已承诺占用合并成负载曲线分段；末端为 nil 表示开放区间
// （停止目标尚未确认，容量一直被占用）。
func buildLoadSegments(intervals []solveInterval) []PowerLoadSegment {
	pts := []int64{}
	for _, iv := range intervals {
		pts = append(pts, iv.start, iv.end)
	}
	if len(pts) == 0 {
		return []PowerLoadSegment{}
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i] < pts[j] })
	pts = uniqueInts(pts)
	out := []PowerLoadSegment{}
	for i := 0; i+1 < len(pts); i++ {
		a, b := pts[i], pts[i+1]
		if b <= a {
			continue
		}
		load := 0.0
		ids := []string{}
		for _, iv := range intervals {
			if iv.start <= a && a < iv.end {
				load += iv.power
				ids = append(ids, iv.id)
			}
		}
		if load <= 0 {
			continue
		}
		sort.Strings(ids)
		seg := PowerLoadSegment{Start: time.Unix(a, 0).UTC(), PowerKW: load, Reservations: ids}
		if b < infiniteEnd {
			end := time.Unix(b, 0).UTC()
			seg.End = &end
		}
		out = append(out, seg)
	}
	return out
}

// dispatchDuePowerLocked 把计划时间已到的预约接入现有下发与确认流程。
// 容量不足（例如停止目标尚未确认）时绝不提前超额启动，返回 blocked=true。
func (s *Store) dispatchDuePowerLocked(now time.Time) (bool, error) {
	due := []*PowerReservation{}
	for _, res := range s.reservations {
		if res.Status == PowerStatusPlanned && res.PlannedStart != nil && !res.PlannedStart.After(now) {
			due = append(due, res)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].PlannedStart.Equal(*due[j].PlannedStart) {
			return due[i].PlannedStart.Before(*due[j].PlannedStart)
		}
		return due[i].ID < due[j].ID
	})
	blocked := false
	for _, res := range due {
		if d := s.devices[res.DeviceID]; d != nil && !d.Online {
			continue
		}
		if res.PlannedEnd == nil || !res.PlannedEnd.After(now) {
			blocked = true
			continue
		}
		committed := []solveInterval{}
		for _, iv := range s.committedPowerIntervalsLocked() {
			if iv.id == res.ID {
				continue
			}
			committed = append(committed, iv)
		}
		iv := solveInterval{start: now.Unix(), end: res.PlannedEnd.Unix(), power: res.PowerKW, device: res.DeviceID, id: res.ID}
		if !intervalFits(committed, iv, s.powerLimitLocked()) {
			blocked = true
			continue
		}
		ch, err := s.publishChangeLocked(map[string]Config{res.DeviceID: cloneConfig(res.Target)}, PublishOptions{
			Kind:     ChangeKindTarget,
			Strategy: ChangeStrategyQueue,
			EndAt:    res.PlannedEnd,
		}, now)
		if err != nil {
			return false, err
		}
		res.Status = PowerStatusRunning
		started := now
		res.StartedAt = &started
		res.ChangeID = ch.ID
		res.UpdatedAt = now
	}
	return blocked, nil
}
