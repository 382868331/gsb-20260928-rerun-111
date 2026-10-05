package twin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Store struct {
	mu          sync.Mutex
	path        string
	devices     map[string]*Device
	conflicts   map[string]*Conflict
	conflictSeq int64
	changes     map[string]*Change
	changeSeq   int64
	clock       *time.Time
	// 站点级功率调度状态。
	reservations   map[string]*PowerReservation
	reservationSeq int64
	powerLimitKW   float64
	powerLimitSet  bool
	powerOverloads []PowerOverloadView
}

type snapshot struct {
	Devices        []*Device           `json:"devices"`
	Conflicts      []*Conflict         `json:"conflicts"`
	ConflictSeq    int64               `json:"conflict_seq"`
	Changes        []*Change           `json:"changes"`
	ChangeSeq      int64               `json:"change_seq"`
	Clock          *time.Time          `json:"clock,omitempty"`
	Reservations   []*PowerReservation `json:"reservations,omitempty"`
	ReservationSeq int64               `json:"reservation_seq,omitempty"`
	PowerLimitKW   float64             `json:"power_limit_kw,omitempty"`
	PowerLimitSet  bool                `json:"power_limit_set,omitempty"`
	PowerOverloads []PowerOverloadView `json:"power_overloads,omitempty"`
}

type PublishOptions struct {
	Kind           ChangeKind
	CompensatesFor string
	Strategy       ChangeStrategy
	StartAt        *time.Time
	EndAt          *time.Time
}

func NewStore(path string) (*Store, error) {
	s := &Store{
		path:         path,
		devices:      map[string]*Device{},
		conflicts:    map[string]*Conflict{},
		changes:      map[string]*Change{},
		reservations: map[string]*PowerReservation{},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}
	for _, d := range snap.Devices {
		s.devices[d.ID] = d
	}
	for _, c := range snap.Conflicts {
		s.conflicts[c.ID] = c
	}
	s.conflictSeq = snap.ConflictSeq
	for _, ch := range snap.Changes {
		s.changes[ch.ID] = ch
	}
	s.changeSeq = snap.ChangeSeq
	s.clock = snap.Clock
	for _, r := range snap.Reservations {
		s.reservations[r.ID] = r
	}
	s.reservationSeq = snap.ReservationSeq
	s.powerLimitKW = snap.PowerLimitKW
	s.powerLimitSet = snap.PowerLimitSet
	s.powerOverloads = snap.PowerOverloads
	return nil
}

// persist 必须在持锁状态下调用，先写临时文件再原子改名，保证重启后状态完整。
func (s *Store) persistLocked() error {
	snap := snapshot{ConflictSeq: s.conflictSeq}
	for _, d := range s.devices {
		snap.Devices = append(snap.Devices, d)
	}
	sort.Slice(snap.Devices, func(i, j int) bool { return snap.Devices[i].ID < snap.Devices[j].ID })
	for _, c := range s.conflicts {
		snap.Conflicts = append(snap.Conflicts, c)
	}
	sort.Slice(snap.Conflicts, func(i, j int) bool { return snap.Conflicts[i].ID < snap.Conflicts[j].ID })
	snap.ConflictSeq = s.conflictSeq
	snap.ChangeSeq = s.changeSeq
	snap.Clock = s.clock
	snap.ReservationSeq = s.reservationSeq
	snap.PowerLimitKW = s.powerLimitKW
	snap.PowerLimitSet = s.powerLimitSet
	snap.PowerOverloads = s.powerOverloads
	for _, r := range s.reservations {
		snap.Reservations = append(snap.Reservations, r)
	}
	sort.Slice(snap.Reservations, func(i, j int) bool { return snap.Reservations[i].ID < snap.Reservations[j].ID })
	for _, ch := range s.changes {
		snap.Changes = append(snap.Changes, ch)
	}
	sort.Slice(snap.Changes, func(i, j int) bool { return snap.Changes[i].ID < snap.Changes[j].ID })

	data, err := json.MarshalIndent(&snap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) deviceLocked(id string) *Device {
	d, ok := s.devices[id]
	if !ok {
		d = &Device{ID: id}
		s.devices[id] = d
	}
	return d
}

func (s *Store) addConflictLocked(deviceID string, reason ConflictReason, rev int64, detail Config, now time.Time) *Conflict {
	s.conflictSeq++
	c := &Conflict{
		ID:        fmt.Sprintf("C%06d", s.conflictSeq),
		DeviceID:  deviceID,
		Reason:    reason,
		Rev:       rev,
		Detail:    detail,
		Open:      true,
		CreatedAt: now.UTC(),
	}
	s.conflicts[c.ID] = c
	return c
}

// SetDesired 写入单设备目标，兼容旧接口；内部仍作为一个整体变更发布。
func (s *Store) SetDesired(deviceID string, cfg Config, now time.Time) (*Device, int64, error) {
	ch, err := s.PublishChangeWithOptions(map[string]Config{deviceID: cfg}, PublishOptions{
		Kind:     ChangeKindTarget,
		Strategy: ChangeStrategySupersede,
	}, now)
	if err != nil {
		return nil, 0, err
	}
	s.mu.Lock()
	d := s.devices[deviceID]
	rev := ch.DeviceResults[deviceID].DesiredRev
	s.mu.Unlock()
	return d, rev, nil
}

func (s *Store) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clock != nil {
		return *s.clock
	}
	return time.Now().UTC()
}

func (s *Store) nowLocked() time.Time {
	if s.clock != nil {
		return *s.clock
	}
	return time.Now().UTC()
}

func (s *Store) SetVirtualClock(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := t.UTC()
	s.clock = &now
	if err := s.processDueLocked(now); err != nil {
		return err
	}
	return s.persistLocked()
}

func (s *Store) ClearVirtualClock() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = nil
	return s.persistLocked()
}

func (s *Store) CancelReservation(changeID string, devices []string, now time.Time) (*Change, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	if err := s.processDueLocked(now); err != nil {
		return nil, err
	}
	ch, ok := s.changes[changeID]
	if !ok {
		return nil, fmt.Errorf("变更 %s 不存在", changeID)
	}
	if ch.StartAt == nil {
		return nil, fmt.Errorf("变更 %s 不是预约", changeID)
	}
	targets := deviceSet(devices, ch.Targets)
	if len(targets) == 0 {
		return nil, fmt.Errorf("没有可操作的预约设备")
	}
	cancelled := now
	for id := range targets {
		result := ch.DeviceResults[id]
		switch result.Status {
		case StatusScheduled:
			result.Status = StatusCancelled
			result.DispatchState = "CANCELLED"
			result.EndReason = "CANCELLED"
			result.WaitReason = ""
			result.EndAt = &cancelled
			result.UpdatedAt = now
			ch.DeviceResults[id] = result
			delete(ch.DeviceEndsAt, id)
		case StatusPending, StatusDelivered, StatusEffective, StatusPartial, StatusFailed:
			if err := s.finishReservationDeviceLocked(ch, id, now, "CANCELLED"); err != nil {
				return nil, err
			}
			result = ch.DeviceResults[id]
			result.EndReason = "CANCELLED"
			ch.DeviceResults[id] = result
			delete(ch.DeviceEndsAt, id)
		default:
			return nil, fmt.Errorf("设备 %s 的预约已结束，不能取消", id)
		}
	}
	if allDeviceResults(ch, StatusCancelled) {
		ch.CancelledAt = &cancelled
	}
	s.refreshChangeStatusLocked(ch, now)
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return cloneChange(ch), nil
}

func (s *Store) ShortenReservation(changeID string, endAt time.Time, devices []string, now time.Time) (*Change, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	endAt = endAt.UTC()
	if err := s.processDueLocked(now); err != nil {
		return nil, err
	}
	ch, ok := s.changes[changeID]
	if !ok || ch.StartAt == nil {
		return nil, fmt.Errorf("预约 %s 不存在", changeID)
	}
	if !endAt.After(now) {
		return nil, fmt.Errorf("新的有效期必须晚于当前时间")
	}
	targets := deviceSet(devices, ch.Targets)
	if len(targets) == 0 {
		return nil, fmt.Errorf("没有可缩短的预约设备")
	}
	for id := range targets {
		result := ch.DeviceResults[id]
		if result.EndAt == nil {
			return nil, fmt.Errorf("设备 %s 的预约没有有效期", id)
		}
		if !endAt.Before(*result.EndAt) {
			return nil, fmt.Errorf("只能缩短，不能延长设备 %s 的有效期", id)
		}
		result.EndAt = &endAt
		result.UpdatedAt = now
		ch.DeviceResults[id] = result
		ch.DeviceEndsAt[id] = endAt
	}
	if err := s.processDueLocked(now); err != nil {
		return nil, err
	}
	s.refreshChangeStatusLocked(ch, now)
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return cloneChange(ch), nil
}

func deviceSet(devices []string, targets map[string]Config) map[string]bool {
	out := map[string]bool{}
	if len(devices) == 0 {
		for id := range targets {
			out[id] = true
		}
		return out
	}
	for _, id := range devices {
		if _, ok := targets[id]; ok {
			out[id] = true
		}
	}
	return out
}

func allDeviceResults(ch *Change, want Status) bool {
	if len(ch.DeviceResults) == 0 {
		return false
	}
	for _, result := range ch.DeviceResults {
		if result.Status != want {
			return false
		}
	}
	return true
}

func (s *Store) processDueLocked(now time.Time) error {
	// 正常时钟与虚拟时钟走同一套到期处理：有效时间取两者较大值；
	// 只有启用了虚拟时钟时才推进持久化时钟。
	effective := now
	if s.clock != nil {
		if now.Before(*s.clock) {
			effective = *s.clock
		} else {
			s.clock = timePtrUTC(now)
		}
	}
	for {
		changed, err := s.runDueOnceLocked(effective)
		if err != nil {
			return err
		}
		if !changed {
			break
		}
	}
	// 功率预约与正常/虚拟时钟走同一套规则：到点即进入下发与确认流程；
	// 容量不足（如停止目标未确认）时先重排，绝不提前超额启动。
	s.syncPowerStatesLocked(effective)
	blocked, err := s.dispatchDuePowerLocked(effective)
	if err != nil {
		return err
	}
	if blocked {
		if err := s.replanPowerLocked(effective); err != nil {
			return err
		}
		if _, err := s.dispatchDuePowerLocked(effective); err != nil {
			return err
		}
	}
	return nil
}

func timePtrUTC(t time.Time) *time.Time {
	u := t.UTC()
	return &u
}

type dueEvent struct {
	at       time.Time
	changeID string
	deviceID string
	kind     string
}

func (s *Store) runDueOnceLocked(now time.Time) (bool, error) {
	events := make([]dueEvent, 0)
	for _, ch := range s.changes {
		for id, result := range ch.DeviceResults {
			if result.Scheduled && result.Status == StatusScheduled && result.StartAt != nil && !result.StartAt.After(now) {
				events = append(events, dueEvent{at: *result.StartAt, changeID: ch.ID, deviceID: id, kind: "start"})
			}
			if result.EndAt != nil && !result.EndAt.After(now) {
				if result.EndReason != "" {
					continue
				}
				switch result.Status {
				case StatusPending, StatusDelivered, StatusEffective, StatusPartial, StatusFailed:
					events = append(events, dueEvent{at: *result.EndAt, changeID: ch.ID, deviceID: id, kind: "end"})
				}
			}
		}
	}
	if len(events) == 0 {
		return false, nil
	}
	sort.Slice(events, func(i, j int) bool {
		if !events[i].at.Equal(events[j].at) {
			return events[i].at.Before(events[j].at)
		}
		if events[i].kind != events[j].kind {
			return events[i].kind == "start"
		}
		if events[i].changeID != events[j].changeID {
			return events[i].changeID < events[j].changeID
		}
		return events[i].deviceID < events[j].deviceID
	})
	event := events[0]
	ch := s.changes[event.changeID]
	if event.kind == "start" {
		s.activateScheduledDeviceLocked(ch, event.deviceID, event.at)
	} else {
		if err := s.finishReservationDeviceLocked(ch, event.deviceID, event.at, "EXPIRED"); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (s *Store) activateScheduledDeviceLocked(ch *Change, deviceID string, now time.Time) {
	d := s.deviceLocked(deviceID)
	if active := s.currentChangeForDeviceLocked(deviceID); active != nil {
		if active.StartAt != nil {
			s.coverDeviceInChangeLocked(active, deviceID, now)
		} else {
			s.supersedeDeviceInChangeLocked(active, deviceID, now)
			ch.Supersedes = appendUnique(ch.Supersedes, active.ID)
		}
	}
	if current := s.changes[d.DesiredChangeID]; current != nil && current.ID != ch.ID && current.StartAt != nil {
		if result := current.DeviceResults[deviceID]; result.Status == StatusEffective && result.EndAt != nil && result.EndAt.After(now) {
			s.coverDeviceInChangeLocked(current, deviceID, now)
		}
	}
	for _, queued := range s.queuedChangesForDeviceLocked(deviceID) {
		s.supersedeDeviceInChangeLocked(queued, deviceID, now)
		ch.Supersedes = appendUnique(ch.Supersedes, queued.ID)
	}
	scheduledResult := ch.DeviceResults[deviceID]
	creationBaseline := cloneConfig(ch.BaselineTargets[deviceID])
	s.assignTargetToDeviceLocked(ch, deviceID, ch.Targets[deviceID], d, now, nil, scheduledResult.EndAt)
	ch.BaselineTargets[deviceID] = creationBaseline
	result := ch.DeviceResults[deviceID]
	result.StartAt = scheduledResult.StartAt
	ch.DeviceResults[deviceID] = result
	s.refreshChangeStatusLocked(ch, now)
}

func (s *Store) coverDeviceInChangeLocked(ch *Change, deviceID string, now time.Time) {
	result := ch.DeviceResults[deviceID]
	result.DispatchState = "OVERLAPPED"
	result.WaitReason = "WAITING_OVERLAPPING_RESERVATION"
	result.UpdatedAt = now
	ch.DeviceResults[deviceID] = result
	s.refreshChangeStatusLocked(ch, now)
}

func (s *Store) finishReservationDeviceLocked(reservation *Change, deviceID string, now time.Time, reason string) error {
	d := s.deviceLocked(deviceID)
	result := reservation.DeviceResults[deviceID]
	currentReservation := d.DesiredChangeID == reservation.ID && result.DesiredRev == d.DesiredRev
	if current := s.changes[d.DesiredChangeID]; current != nil && current.Kind == ChangeKindRestoration && current.RestoresToChange == reservation.ID {
		currentReservation = true
	}
	result.EndReason = reason
	result.WaitReason = "WAITING_RESTORE_CONFIRMATION"
	if result.Status == StatusEffective {
		if reason == "CANCELLED" {
			result.DispatchState = "CANCELLED_EFFECTIVE"
		} else if reason == "INTERRUPTED" {
			result.DispatchState = "INTERRUPTED_EFFECTIVE"
		} else {
			result.DispatchState = "EXPIRED_EFFECTIVE"
		}
	} else {
		if reason == "CANCELLED" {
			result.Status = StatusCancelled
			result.DispatchState = "CANCELLED"
		} else if reason == "INTERRUPTED" {
			result.Status = StatusExpired
			result.DispatchState = "INTERRUPTED"
		} else {
			result.Status = StatusExpired
			result.DispatchState = "EXPIRED"
		}
	}
	result.UpdatedAt = now
	reservation.DeviceResults[deviceID] = result
	s.refreshChangeStatusLocked(reservation, now)
	_, activeTargetChange, _ := s.restoreTargetLocked(deviceID, reservation, now)
	if currentReservation {
		target, targetChange, ok := s.restoreTargetLocked(deviceID, reservation, now)
		activeTargetChange = targetChange
		if ok {
			if err := s.createRestorationLocked(deviceID, target, targetChange, reservation, now); err != nil {
				return err
			}
		} else {
			result = reservation.DeviceResults[deviceID]
			result.WaitReason = ""
			reservation.DeviceResults[deviceID] = result
		}
	} else if d.DesiredChangeID == activeTargetChange.ID {
		result = reservation.DeviceResults[deviceID]
		result.WaitReason = "WAITING_OVERLAPPING_RESERVATION_CONFIRMATION"
		reservation.DeviceResults[deviceID] = result
	}
	if activeTargetChange != nil {
		reservation.RestoresToChange = activeTargetChange.ID
	}
	return nil
}

func (s *Store) createRestorationLocked(deviceID string, target Config, targetChange *Change, reservation *Change, now time.Time) error {
	s.changeSeq++
	restore := &Change{
		ID:               fmt.Sprintf("G%06d", s.changeSeq),
		Kind:             ChangeKindRestoration,
		Targets:          map[string]Config{deviceID: cloneConfig(target)},
		BaselineTargets:  map[string]Config{},
		DeliveredRevs:    map[string]int64{},
		DeviceResults:    map[string]ChangeDeviceResult{},
		Status:           StatusPending,
		Strategy:         ChangeStrategySupersede,
		Supersedes:       []string{},
		ReadyDevices:     []string{},
		WaitingDevices:   []string{},
		RestoresToChange: targetChange.ID,
		CreatedAt:        now,
	}
	d := s.deviceLocked(deviceID)
	if previous := s.changes[d.DesiredChangeID]; previous != nil && previous.ID != reservation.ID {
		if previous.StartAt != nil {
			s.coverDeviceInChangeLocked(previous, deviceID, now)
		} else {
			s.supersedeDeviceInChangeLocked(previous, deviceID, now)
			restore.Supersedes = appendUnique(restore.Supersedes, previous.ID)
		}
	}
	result := reservation.DeviceResults[deviceID]
	if result.Status == StatusEffective {
		if result.EndReason == "CANCELLED" {
			result.DispatchState = "CANCELLED_EFFECTIVE"
		} else if result.EndReason == "INTERRUPTED" {
			result.DispatchState = "INTERRUPTED_EFFECTIVE"
		} else {
			result.DispatchState = "EXPIRED_EFFECTIVE"
		}
	} else if result.EndReason == "CANCELLED" {
		result.Status = StatusCancelled
		result.DispatchState = "CANCELLED"
	} else if result.EndReason == "INTERRUPTED" {
		result.Status = StatusExpired
		result.DispatchState = "INTERRUPTED"
	} else {
		result.Status = StatusExpired
		result.DispatchState = "EXPIRED"
	}
	result.RestoresFor = restore.ID
	result.WaitReason = "WAITING_RESTORE_CONFIRMATION"
	reservation.DeviceResults[deviceID] = result
	s.assignTargetToDeviceLocked(restore, deviceID, target, d, now, nil, nil)
	r := restore.DeviceResults[deviceID]
	r.RestoresFor = reservation.ID
	restore.DeviceResults[deviceID] = r
	restore.ReadyDevices = []string{deviceID}
	reservation.RestoreChanges = appendUnique(reservation.RestoreChanges, restore.ID)
	s.refreshChangeStatusLocked(restore, now)
	s.refreshChangeStatusLocked(reservation, now)
	s.changes[restore.ID] = restore
	return nil
}

func (s *Store) restoreTargetLocked(deviceID string, expiring *Change, now time.Time) (Config, *Change, bool) {
	candidates := make([]*Change, 0)
	for _, ch := range s.changes {
		if ch.ID == expiring.ID || ch.CancelledAt != nil || ch.Kind == ChangeKindRestoration {
			continue
		}
		result, ok := ch.DeviceResults[deviceID]
		if !ok {
			continue
		}
		target, ok := ch.Targets[deviceID]
		if !ok {
			continue
		}
		if ch.StartAt != nil {
			start := result.StartAt
			if start == nil {
				start = ch.StartAt
			}
			end := result.EndAt
			if end == nil {
				end = ch.EndAt
			}
			if start == nil || start.After(now) || (end != nil && !end.After(now)) {
				continue
			}
		}
		switch result.Status {
		case StatusScheduled, StatusSuperseded, StatusCancelled:
			continue
		}
		_ = target
		candidates = append(candidates, ch)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if (candidates[i].StartAt != nil) != (candidates[j].StartAt != nil) {
			return candidates[i].StartAt != nil
		}
		return candidates[i].CreatedAt.After(candidates[j].CreatedAt)
	})
	if len(candidates) > 0 {
		return cloneConfig(candidates[0].Targets[deviceID]), candidates[0], true
	}
	fallback := cloneConfig(expiring.BaselineTargets[deviceID])
	if _, exists := expiring.BaselineTargets[deviceID]; !exists {
		fallback = s.restoreFallbackLocked(deviceID)
	}
	return fallback, expiring, true
}

func (s *Store) restoreFallbackLocked(deviceID string) Config {
	d := s.devices[deviceID]
	if d == nil {
		return Config{}
	}
	return cloneConfig(d.ConfirmedConfig)
}

// PreviewChange 只计算新目标与各设备当前期望/观察/确认版本之间的差异，不发布、不落盘。
func (s *Store) PreviewChange(targets map[string]Config, now time.Time) (*Preview, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	if err := s.processDueLocked(now); err != nil {
		return nil, err
	}
	targets, err := normalizeTargets(targets)
	if err != nil {
		return nil, err
	}
	p := &Preview{Targets: targets, Diffs: make([]DeviceDiff, 0, len(targets))}
	ids := make([]string, 0, len(targets))
	for id := range targets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		d := s.devices[id]
		if d == nil {
			d = &Device{ID: id}
		}
		target := targets[id]
		diff := DeviceDiff{
			DeviceID:      id,
			CurrentRev:    d.DesiredRev,
			CurrentConfig: cloneConfig(d.DesiredConfig),
			TargetConfig:  cloneConfig(target),
			Added:         Config{},
			Updated:       Config{},
			ConfirmedRev:  d.ConfirmedRev,
			ReportedRev:   d.ReportedRev,
			CurrentStatus: d.Status(now),
			Ready:         !s.deviceOperationActiveLocked(d),
		}
		if active := s.currentChangeForDeviceLocked(id); active != nil {
			diff.WaitForChange = active.ID
		}
		for k, v := range target {
			old, ok := d.DesiredConfig[k]
			if !ok {
				diff.Added[k] = v
			} else if old != v {
				diff.Updated[k] = v
			}
		}
		for k := range d.DesiredConfig {
			if _, ok := target[k]; !ok {
				diff.Removed = append(diff.Removed, k)
			}
		}
		sort.Strings(diff.Removed)
		p.Diffs = append(p.Diffs, diff)
	}
	return p, nil
}

// PublishChange 发布一组相关配置。任一设备都只确认本设备目标，不能替变更中的其他设备背书。
func (s *Store) PublishChange(targets map[string]Config, kind ChangeKind, compensatesFor string, now time.Time) (*Change, error) {
	return s.PublishChangeWithOptions(targets, PublishOptions{Kind: kind, CompensatesFor: compensatesFor, Strategy: ChangeStrategyQueue}, now)
}

// PublishChangeWithOptions 发布一组相关配置。默认排队；只有显式 SUPERSEDE 才抢占设备当前操作。
func (s *Store) PublishChangeWithOptions(targets map[string]Config, opts PublishOptions, now time.Time) (*Change, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	if err := s.processDueLocked(now); err != nil {
		return nil, err
	}
	ch, err := s.publishChangeLocked(targets, opts, now)
	if err != nil {
		return nil, err
	}
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return cloneChange(ch), nil
}

// publishChangeLocked 是持锁状态下的发布实现，供功率调度等到点下发复用。
func (s *Store) publishChangeLocked(targets map[string]Config, opts PublishOptions, now time.Time) (*Change, error) {
	var startAt, endAt *time.Time
	if opts.StartAt != nil {
		t := opts.StartAt.UTC()
		startAt = &t
	}
	if opts.EndAt != nil {
		t := opts.EndAt.UTC()
		endAt = &t
	}
	if startAt != nil && startAt.Before(now) {
		return nil, fmt.Errorf("预约开始时间不能早于当前时间")
	}
	if endAt != nil {
		lower := now
		if startAt != nil {
			lower = *startAt
		}
		if !endAt.After(lower) {
			return nil, fmt.Errorf("预约结束时间必须晚于开始时间")
		}
	}
	targets, err := normalizeTargets(targets)
	if err != nil {
		return nil, err
	}
	kind := opts.Kind
	if kind == "" {
		kind = ChangeKindTarget
	}
	strategy := opts.Strategy
	if strategy == "" {
		strategy = ChangeStrategyQueue
	}
	if strategy != ChangeStrategyQueue && strategy != ChangeStrategySupersede {
		return nil, fmt.Errorf("未知变更策略 %q", strategy)
	}
	if kind == ChangeKindCompensation {
		if opts.CompensatesFor == "" {
			return nil, fmt.Errorf("补偿变更必须指定原变更")
		}
		if _, ok := s.changes[opts.CompensatesFor]; !ok {
			return nil, fmt.Errorf("原变更 %s 不存在", opts.CompensatesFor)
		}
	}
	s.changeSeq++
	ch := &Change{
		ID:              fmt.Sprintf("G%06d", s.changeSeq),
		Kind:            kind,
		Targets:         targets,
		BaselineTargets: map[string]Config{},
		DeliveredRevs:   map[string]int64{},
		DeviceResults:   map[string]ChangeDeviceResult{},
		Strategy:        strategy,
		ReadyDevices:    []string{},
		WaitingDevices:  []string{},
		CreatedAt:       now,
	}
	if startAt != nil {
		ch.StartAt = startAt
	}
	if endAt != nil {
		ch.EndAt = endAt
		ch.DeviceEndsAt = map[string]time.Time{}
	}
	if kind == ChangeKindCompensation {
		ch.CompensatesFor = opts.CompensatesFor
	}

	ids := make([]string, 0, len(targets))
	for id := range targets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		d := s.deviceLocked(id)
		if startAt != nil {
			ch.BaselineTargets[id] = cloneConfig(d.ConfirmedConfig)
			r := ChangeDeviceResult{
				Status:        StatusScheduled,
				DispatchState: "SCHEDULED",
				WaitReason:    "WAITING_SCHEDULED_START",
				StartAt:       startAt,
				EndAt:         endAt,
				UpdatedAt:     now,
				Scheduled:     true,
			}
			ch.DeviceResults[id] = r
			ch.WaitingDevices = append(ch.WaitingDevices, id)
			if endAt != nil {
				ch.DeviceEndsAt[id] = *endAt
			}
			continue
		}
		previousID := d.DesiredChangeID
		ch.BaselineTargets[id] = cloneConfig(d.ConfirmedConfig)
		active := s.currentChangeForDeviceLocked(id)
		if active != nil && strategy == ChangeStrategySupersede {
			s.supersedeDeviceInChangeLocked(active, id, now)
			ch.Supersedes = appendUnique(ch.Supersedes, active.ID)
			for _, queued := range s.queuedChangesForDeviceLocked(id) {
				s.supersedeDeviceInChangeLocked(queued, id, now)
				ch.Supersedes = appendUnique(ch.Supersedes, queued.ID)
			}
			active = nil
		}
		if active != nil {
			ch.DeviceResults[id] = ChangeDeviceResult{
				Status:        StatusQueued,
				DispatchState: "WAITING",
				WaitForChange: active.ID,
				UpdatedAt:     now,
			}
			ch.WaitingDevices = append(ch.WaitingDevices, id)
			continue
		}
		if d.DesiredRev > 0 && d.DeliveredRev >= d.DesiredRev && s.deviceOperationActiveLocked(d) {
			c := s.addConflictLocked(id, ReasonSuperseded, d.DesiredRev, d.DesiredConfig, now)
			c.Open = false
			c.ResolvedAt = &now
		}
		s.assignTargetToDeviceLocked(ch, id, targets[id], d, now, nil, endAt)
		if endAt != nil {
			ch.DeviceEndsAt[id] = *endAt
		}
		if previousID != "" && previousID != ch.ID {
			if previous := s.changes[previousID]; previous != nil {
				s.supersedeDeviceInChangeLocked(previous, id, now)
				ch.Supersedes = appendUnique(ch.Supersedes, previousID)
			}
		}
		ch.ReadyDevices = append(ch.ReadyDevices, id)
	}
	sort.Strings(ch.Supersedes)
	s.refreshChangeStatusLocked(ch, now)
	s.changes[ch.ID] = ch
	return ch, nil
}

// PublishCompensation 以各设备上一份精确确认状态作为默认补偿目标；也可显式给出补偿配置。
func (s *Store) PublishCompensation(changeID string, targets map[string]Config, now time.Time) (*Change, error) {
	s.mu.Lock()
	base, ok := s.changes[changeID]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("变更 %s 不存在", changeID)
	}
	if targets == nil {
		targets = map[string]Config{}
	}
	for id := range base.Targets {
		if _, provided := targets[id]; provided {
			continue
		}
		if base, ok := base.BaselineTargets[id]; ok {
			targets[id] = cloneConfig(base)
			continue
		}
		d := s.devices[id]
		if d == nil || len(d.ConfirmedConfig) == 0 {
			s.mu.Unlock()
			return nil, fmt.Errorf("设备 %s 没有可回退的已确认配置，需要显式指定补偿目标", id)
		}
		targets[id] = cloneConfig(d.ConfirmedConfig)
	}
	s.mu.Unlock()
	return s.PublishChangeWithOptions(targets, PublishOptions{Kind: ChangeKindCompensation, CompensatesFor: changeID, Strategy: ChangeStrategySupersede}, now)
}

// Pull 是设备拉取最新目标。只标记为“已下发”，状态不会跳到“已生效”。
func (s *Store) Pull(deviceID string, now time.Time) (*Device, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	if err := s.processDueLocked(now); err != nil {
		return nil, false, err
	}
	d := s.deviceLocked(deviceID)
	d.Online = true
	changed := d.DesiredRev > d.DeliveredRev
	if changed {
		d.DeliveredRev = d.DesiredRev
		d.DeliveredAt = now.UTC()
		if ch := s.changes[d.DesiredChangeID]; ch != nil {
			result := ch.DeviceResults[deviceID]
			if result.DesiredRev == d.DesiredRev && (result.Status == "" || result.Status == StatusPending) {
				result.DeliveredRev = d.DesiredRev
				result.Status = StatusDelivered
				result.DispatchState = "DELIVERED"
			}
			result.UpdatedAt = now
			ch.DeviceResults[deviceID] = result
			ch.DeliveredRevs[deviceID] = d.DesiredRev
			s.refreshChangeStatusLocked(ch, now)
		}
	}
	if err := s.persistLocked(); err != nil {
		return nil, false, err
	}
	return d, changed, nil
}

// Report 处理设备上报。
// 迟到上报（rev < DesiredRev）只保留为上报记录，绝不更新已确认状态或覆盖最新目标。
func (s *Store) Report(deviceID string, req ReportRequest, now time.Time) (*Device, *Conflict, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	if err := s.processDueLocked(now); err != nil {
		return nil, nil, false, err
	}
	d := s.deviceLocked(deviceID)
	d.Online = true
	operationActiveBeforeReport := s.currentChangeForDeviceLocked(deviceID) != nil
	// 记录最新观察值；上报不改变期望配置。
	d.ReportedConfig = cloneConfig(req.Applied)
	d.ReportedRev = req.Rev
	d.ReportedRejected = cloneConfig(req.Rejected)
	d.ReportedAt = now
	if req.Rev == d.DesiredRev && req.Rev > d.DeliveredRev {
		d.DeliveredRev = req.Rev
		d.DeliveredAt = now
	}

	stale := req.Rev != d.DesiredRev
	if stale {
		c := s.addConflictLocked(deviceID, ReasonStale, req.Rev, map[string]string{
			"reported_rev": fmt.Sprint(req.Rev),
			"desired_rev":  fmt.Sprint(d.DesiredRev),
		}, now)
		s.recordReportOnChangeLocked(deviceID, req, StatusSuperseded, true, now)
		if err := s.persistLocked(); err != nil {
			return nil, nil, false, err
		}
		return d, c, true, nil
	}

	var conflict *Conflict
	deviceStatus := StatusFailed
	exact := req.Rev == d.DesiredRev && len(req.Rejected) == 0 && configEqual(req.Applied, d.DesiredConfig)
	if exact {
		deviceStatus = StatusEffective
		// 只有整份目标精确确认才推进确认版本；部分接受不能被记为整版生效。
		if req.Rev > d.ConfirmedRev {
			d.ConfirmedRev = req.Rev
			d.ConfirmedAt = now
			d.ConfirmedConfig = cloneConfig(req.Applied)
		}
		s.resolveOpenLocked(deviceID, ReasonRejected, req.Rev, now)
		s.resolveOpenLocked(deviceID, ReasonMismatch, req.Rev, now)
	} else {
		if len(req.Rejected) > 0 {
			deviceStatus = StatusPartial
			conflict = s.addConflictLocked(deviceID, ReasonRejected, req.Rev, cloneConfig(req.Rejected), now)
		} else {
			conflict = s.addConflictLocked(deviceID, ReasonMismatch, req.Rev, map[string]string{
				"applied": fmt.Sprint(cloneConfig(req.Applied)),
				"desired": fmt.Sprint(cloneConfig(d.DesiredConfig)),
			}, now)
		}
	}
	s.recordReportOnChangeLocked(deviceID, req, deviceStatus, false, now)
	if current := s.changes[d.DesiredChangeID]; current != nil && current.StartAt != nil && deviceStatus == StatusEffective {
		for _, candidate := range s.changes {
			if candidate.RestoresToChange != current.ID {
				continue
			}
			if linkedResult, ok := candidate.DeviceResults[deviceID]; ok && linkedResult.WaitReason == "WAITING_OVERLAPPING_RESERVATION_CONFIRMATION" {
				linkedResult.WaitReason = ""
				candidate.DeviceResults[deviceID] = linkedResult
				s.refreshChangeStatusLocked(candidate, now)
			}
		}
	}
	if current := s.changes[d.DesiredChangeID]; current != nil && current.Kind == ChangeKindRestoration && deviceStatus == StatusEffective {
		if linkedID := current.DeviceResults[deviceID].RestoresFor; linkedID != "" {
			if linked := s.changes[linkedID]; linked != nil {
				linkedResult := linked.DeviceResults[deviceID]
				linkedResult.WaitReason = ""
				linked.DeviceResults[deviceID] = linkedResult
				s.refreshChangeStatusLocked(linked, now)
			}
		}
	}
	if operationActiveBeforeReport && !stale {
		s.activateNextQueuedForDeviceLocked(deviceID, now)
	}

	// 停止确认可能刚释放了站点容量：只重排尚未开始的任务并下发到点计划。
	if err := s.replanPowerLocked(now); err != nil {
		return nil, nil, false, err
	}
	if _, err := s.dispatchDuePowerLocked(now); err != nil {
		return nil, nil, false, err
	}
	if err := s.persistLocked(); err != nil {
		return nil, nil, false, err
	}
	return d, conflict, false, nil
}

func (s *Store) resolveOpenLocked(deviceID string, reason ConflictReason, rev int64, now time.Time) {
	for _, c := range s.conflicts {
		if c.DeviceID == deviceID && c.Open && c.Reason == reason && c.Rev <= rev {
			resolved := now
			c.Open = false
			c.ResolvedAt = &resolved
		}
	}
}

func (s *Store) MarkOffline(deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nowLocked()
	d := s.deviceLocked(deviceID)
	d.Online = false
	// 设备离线只触发对尚未开始任务的重排，已启动任务保持原样。
	if err := s.replanPowerLocked(now); err != nil {
		return err
	}
	return s.persistLocked()
}

func (s *Store) View(deviceID string, now time.Time) (*TwinView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	_ = s.processDueLocked(now)
	d, ok := s.devices[deviceID]
	if !ok {
		return nil, false
	}
	return s.viewLocked(d, now), true
}

func (s *Store) viewLocked(d *Device, now time.Time) *TwinView {
	st := d.Status(now)
	return &TwinView{
		DeviceID:         d.ID,
		Online:           d.Online,
		DesiredChangeID:  d.DesiredChangeID,
		Status:           st,
		StatusText:       statusText(st),
		DesiredRev:       d.DesiredRev,
		DesiredConfig:    cloneConfig(d.DesiredConfig),
		DeliveredRev:     d.DeliveredRev,
		ReportedRev:      d.ReportedRev,
		ReportedConfig:   cloneConfig(d.ReportedConfig),
		ReportedRejected: cloneConfig(d.ReportedRejected),
		ConfirmedRev:     d.ConfirmedRev,
		ConfirmedConfig:  cloneConfig(d.ConfirmedConfig),
	}
}

func (s *Store) ListViews(now time.Time) []*TwinView {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	_ = s.processDueLocked(now)
	ids := make([]string, 0, len(s.devices))
	for id := range s.devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	views := make([]*TwinView, 0, len(ids))
	for _, id := range ids {
		views = append(views, s.viewLocked(s.devices[id], now))
	}
	return views
}

func (s *Store) Conflicts(deviceID string, includeResolved bool) []*Conflict {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Conflict, 0)
	for _, c := range s.conflicts {
		if c.DeviceID != deviceID {
			continue
		}
		if !includeResolved && !c.Open {
			continue
		}
		cp := *c
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) Change(changeID string) (*ChangeView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nowLocked()
	_ = s.processDueLocked(now)
	ch, ok := s.changes[changeID]
	if !ok {
		return nil, false
	}
	v := ChangeView{Change: cloneChange(ch), StatusText: statusText(ch.Status)}
	return &v, true
}

func (s *Store) ListChanges() []*ChangeView {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nowLocked()
	_ = s.processDueLocked(now)
	ids := make([]string, 0, len(s.changes))
	for id := range s.changes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*ChangeView, 0, len(ids))
	for _, id := range ids {
		ch := s.changes[id]
		out = append(out, &ChangeView{Change: cloneChange(ch), StatusText: statusText(ch.Status)})
	}
	return out
}

func normalizeTargets(targets map[string]Config) (map[string]Config, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("变更目标不能为空")
	}
	out := make(map[string]Config, len(targets))
	ids := make([]string, 0, len(targets))
	for id := range targets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if id == "" {
			return nil, fmt.Errorf("设备 ID 不能为空")
		}
		if targets[id] == nil {
			return nil, fmt.Errorf("设备 %s 的目标配置必须是 JSON 对象", id)
		}
		out[id] = cloneConfig(targets[id])
	}
	return out, nil
}

func (s *Store) currentChangeForDeviceLocked(deviceID string) *Change {
	d := s.devices[deviceID]
	if d == nil || d.DesiredChangeID == "" {
		return nil
	}
	ch := s.changes[d.DesiredChangeID]
	if ch == nil {
		return nil
	}
	result := ch.DeviceResults[deviceID]
	if result.DesiredRev != d.DesiredRev || !isDeviceOperationStatus(result.Status) {
		return nil
	}
	return ch
}

func isDeviceOperationStatus(st Status) bool {
	return st == StatusPending || st == StatusDelivered
}

func (s *Store) deviceOperationActiveLocked(d *Device) bool {
	return d.DesiredRev > 0 && !d.isDesiredConfirmedLocked()
}

func (s *Store) assignTargetToDeviceLocked(ch *Change, deviceID string, target Config, d *Device, now time.Time, startAt, endAt *time.Time) {
	d.DesiredRev++
	d.DesiredConfig = cloneConfig(target)
	d.DesiredChangeID = ch.ID
	if ch.BaselineTargets == nil {
		ch.BaselineTargets = map[string]Config{}
	}
	ch.BaselineTargets[deviceID] = cloneConfig(d.ConfirmedConfig)
	ch.DeviceResults[deviceID] = ChangeDeviceResult{
		DesiredRev:    d.DesiredRev,
		Status:        StatusPending,
		DispatchState: "READY",
		StartAt:       startAt,
		EndAt:         endAt,
		UpdatedAt:     now,
		Scheduled:     startAt != nil,
	}
}

func (s *Store) queuedChangesForDeviceLocked(deviceID string) []*Change {
	out := []*Change{}
	for _, ch := range s.changes {
		if result, ok := ch.DeviceResults[deviceID]; ok && result.Status == StatusQueued {
			out = append(out, ch)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) activateNextQueuedForDeviceLocked(deviceID string, now time.Time) {
	if s.currentChangeForDeviceLocked(deviceID) != nil {
		return
	}
	queued := s.queuedChangesForDeviceLocked(deviceID)
	if len(queued) == 0 {
		return
	}
	next := queued[0]
	d := s.deviceLocked(deviceID)
	queuedResult := next.DeviceResults[deviceID]
	s.assignTargetToDeviceLocked(next, deviceID, next.Targets[deviceID], d, now, queuedResult.StartAt, queuedResult.EndAt)
	result := next.DeviceResults[deviceID]
	result.WaitForChange = ""
	result.WaitReason = ""
	result.UpdatedAt = now
	next.DeviceResults[deviceID] = result
	next.WaitingDevices = removeString(next.WaitingDevices, deviceID)
	next.ReadyDevices = appendUnique(next.ReadyDevices, deviceID)
	s.refreshChangeStatusLocked(next, now)
}

func (s *Store) supersedeDeviceInChangeLocked(ch *Change, deviceID string, now time.Time) {
	result := ch.DeviceResults[deviceID]
	if result.Status == StatusEffective {
		result.DispatchState = "SUPERSEDED"
		result.UpdatedAt = now
		ch.DeviceResults[deviceID] = result
	} else {
		result.Status = StatusSuperseded
		result.DispatchState = "SUPERSEDED"
		result.WaitForChange = ""
		result.UpdatedAt = now
		ch.DeviceResults[deviceID] = result
	}
	ch.WaitingDevices = removeString(ch.WaitingDevices, deviceID)
	ch.ReadyDevices = removeString(ch.ReadyDevices, deviceID)
	s.refreshChangeStatusLocked(ch, now)
	if ch.Status == StatusSuperseded && ch.SupersededAt == nil {
		ch.SupersededAt = &now
	}
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func removeString(values []string, want string) []string {
	out := values[:0]
	for _, value := range values {
		if value != want {
			out = append(out, value)
		}
	}
	return out
}

func (s *Store) recordReportOnChangeLocked(deviceID string, req ReportRequest, resultStatus Status, stale bool, now time.Time) {
	d := s.devices[deviceID]
	ch := s.changes[d.DesiredChangeID]
	if ch == nil || stale || ch.DeviceResults[deviceID].DesiredRev != req.Rev {
		var matched *Change
		for _, candidate := range s.changes {
			if result, ok := candidate.DeviceResults[deviceID]; ok && result.DesiredRev == req.Rev {
				matched = candidate
				break
			}
		}
		ch = matched
	}
	if ch == nil {
		return
	}
	result := ch.DeviceResults[deviceID]
	result.DesiredRev = req.Rev
	result.ReportedRev = req.Rev
	result.AppliedConfig = cloneConfig(req.Applied)
	result.Rejected = cloneConfig(req.Rejected)
	result.Stale = stale
	result.UpdatedAt = now
	if isDeviceOperationStatus(result.Status) || (result.Status == "") {
		result.Status = resultStatus
		if resultStatus == StatusEffective {
			result.DispatchState = "EFFECTIVE"
		} else if stale {
			result.DispatchState = "STALE"
		}
		ch.DeviceResults[deviceID] = result
		s.refreshChangeStatusLocked(ch, now)
	} else {
		if stale && (result.Status == StatusExpired || result.Status == StatusEffective) {
			switch resultStatus {
			case StatusEffective:
				result.DispatchState = "LATE_EFFECTIVE"
			case StatusPartial:
				result.DispatchState = "LATE_PARTIAL"
			case StatusFailed:
				result.DispatchState = "LATE_FAILED"
			}
			result.Stale = true
		}
		if stale && resultStatus != StatusEffective && result.Status != StatusEffective {
			if result.Status != StatusExpired {
				result.Status = StatusSuperseded
				result.DispatchState = "STALE"
			}
		}
		ch.DeviceResults[deviceID] = result
		s.refreshChangeStatusLocked(ch, now)
	}
}

func (s *Store) refreshChangeStatusLocked(ch *Change, now time.Time) {
	ch.ReadyDevices = ch.ReadyDevices[:0]
	ch.WaitingDevices = ch.WaitingDevices[:0]
	for id, result := range ch.DeviceResults {
		switch result.Status {
		case StatusQueued:
			ch.WaitingDevices = append(ch.WaitingDevices, id)
		case StatusScheduled:
			ch.WaitingDevices = append(ch.WaitingDevices, id)
		case StatusPending, StatusDelivered:
			ch.ReadyDevices = append(ch.ReadyDevices, id)
		}
	}
	sort.Strings(ch.ReadyDevices)
	sort.Strings(ch.WaitingDevices)
	hasStatus := func(want Status) bool {
		for _, result := range ch.DeviceResults {
			if result.Status == want {
				return true
			}
		}
		return false
	}
	allEffective := true
	allTerminalReplaced := true
	hasSuperseded := false
	allTerminal := true
	hasEnded := false
	for _, result := range ch.DeviceResults {
		if result.Status != StatusEffective {
			allEffective = false
		} else if result.EndReason != "" {
			allEffective = false
		}
		if result.Status != StatusEffective && result.Status != StatusSuperseded {
			allTerminalReplaced = false
		}
		if result.Status == StatusSuperseded {
			hasSuperseded = true
		}
		if result.Status == StatusEffective && result.DispatchState == "SUPERSEDED" {
			hasSuperseded = true
		}
		if result.Status == StatusQueued || result.Status == StatusScheduled || isDeviceOperationStatus(result.Status) {
			allTerminal = false
		}
		if result.EndReason != "" {
			hasEnded = true
		}
	}
	switch {
	case (hasStatus(StatusExpired) || (hasEnded && !hasStatus(StatusScheduled) && !hasStatus(StatusQueued))):
		ch.Status = StatusExpired
	case allTerminalReplaced && hasSuperseded:
		ch.Status = StatusSuperseded
		if ch.SupersededAt == nil {
			ch.SupersededAt = &now
		}
	case allEffective:
		ch.Status = StatusEffective
		if ch.EffectiveAt == nil {
			ch.EffectiveAt = &now
		}
	case hasStatus(StatusCancelled):
		ch.Status = StatusCancelled
	case hasStatus(StatusPartial):
		ch.Status = StatusPartial
	case hasStatus(StatusFailed):
		ch.Status = StatusFailed
	case allTerminal:
		ch.Status = StatusSuperseded
		if ch.SupersededAt == nil {
			ch.SupersededAt = &now
		}
	case hasStatus(StatusScheduled):
		ch.Status = StatusScheduled
	case len(ch.WaitingDevices) > 0:
		ch.Status = StatusQueued
	case hasStatus(StatusDelivered):
		ch.Status = StatusDelivered
	default:
		ch.Status = StatusPending
	}
}

func cloneChange(ch *Change) *Change {
	if ch == nil {
		return nil
	}
	cp := *ch
	cp.Targets = map[string]Config{}
	for id, cfg := range ch.Targets {
		cp.Targets[id] = cloneConfig(cfg)
	}
	cp.BaselineTargets = map[string]Config{}
	for id, cfg := range ch.BaselineTargets {
		cp.BaselineTargets[id] = cloneConfig(cfg)
	}
	cp.DeliveredRevs = map[string]int64{}
	for id, rev := range ch.DeliveredRevs {
		cp.DeliveredRevs[id] = rev
	}
	cp.DeviceResults = map[string]ChangeDeviceResult{}
	cp.DeviceEndsAt = map[string]time.Time{}
	for id, t := range ch.DeviceEndsAt {
		cp.DeviceEndsAt[id] = t
	}
	for id, result := range ch.DeviceResults {
		result.AppliedConfig = cloneConfig(result.AppliedConfig)
		result.Rejected = cloneConfig(result.Rejected)
		cp.DeviceResults[id] = result
	}
	cp.Supersedes = append([]string(nil), ch.Supersedes...)
	cp.RestoreChanges = append([]string(nil), ch.RestoreChanges...)
	cp.ReadyDevices = append([]string(nil), ch.ReadyDevices...)
	cp.WaitingDevices = append([]string(nil), ch.WaitingDevices...)
	return &cp
}

func cloneConfig(c Config) Config {
	if c == nil {
		return Config{}
	}
	cp := make(Config, len(c))
	for k, v := range c {
		cp[k] = v
	}
	return cp
}

func statusText(st Status) string {
	switch st {
	case StatusEffective:
		return "已生效"
	case StatusDelivered:
		return "已下发，待设备确认生效"
	case StatusPartial:
		return "部分生效，仍有配置项未确认"
	case StatusFailed:
		return "未生效，设备确认失败"
	case StatusSuperseded:
		return "已被新目标替代，历史保留"
	case StatusQueued:
		return "等待先前设备操作结束"
	case StatusScheduled:
		return "预约等待生效时间"
	case StatusExpired:
		return "预约有效期已结束，等待恢复收尾确认"
	case StatusCancelled:
		return "预约已取消"
	default:
		return "待下发"
	}
}
