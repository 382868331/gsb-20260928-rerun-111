package twin

import (
	"fmt"
	"math"
	"sort"
	"time"

	"twindemo/internal/power"
)

// 功率预约的生命周期状态。
const (
	// PowerStatusPlanned 已计算出计划开始时间，等待到点派发。
	PowerStatusPlanned = "PLANNED"
	// PowerStatusHeld 计划开始时间已到，但站点容量不足（如停止确认迟到），暂不下发。
	PowerStatusHeld = "HELD"
	// PowerStatusDispatched 已生成变更进入下发/确认流程。
	PowerStatusDispatched = "DISPATCHED"
	// PowerStatusUnscheduled 窗口内功率约束不可行，未安排；附带冲突说明。
	PowerStatusUnscheduled = "UNSCHEDULED"
	// PowerStatusDone 任务结束且设备已确认恢复目标，容量真正释放。
	PowerStatusDone = "DONE"
	// PowerStatusCancelled 开始前被取消。
	PowerStatusCancelled = "CANCELLED"
	// PowerStatusInterrupted 运行中按明确的中断许可被中断。
	PowerStatusInterrupted = "INTERRUPTED"
)

// PowerReservation 是一项声明窗口、连续时长、功率与优先级的站点预约。
type PowerReservation struct {
	ID             string             `json:"id"`
	DeviceID       string             `json:"device_id"`
	Config         Config             `json:"config"`
	EarliestStart  time.Time          `json:"earliest_start"`
	LatestStart    time.Time          `json:"latest_start"`
	DurationSec    int64              `json:"duration_sec"`
	Power          float64            `json:"power"`
	Priority       int                `json:"priority"`
	AllowInterrupt bool               `json:"allow_interrupt"`
	Emergency      bool               `json:"emergency"`
	Status         string             `json:"status"`
	PlannedStart   *time.Time         `json:"planned_start,omitempty"`
	ActualStart    *time.Time         `json:"actual_start,omitempty"`
	ChangeID       string             `json:"change_id,omitempty"`
	Conflict       *power.Unscheduled `json:"conflict,omitempty"`
	CreatedAt      time.Time          `json:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at"`
}

func (r *PowerReservation) duration() time.Duration {
	return time.Duration(r.DurationSec) * time.Second
}

// 紧急预约在求解时等同于最高优先级。
func (r *PowerReservation) effectivePriority() int {
	if r.Emergency {
		return r.Priority + 1_000_000
	}
	return r.Priority
}

// PowerReservationRequest 是提交功率预约的输入。
type PowerReservationRequest struct {
	ID             string    `json:"id"`
	DeviceID       string    `json:"device_id"`
	Config         Config    `json:"config"`
	EarliestStart  time.Time `json:"earliest_start"`
	LatestStart    time.Time `json:"latest_start"`
	DurationSec    int64     `json:"duration_sec"`
	Power          float64   `json:"power"`
	Priority       int       `json:"priority"`
	AllowInterrupt bool      `json:"allow_interrupt"`
	Emergency      bool      `json:"emergency"`
}

// PowerOverview 是站点功率调度的整体视图。
type PowerOverview struct {
	Limit         *float64            `json:"limit"`
	CurrentLoad   float64             `json:"current_load"`
	Reservations  []*PowerReservation `json:"reservations"`
	SiteOverloads []power.Interval    `json:"site_overloads,omitempty"`
}

func (s *Store) powerLimitValueLocked() float64 {
	if s.sitePowerLimit == nil {
		return math.Inf(1)
	}
	return *s.sitePowerLimit
}

// SetSitePowerLimit 设置或临时降低站点功率上限，并触发重排。
func (s *Store) SetSitePowerLimit(limit float64, now time.Time) (*PowerOverview, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("站点功率上限必须为正数")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	if err := s.processDueLocked(now); err != nil {
		return nil, err
	}
	s.sitePowerLimit = &limit
	s.replanPowerLocked(now)
	if err := s.processDueLocked(now); err != nil {
		return nil, err
	}
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return s.powerOverviewLocked(now), nil
}

// SubmitPowerReservations 提交一批功率预约并重新求解计划。
// 只有尚未实际开始的预约会被重新安排；已开始的任务保持不动。
func (s *Store) SubmitPowerReservations(reqs []PowerReservationRequest, now time.Time) (*PowerOverview, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	if err := s.processDueLocked(now); err != nil {
		return nil, err
	}
	for i := range reqs {
		req := reqs[i]
		if req.DeviceID == "" {
			return nil, fmt.Errorf("功率预约缺少 device_id")
		}
		if req.DurationSec <= 0 {
			return nil, fmt.Errorf("预约 %s 的连续运行时长必须为正", req.DeviceID)
		}
		if req.Power <= 0 {
			return nil, fmt.Errorf("预约 %s 的运行功率必须为正", req.DeviceID)
		}
		if req.LatestStart.Before(req.EarliestStart) {
			return nil, fmt.Errorf("预约 %s 的开始窗口不合法", req.DeviceID)
		}
		s.powerSeq++
		id := req.ID
		if id == "" {
			id = fmt.Sprintf("P%06d", s.powerSeq)
		}
		if _, exists := s.powerRes[id]; exists {
			return nil, fmt.Errorf("功率预约 %s 已存在", id)
		}
		earliest := req.EarliestStart.UTC()
		if earliest.Before(now) {
			earliest = now
		}
		r := &PowerReservation{
			ID:             id,
			DeviceID:       req.DeviceID,
			Config:         cloneConfig(req.Config),
			EarliestStart:  earliest,
			LatestStart:    req.LatestStart.UTC(),
			DurationSec:    req.DurationSec,
			Power:          req.Power,
			Priority:       req.Priority,
			AllowInterrupt: req.AllowInterrupt,
			Emergency:      req.Emergency,
			Status:         PowerStatusPlanned,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		s.powerRes[id] = r
	}
	s.replanPowerLocked(now)
	if err := s.processDueLocked(now); err != nil {
		return nil, err
	}
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return s.powerOverviewLocked(now), nil
}

// PowerOverview 返回当前站点功率视图。
func (s *Store) PowerOverview(now time.Time) *PowerOverview {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	_ = s.processDueLocked(now)
	return s.powerOverviewLocked(now)
}

func (s *Store) powerOverviewLocked(now time.Time) *PowerOverview {
	out := &PowerOverview{
		Limit:        s.sitePowerLimit,
		CurrentLoad:  s.committedPowerLoadLocked(now),
		Reservations: []*PowerReservation{},
	}
	ids := make([]string, 0, len(s.powerRes))
	for id := range s.powerRes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		cp := *s.powerRes[id]
		out.Reservations = append(out.Reservations, &cp)
	}
	// 中断之后仍超限（存在无中断许可的运行任务）时，如实报告超限区间。
	if load := s.committedPowerLoadLocked(now); load > s.powerLimitValueLocked()+1e-9 {
		horizon := now.Add(time.Hour)
		for _, r := range s.powerRes {
			if r.Status == PowerStatusDispatched && r.ActualStart != nil {
				if end := r.ActualStart.Add(r.duration()); end.After(horizon) {
					horizon = end
				}
			}
		}
		out.SiteOverloads = append(out.SiteOverloads, power.Interval{
			Start: now,
			End:   horizon,
			Load:  load,
			Limit: s.powerLimitValueLocked(),
		})
	}
	return out
}

// replanPowerLocked 重新求解尚未实际开始的预约；已开始的任务作为固定区间。
// 若当前运行负载超过上限，只按明确的中断许可中断运行中任务。
func (s *Store) replanPowerLocked(now time.Time) {
	if len(s.powerRes) == 0 {
		return
	}
	limit := s.powerLimitValueLocked()
	// 运行负载超限：只中断有明确许可的任务，从最低优先级开始。
	for {
		load := s.committedPowerLoadLocked(now)
		if load <= limit+1e-9 {
			break
		}
		var victim *PowerReservation
		for _, r := range s.powerRes {
			if r.Status != PowerStatusDispatched || !r.AllowInterrupt {
				continue
			}
			if !s.reservationLoadActiveLocked(r, now) {
				continue
			}
			if victim == nil || r.effectivePriority() < victim.effectivePriority() ||
				(r.effectivePriority() == victim.effectivePriority() && r.ID < victim.ID) {
				victim = r
			}
		}
		if victim == nil {
			break
		}
		ch := s.changes[victim.ChangeID]
		if ch != nil {
			_ = s.finishReservationDeviceLocked(ch, victim.DeviceID, now, "INTERRUPTED")
		}
		victim.Status = PowerStatusInterrupted
		victim.UpdatedAt = now
	}
	fixed := []power.Block{}
	movable := []*PowerReservation{}
	for _, r := range s.powerRes {
		switch r.Status {
		case PowerStatusDispatched:
			if r.ActualStart != nil {
				fixed = append(fixed, power.Block{
					Start: *r.ActualStart,
					End:   r.ActualStart.Add(r.duration()),
					Power: r.Power,
					Ref:   r.ID,
				})
			}
		case PowerStatusPlanned, PowerStatusHeld, PowerStatusUnscheduled:
			movable = append(movable, r)
		}
	}
	if len(movable) == 0 {
		return
	}
	buildReqs := func() []power.Reservation {
		reqs := make([]power.Reservation, 0, len(movable))
		for _, r := range movable {
			reqs = append(reqs, power.Reservation{
				ID:            r.ID,
				EarliestStart: r.EarliestStart,
				LatestStart:   r.LatestStart,
				Duration:      r.duration(),
				Power:         r.Power,
				Priority:      r.effectivePriority(),
			})
		}
		return reqs
	}
	plan := power.Solve(buildReqs(), fixed, limit)
	// 紧急/高优先级预约排不进去时，按明确的中断许可中断优先级更低的
	// 运行中任务来腾出容量；无中断许可的任务绝不动。
	for len(plan.Unscheduled) > 0 {
		var need *PowerReservation
		for _, u := range plan.Unscheduled {
			r := s.powerRes[u.ReservationID]
			if r == nil {
				continue
			}
			if need == nil || r.effectivePriority() > need.effectivePriority() {
				need = r
			}
		}
		if need == nil {
			break
		}
		var victim *PowerReservation
		for _, r := range s.powerRes {
			if r.Status != PowerStatusDispatched || !r.AllowInterrupt {
				continue
			}
			if r.effectivePriority() >= need.effectivePriority() {
				continue
			}
			if victim == nil || r.effectivePriority() < victim.effectivePriority() ||
				(r.effectivePriority() == victim.effectivePriority() && r.ID < victim.ID) {
				victim = r
			}
		}
		if victim == nil {
			break
		}
		ch := s.changes[victim.ChangeID]
		if ch != nil {
			_ = s.finishReservationDeviceLocked(ch, victim.DeviceID, now, "INTERRUPTED")
		}
		victim.Status = PowerStatusInterrupted
		victim.UpdatedAt = now
		nextFixed := make([]power.Block, 0, len(fixed))
		for _, b := range fixed {
			if b.Ref != victim.ID {
				nextFixed = append(nextFixed, b)
			}
		}
		fixed = nextFixed
		plan = power.Solve(buildReqs(), fixed, limit)
	}
	unscheduled := map[string]power.Unscheduled{}
	for _, u := range plan.Unscheduled {
		unscheduled[u.ReservationID] = u
	}
	for _, r := range movable {
		if start, ok := plan.Assignments[r.ID]; ok {
			start = start.UTC()
			r.PlannedStart = &start
			r.Status = PowerStatusPlanned
			r.Conflict = nil
		} else {
			u := unscheduled[r.ID]
			r.PlannedStart = nil
			r.Status = PowerStatusUnscheduled
			r.Conflict = &u
		}
		r.UpdatedAt = now
	}
}

// reservationLoadActiveLocked 判断预约是否仍占用站点功率：
// 已派发（含被中断）且停止目标尚未被设备确认的部分都算占用。
func (s *Store) reservationLoadActiveLocked(r *PowerReservation, now time.Time) bool {
	if r.Status != PowerStatusDispatched && r.Status != PowerStatusInterrupted {
		return false
	}
	if r.ActualStart == nil || r.ActualStart.After(now) {
		return false
	}
	ch := s.changes[r.ChangeID]
	if ch == nil {
		return false
	}
	result := ch.DeviceResults[r.DeviceID]
	if result.Status == StatusCancelled || result.Status == StatusSuperseded {
		return false
	}
	return !s.stopConfirmedLocked(ch, r.DeviceID)
}

func (s *Store) stopConfirmedLocked(ch *Change, deviceID string) bool {
	for _, restoreID := range ch.RestoreChanges {
		restore := s.changes[restoreID]
		if restore == nil {
			continue
		}
		if result, ok := restore.DeviceResults[deviceID]; ok && result.Status == StatusEffective {
			return true
		}
	}
	return false
}

// committedPowerLoadLocked 是此刻站点已承诺的功率：运行中 +
// 已发出停止目标但设备尚未确认的部分（容量不能提前分给新任务）。
func (s *Store) committedPowerLoadLocked(now time.Time) float64 {
	total := 0.0
	for _, r := range s.powerRes {
		if s.reservationLoadActiveLocked(r, now) {
			total += r.Power
		}
	}
	return total
}

// sweepPowerReservationsLocked 根据关联变更的确认结果推进预约状态。
// 返回是否有预约完成（容量真正释放，需要重排）。
func (s *Store) sweepPowerReservationsLocked(now time.Time) bool {
	changed := false
	for _, r := range s.powerRes {
		if r.Status == PowerStatusInterrupted {
			// 被中断的任务也要等设备确认停止后才算容量释放。
			if ch := s.changes[r.ChangeID]; ch != nil && s.stopConfirmedLocked(ch, r.DeviceID) {
				r.Status = PowerStatusDone
				r.UpdatedAt = now
				changed = true
			}
			continue
		}
		if r.Status != PowerStatusDispatched {
			continue
		}
		ch := s.changes[r.ChangeID]
		if ch == nil {
			continue
		}
		result := ch.DeviceResults[r.DeviceID]
		switch {
		case result.EndReason == "INTERRUPTED":
			r.Status = PowerStatusInterrupted
			r.UpdatedAt = now
		case result.EndReason == "CANCELLED" || result.Status == StatusCancelled:
			r.Status = PowerStatusCancelled
			r.UpdatedAt = now
		case s.stopConfirmedLocked(ch, r.DeviceID):
			r.Status = PowerStatusDone
			r.UpdatedAt = now
			changed = true
		}
	}
	return changed
}

// dispatchDuePowerLocked 派发计划开始时间已到的预约。
// 派发前做容量门控：停止确认未完成的功率仍被占用，绝不让新任务提前超额启动。
func (s *Store) dispatchDuePowerLocked(now time.Time) error {
	due := []*PowerReservation{}
	for _, r := range s.powerRes {
		if r.Status != PowerStatusPlanned && r.Status != PowerStatusHeld {
			continue
		}
		if r.PlannedStart == nil || r.PlannedStart.After(now) {
			continue
		}
		due = append(due, r)
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].effectivePriority() != due[j].effectivePriority() {
			return due[i].effectivePriority() > due[j].effectivePriority()
		}
		return due[i].ID < due[j].ID
	})
	limit := s.powerLimitValueLocked()
	for _, r := range due {
		if s.committedPowerLoadLocked(now)+r.Power > limit+1e-9 {
			// 容量不足（例如停止确认迟到）：保持等待，不缩短、不超额。
			r.Status = PowerStatusHeld
			r.UpdatedAt = now
			continue
		}
		if err := s.dispatchPowerReservationLocked(r, now); err != nil {
			return err
		}
	}
	return nil
}

// dispatchPowerReservationLocked 把预约转成现有变更下发/确认流程：
// 生成带 start_at/end_at 的预约变更，由设备拉取、上报、确认。
func (s *Store) dispatchPowerReservationLocked(r *PowerReservation, now time.Time) error {
	start := now
	if r.PlannedStart != nil && r.PlannedStart.After(now) {
		start = *r.PlannedStart
	}
	start = start.UTC()
	end := start.Add(r.duration())
	s.changeSeq++
	ch := &Change{
		ID:              fmt.Sprintf("G%06d", s.changeSeq),
		Kind:            ChangeKindTarget,
		Targets:         map[string]Config{r.DeviceID: cloneConfig(r.Config)},
		BaselineTargets: map[string]Config{},
		DeliveredRevs:   map[string]int64{},
		DeviceResults:   map[string]ChangeDeviceResult{},
		Status:          StatusScheduled,
		Strategy:        ChangeStrategySupersede,
		StartAt:         &start,
		EndAt:           &end,
		DeviceEndsAt:    map[string]time.Time{r.DeviceID: end},
		Supersedes:      []string{},
		ReadyDevices:    []string{},
		WaitingDevices:  []string{r.DeviceID},
		CreatedAt:       now,
	}
	d := s.deviceLocked(r.DeviceID)
	ch.BaselineTargets[r.DeviceID] = cloneConfig(d.ConfirmedConfig)
	ch.DeviceResults[r.DeviceID] = ChangeDeviceResult{
		Status:        StatusScheduled,
		DispatchState: "SCHEDULED",
		WaitReason:    "WAITING_SCHEDULED_START",
		StartAt:       &start,
		EndAt:         &end,
		UpdatedAt:     now,
		Scheduled:     true,
	}
	s.changes[ch.ID] = ch
	s.refreshChangeStatusLocked(ch, now)
	r.ChangeID = ch.ID
	r.ActualStart = &start
	r.Status = PowerStatusDispatched
	r.UpdatedAt = now
	return nil
}
