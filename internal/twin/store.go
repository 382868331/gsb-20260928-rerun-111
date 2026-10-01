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
}

type snapshot struct {
	Devices     []*Device   `json:"devices"`
	Conflicts   []*Conflict `json:"conflicts"`
	ConflictSeq int64       `json:"conflict_seq"`
	Changes     []*Change   `json:"changes"`
	ChangeSeq   int64       `json:"change_seq"`
}

type PublishOptions struct {
	Kind           ChangeKind
	CompensatesFor string
	Strategy       ChangeStrategy
}

func NewStore(path string) (*Store, error) {
	s := &Store{
		path:      path,
		devices:   map[string]*Device{},
		conflicts: map[string]*Conflict{},
		changes:   map[string]*Change{},
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

// PreviewChange 只计算新目标与各设备当前期望/观察/确认版本之间的差异，不发布、不落盘。
func (s *Store) PreviewChange(targets map[string]Config, now time.Time) (*Preview, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
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
		Status:          StatusPending,
		Strategy:        strategy,
		ReadyDevices:    []string{},
		WaitingDevices:  []string{},
		CreatedAt:       now,
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
		s.assignTargetToDeviceLocked(ch, id, targets[id], d, now)
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
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return cloneChange(ch), nil
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
			result.UpdatedAt = now.UTC()
			ch.DeviceResults[deviceID] = result
			ch.DeliveredRevs[deviceID] = d.DesiredRev
			s.refreshChangeStatusLocked(ch, now.UTC())
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
	d := s.deviceLocked(deviceID)
	d.Online = true
	now = now.UTC()
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
	if operationActiveBeforeReport && !stale {
		s.activateNextQueuedForDeviceLocked(deviceID, now)
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
	d := s.deviceLocked(deviceID)
	d.Online = false
	return s.persistLocked()
}

func (s *Store) View(deviceID string, now time.Time) (*TwinView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
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

func (s *Store) assignTargetToDeviceLocked(ch *Change, deviceID string, target Config, d *Device, now time.Time) {
	d.DesiredRev++
	d.DesiredConfig = cloneConfig(target)
	d.DesiredChangeID = ch.ID
	ch.DeviceResults[deviceID] = ChangeDeviceResult{
		DesiredRev:    d.DesiredRev,
		Status:        StatusPending,
		DispatchState: "READY",
		UpdatedAt:     now,
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
	d.DesiredRev++
	d.DesiredConfig = cloneConfig(next.Targets[deviceID])
	d.DesiredChangeID = next.ID
	result := next.DeviceResults[deviceID]
	result.DesiredRev = d.DesiredRev
	result.Status = StatusPending
	result.DispatchState = "READY"
	result.WaitForChange = ""
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
		if stale && resultStatus != StatusEffective && result.Status != StatusEffective {
			result.Status = StatusSuperseded
			result.DispatchState = "STALE"
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
	for _, result := range ch.DeviceResults {
		if result.Status != StatusEffective {
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
		if result.Status == StatusQueued || isDeviceOperationStatus(result.Status) {
			allTerminal = false
		}
	}
	switch {
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
	case hasStatus(StatusPartial):
		ch.Status = StatusPartial
	case hasStatus(StatusFailed):
		ch.Status = StatusFailed
	case allTerminal:
		ch.Status = StatusSuperseded
		if ch.SupersededAt == nil {
			ch.SupersededAt = &now
		}
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
	for id, result := range ch.DeviceResults {
		result.AppliedConfig = cloneConfig(result.AppliedConfig)
		result.Rejected = cloneConfig(result.Rejected)
		cp.DeviceResults[id] = result
	}
	cp.Supersedes = append([]string(nil), ch.Supersedes...)
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
	default:
		return "待下发"
	}
}
