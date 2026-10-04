package twin

import (
	"fmt"
	"sort"
	"time"
)

// syncLocked 把预约状态推进到 now：到点激活、到期安排恢复。
// 时间到了只代表“应当生效/应当恢复”，绝不直接当作设备已完成。
// 调用前必须持锁；若有状态变化会落盘，保证重启后可续作。
func (s *Store) syncLocked(now time.Time) {
	now = now.UTC()
	changed := false
	for _, r := range s.sortedReservationsLocked() {
		if r.ActivateChangeID == "" && !r.Expired && !now.Before(r.StartAt) && now.Before(r.EndAt) {
			s.activateReservationLocked(r, now)
			changed = true
		}
	}
	for _, r := range s.sortedReservationsLocked() {
		if r.Expired || now.Before(r.EndAt) {
			continue
		}
		r.Expired = true
		changed = true
		if r.ActivateChangeID != "" {
			s.wrapUpReservationDevicesLocked(r, nil, now)
		}
	}
	if changed {
		// 落盘失败不阻断内存态推进，下次写操作会再次持久化。
		_ = s.persistLocked()
	}
}

func (s *Store) sortedReservationsLocked() []*Reservation {
	out := make([]*Reservation, 0, len(s.reservations))
	for _, r := range s.reservations {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// activateReservationLocked 到点发布预约生效变更（SUPERSEDE），并把预约层叠到设备配置层上。
func (s *Store) activateReservationLocked(r *Reservation, now time.Time) {
	targets := map[string]Config{}
	for id, cfg := range r.Targets {
		if r.CancelledDevices[id] {
			continue
		}
		targets[id] = cfg
	}
	if len(targets) == 0 {
		r.Expired = true
		return
	}
	ch, err := s.publishChangeLocked(targets, PublishOptions{
		Kind:          ChangeKindReservation,
		Strategy:      ChangeStrategySupersede,
		ReservationID: r.ID,
	}, now)
	if err != nil {
		return
	}
	r.ActivateChangeID = ch.ID
	for _, id := range ch.ReadyDevices {
		s.layers[id] = append(s.layers[id], configLayer{ChangeID: ch.ID, ReservationID: r.ID, Config: cloneConfig(targets[id])})
	}
}

// wrapUpReservationDevicesLocked 为到期或被取消的设备安排收尾：
// 移除本预约的配置层，恢复到当时仍然有效的目标；已生效的效果在设备确认前保留。
// only 为 nil 表示处理预约内全部尚未收尾的设备。
func (s *Store) wrapUpReservationDevicesLocked(r *Reservation, only map[string]bool, now time.Time) {
	if r.RestoreChangeIDs == nil {
		r.RestoreChangeIDs = map[string]string{}
	}
	needPublish := map[string]Config{}
	for id := range r.Targets {
		if only != nil && !only[id] {
			continue
		}
		if _, done := r.RestoreChangeIDs[id]; done {
			continue
		}
		s.removeReservationLayerLocked(id, r.ID)
		target := s.restoreTargetLocked(id)
		d := s.devices[id]
		if d != nil && configEqual(d.DesiredConfig, target) {
			// 当前期望已经是恢复目标（例如被更新的操作取代），直接挂到在途变更上等待核实。
			r.RestoreChangeIDs[id] = d.DesiredChangeID
			continue
		}
		needPublish[id] = target
	}
	if len(needPublish) == 0 {
		return
	}
	ch, err := s.publishChangeLocked(needPublish, PublishOptions{
		Kind:          ChangeKindRestore,
		Strategy:      ChangeStrategySupersede,
		ReservationID: r.ID,
	}, now)
	if err != nil {
		return
	}
	for id := range needPublish {
		r.RestoreChangeIDs[id] = ch.ID
	}
}

// restoreTargetLocked 计算设备当前仍有效的目标：优先排队中的最新操作，
// 其次是配置层栈最上层（含仍在有效期内的其他预约），最后回退到已确认配置。
// 绝不机械恢复预约创建时的旧值。
func (s *Store) restoreTargetLocked(deviceID string) Config {
	if queued := s.queuedChangesForDeviceLocked(deviceID); len(queued) > 0 {
		return cloneConfig(queued[len(queued)-1].Targets[deviceID])
	}
	layers := s.layers[deviceID]
	if len(layers) > 0 {
		return cloneConfig(layers[len(layers)-1].Config)
	}
	if d := s.devices[deviceID]; d != nil {
		return cloneConfig(d.ConfirmedConfig)
	}
	return Config{}
}

func (s *Store) setBaseLayerLocked(deviceID, changeID string, cfg Config) {
	out := make([]configLayer, 0, len(s.layers[deviceID])+1)
	out = append(out, configLayer{ChangeID: changeID, Config: cloneConfig(cfg)})
	for _, l := range s.layers[deviceID] {
		if l.ReservationID == "" {
			continue
		}
		out = append(out, l)
	}
	s.layers[deviceID] = out
}

func (s *Store) removeReservationLayerLocked(deviceID, reservationID string) {
	layers := s.layers[deviceID]
	out := layers[:0]
	for _, l := range layers {
		if l.ReservationID == reservationID {
			continue
		}
		out = append(out, l)
	}
	s.layers[deviceID] = out
}

// CreateReservation 创建带有效期的预约变更，结束时间必须晚于开始时间。
func (s *Store) CreateReservation(targets map[string]Config, startAt, endAt time.Time, now time.Time) (*ReservationView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked(now)
	targets, err := normalizeTargets(targets)
	if err != nil {
		return nil, err
	}
	startAt, endAt = startAt.UTC(), endAt.UTC()
	if !endAt.After(startAt) {
		return nil, fmt.Errorf("预约结束时间必须晚于开始时间")
	}
	s.reservationSeq++
	r := &Reservation{
		ID:               fmt.Sprintf("R%06d", s.reservationSeq),
		Targets:          targets,
		StartAt:          startAt,
		EndAt:            endAt,
		RestoreChangeIDs: map[string]string{},
		CancelledDevices: map[string]bool{},
		CreatedAt:        now.UTC(),
	}
	s.reservations[r.ID] = r
	s.syncLocked(now)
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return s.reservationViewLocked(r, now), nil
}

// ShortenReservation 缩短预约有效期；缩短到过去等价于立即到期并安排恢复。
func (s *Store) ShortenReservation(id string, newEnd time.Time, now time.Time) (*ReservationView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked(now)
	r, ok := s.reservations[id]
	if !ok {
		return nil, fmt.Errorf("预约 %s 不存在", id)
	}
	newEnd = newEnd.UTC()
	if newEnd.After(r.EndAt) {
		return nil, fmt.Errorf("仅支持缩短有效期，新结束时间不能晚于 %s", r.EndAt.Format(time.RFC3339))
	}
	r.EndAt = newEnd
	s.syncLocked(now)
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return s.reservationViewLocked(r, now), nil
}

// CancelReservationDevices 取消预约中的部分设备。
// 尚未生效的设备直接取消；已生效的设备保留当前效果并安排恢复收尾。
func (s *Store) CancelReservationDevices(id string, devices []string, now time.Time) (*ReservationView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked(now)
	r, ok := s.reservations[id]
	if !ok {
		return nil, fmt.Errorf("预约 %s 不存在", id)
	}
	if len(devices) == 0 {
		return nil, fmt.Errorf("取消设备列表不能为空")
	}
	for _, dev := range devices {
		if _, ok := r.Targets[dev]; !ok {
			return nil, fmt.Errorf("设备 %s 不在预约 %s 中", dev, id)
		}
	}
	wrapUp := map[string]bool{}
	if r.CancelledDevices == nil {
		r.CancelledDevices = map[string]bool{}
	}
	for _, dev := range devices {
		if r.CancelledDevices[dev] {
			continue
		}
		if _, done := r.RestoreChangeIDs[dev]; done {
			continue
		}
		r.CancelledDevices[dev] = true
		if r.ActivateChangeID != "" && !r.Expired {
			wrapUp[dev] = true
		}
	}
	if len(wrapUp) > 0 {
		s.wrapUpReservationDevicesLocked(r, wrapUp, now)
	}
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return s.reservationViewLocked(r, now), nil
}

func (s *Store) Reservation(id string, now time.Time) (*ReservationView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked(now)
	r, ok := s.reservations[id]
	if !ok {
		return nil, false
	}
	return s.reservationViewLocked(r, now), true
}

func (s *Store) ListReservations(now time.Time) []*ReservationView {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked(now)
	out := make([]*ReservationView, 0, len(s.reservations))
	for _, r := range s.sortedReservationsLocked() {
		out = append(out, s.reservationViewLocked(r, now))
	}
	return out
}

func (s *Store) reservationViewLocked(r *Reservation, now time.Time) *ReservationView {
	cp := *r
	cp.Targets = map[string]Config{}
	for id, cfg := range r.Targets {
		cp.Targets[id] = cloneConfig(cfg)
	}
	cp.RestoreChangeIDs = map[string]string{}
	for id, chID := range r.RestoreChangeIDs {
		cp.RestoreChangeIDs[id] = chID
	}
	cp.CancelledDevices = map[string]bool{}
	for id, v := range r.CancelledDevices {
		cp.CancelledDevices[id] = v
	}
	view := &ReservationView{Reservation: &cp, Devices: []ReservationDeviceView{}}
	ids := make([]string, 0, len(r.Targets))
	for id := range r.Targets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	phases := make([]ReservationPhase, 0, len(ids))
	for _, id := range ids {
		phase, reason := s.devicePhaseLocked(r, id)
		d := s.devices[id]
		online := d != nil && d.Online
		dv := ReservationDeviceView{
			DeviceID:         id,
			Online:           online,
			Phase:            phase,
			PhaseText:        reservationPhaseText(phase),
			WaitReason:       reason,
			ActivateChangeID: r.ActivateChangeID,
			RestoreChangeID:  r.RestoreChangeIDs[id],
		}
		view.Devices = append(view.Devices, dv)
		phases = append(phases, phase)
	}
	view.Status = reservationOverallStatus(phases)
	view.StatusText = reservationStatusText(view.Status)
	return view
}

// devicePhaseLocked 推导单台设备的预约进度：已安排收尾的看恢复变更，
// 否则看生效变更；时间到但未确认一律是“应当生效/等待核实”，不是完成。
func (s *Store) devicePhaseLocked(r *Reservation, deviceID string) (ReservationPhase, string) {
	if chID, ok := r.RestoreChangeIDs[deviceID]; ok {
		if chID == "" {
			return PhaseRestored, ""
		}
		return s.phaseFromChangeLocked(chID, deviceID, PhaseRestoreDue, PhaseRestored)
	}
	if r.CancelledDevices[deviceID] {
		return PhaseCancelled, ""
	}
	if r.ActivateChangeID != "" {
		return s.phaseFromChangeLocked(r.ActivateChangeID, deviceID, PhaseDue, PhaseConfirmed)
	}
	if r.Expired {
		return PhaseSkipped, "预约在生效前已到期，未执行"
	}
	return PhaseWaitingStart, "未到预约开始时间"
}

func (s *Store) phaseFromChangeLocked(changeID, deviceID string, pending, done ReservationPhase) (ReservationPhase, string) {
	ch := s.changes[changeID]
	if ch == nil {
		return done, ""
	}
	result, ok := ch.DeviceResults[deviceID]
	if ok && result.Status == StatusEffective {
		return done, ""
	}
	d := s.devices[deviceID]
	offline := d == nil || !d.Online
	restore := pending == PhaseRestoreDue
	if ok && result.Status == StatusDelivered {
		if restore {
			return pending, "恢复目标已下发，等待设备核实"
		}
		return pending, "预约目标已下发，等待设备确认生效"
	}
	if offline {
		if restore {
			return pending, "设备离线，等待重连后核实恢复目标"
		}
		return pending, "设备离线，等待重连后拉取预约目标"
	}
	if restore {
		return pending, "等待设备拉取恢复目标"
	}
	return pending, "等待设备拉取预约目标"
}

func reservationOverallStatus(phases []ReservationPhase) ReservationStatus {
	if len(phases) == 0 {
		return ReservationCancelled
	}
	allCancelled, allDone := true, true
	anyRestore, anyActive := false, false
	for _, p := range phases {
		if p != PhaseCancelled {
			allCancelled = false
		}
		if p != PhaseRestored && p != PhaseCancelled && p != PhaseSkipped {
			allDone = false
		}
		if p == PhaseRestoreDue {
			anyRestore = true
		}
		if p == PhaseDue || p == PhaseConfirmed {
			anyActive = true
		}
	}
	switch {
	case allCancelled:
		return ReservationCancelled
	case allDone:
		return ReservationCompleted
	case anyRestore:
		return ReservationRestoring
	case anyActive:
		return ReservationActive
	default:
		return ReservationScheduled
	}
}

func reservationPhaseText(p ReservationPhase) string {
	switch p {
	case PhaseWaitingStart:
		return "等待预约开始"
	case PhaseDue:
		return "应当生效，等待设备确认"
	case PhaseConfirmed:
		return "已确认生效"
	case PhaseRestoreDue:
		return "已到期，等待设备核实恢复"
	case PhaseRestored:
		return "已恢复确认"
	case PhaseCancelled:
		return "已取消"
	case PhaseSkipped:
		return "已过期未执行"
	default:
		return string(p)
	}
}

func reservationStatusText(st ReservationStatus) string {
	switch st {
	case ReservationScheduled:
		return "已预约，等待开始"
	case ReservationActive:
		return "生效窗口内，等待设备确认"
	case ReservationRestoring:
		return "已到期，恢复收尾中"
	case ReservationCompleted:
		return "已完成"
	case ReservationCancelled:
		return "已取消"
	default:
		return string(st)
	}
}
