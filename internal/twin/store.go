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
	targets     map[string]*Target
	conflictSeq int64
	targetSeq   int64
}

type snapshot struct {
	Devices     []*Device   `json:"devices"`
	Conflicts   []*Conflict `json:"conflicts"`
	Targets     []*Target   `json:"targets"`
	ConflictSeq int64       `json:"conflict_seq"`
	TargetSeq   int64       `json:"target_seq"`
}

func NewStore(path string) (*Store, error) {
	s := &Store{
		path:      path,
		devices:   map[string]*Device{},
		targets:   map[string]*Target{},
		conflicts: map[string]*Conflict{},
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
	for _, t := range snap.Targets {
		s.targets[t.ID] = t
	}
	s.conflictSeq = snap.ConflictSeq
	s.targetSeq = snap.TargetSeq
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
	for _, t := range s.targets {
		snap.Targets = append(snap.Targets, t)
	}
	sort.Slice(snap.Targets, func(i, j int) bool { return snap.Targets[i].ID < snap.Targets[j].ID })
	snap.TargetSeq = s.targetSeq

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

// Preview 预演一份配置相对设备当前期望的差异，不修改任何状态。
func (s *Store) Preview(deviceID string, cfg Config) ConfigDiff {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.deviceLocked(deviceID)
	return DiffConfig(cloneConfig(d.DesiredConfig), cloneConfig(cfg))
}

// SetDesired 向单台设备发布新目标（普通变更）。
func (s *Store) SetDesired(deviceID string, cfg Config, now time.Time) (*Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.publishLocked(map[string]Config{deviceID: cfg}, ChangeNormal, "", now)
}

// PublishTarget 向一组设备发布相关配置目标。配置作为一个整体，
// 只有每台设备都确认生效，目标整体才算 EFFECTIVE。
func (s *Store) PublishTarget(spec map[string]Config, now time.Time) (*Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(spec) == 0 {
		return nil, fmt.Errorf("目标至少要包含一台设备")
	}
	return s.publishLocked(spec, ChangeNormal, "", now)
}

// publishLocked 必须在持锁状态下调用。
func (s *Store) publishLocked(spec map[string]Config, kind ChangeKind, compensates string, now time.Time) (*Target, error) {
	now = now.UTC()
	s.targetSeq++
	t := &Target{
		ID:          fmt.Sprintf("T%06d", s.targetSeq),
		Kind:        kind,
		Compensates: compensates,
		CreatedAt:   now,
	}
	ids := make([]string, 0, len(spec))
	for id := range spec {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		d := s.deviceLocked(id)
		// 设备上一轮已下发但未全部确认的版本被新版本替代：记录冲突，同时保留变更记录。
		if d.DesiredRev > 0 && d.DeliveredRev >= d.DesiredRev && d.ConfirmedRev < d.DesiredRev {
			s.addConflictLocked(id, ReasonSuperseded, d.DesiredRev, d.DesiredConfig, now)
		}
		if ch := s.lastChangeLocked(d); ch != nil && ch.Status != StatusEffective {
			ch.Status = StatusSuperseded
		}
		d.DesiredRev++
		d.DesiredConfig = cloneConfig(spec[id])
		ch := &Change{
			Rev:       d.DesiredRev,
			TargetID:  t.ID,
			Kind:      kind,
			Config:    cloneConfig(spec[id]),
			Status:    StatusPending,
			CreatedAt: now,
		}
		d.Changes = append(d.Changes, ch)
		t.Items = append(t.Items, TargetItem{DeviceID: id, Rev: d.DesiredRev})
	}
	s.targets[t.ID] = t
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return t, nil
}

// Compensate 为一个尚未整体生效的目标发起补偿目标。补偿是新的期望版本，
// 必须重新经过设备拉取与确认，不能直接标记为生效。
func (s *Store) Compensate(targetID string, spec map[string]Config, now time.Time) (*Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.targets[targetID]
	if !ok {
		return nil, fmt.Errorf("目标 %s 不存在", targetID)
	}
	if len(spec) == 0 {
		// 默认补偿内容：回退为各设备最近一次已确认配置。
		spec = map[string]Config{}
		for _, item := range old.Items {
			d := s.deviceLocked(item.DeviceID)
			spec[item.DeviceID] = cloneConfig(d.ConfirmedConfig)
		}
	}
	for _, item := range old.Items {
		if _, ok := spec[item.DeviceID]; !ok {
			return nil, fmt.Errorf("补偿目标必须覆盖原目标的所有设备，缺少 %s", item.DeviceID)
		}
	}
	for id := range spec {
		if !targetHasDevice(old, id) {
			return nil, fmt.Errorf("设备 %s 不在原目标中", id)
		}
	}
	return s.publishLocked(spec, ChangeCompensation, targetID, now)
}

func targetHasDevice(t *Target, id string) bool {
	for _, item := range t.Items {
		if item.DeviceID == id {
			return true
		}
	}
	return false
}

func (s *Store) lastChangeLocked(d *Device) *Change {
	if len(d.Changes) == 0 {
		return nil
	}
	return d.Changes[len(d.Changes)-1]
}

// changeLocked 找到设备某个期望版本的变更记录。
func (s *Store) changeLocked(d *Device, rev int64) *Change {
	for _, ch := range d.Changes {
		if ch.Rev == rev {
			return ch
		}
	}
	return nil
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
		if ch := s.lastChangeLocked(d); ch != nil && ch.Rev == d.DesiredRev && ch.Status == StatusPending {
			delivered := now.UTC()
			ch.Status = StatusDelivered
			ch.DeliveredAt = &delivered
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

	// 记录最新上报内容；上报不改变期望配置。
	reported := cloneConfig(req.Applied)
	d.ReportedConfig = reported
	d.ReportedRev = req.Rev
	d.ReportedAt = now
	if req.Rev > d.DeliveredRev {
		d.DeliveredRev = req.Rev
		d.DeliveredAt = now
		if ch := s.changeLocked(d, req.Rev); ch != nil && ch.Status == StatusPending {
			ch.Status = StatusDelivered
			ch.DeliveredAt = &now
		}
	}

	stale := req.Rev < d.DesiredRev
	if stale {
		c := s.addConflictLocked(deviceID, ReasonStale, req.Rev, map[string]string{
			"reported_rev": fmt.Sprint(req.Rev),
			"desired_rev":  fmt.Sprint(d.DesiredRev),
		}, now)
		if err := s.persistLocked(); err != nil {
			return nil, nil, false, err
		}
		return d, c, true, nil
	}

	var conflict *Conflict
	if len(req.Rejected) > 0 {
		conflict = s.addConflictLocked(deviceID, ReasonRejected, req.Rev, cloneConfig(req.Rejected), now)
	}

	// 只有针对当前期望版本的上报才能推进确认状态。确认只前进，不回退。
	if req.Rev == d.DesiredRev && req.Rev > d.ConfirmedRev {
		d.ConfirmedRev = req.Rev
		d.ConfirmedAt = now
		d.ConfirmedConfig = cloneConfig(req.Applied)
	}
	// 同步该版本变更记录的实际结果；接受部分配置保留为 PARTIAL，绝不当作整体生效。
	if ch := s.changeLocked(d, req.Rev); ch != nil && ch.Status != StatusSuperseded {
		ch.Rejected = cloneConfig(req.Rejected)
		if len(req.Rejected) == 0 && req.Rev == d.DesiredRev && configEqual(req.Applied, d.DesiredConfig) {
			ch.Status = StatusEffective
			confirmed := now
			ch.ConfirmedAt = &confirmed
		} else if req.Rev == d.DesiredRev {
			ch.Status = StatusPartial
		}
	}
	// 只有设备完全按期望接受时，之前的拒绝冲突才算解决。
	if len(req.Rejected) == 0 && req.Rev == d.DesiredRev && configEqual(req.Applied, d.DesiredConfig) {
		s.resolveOpenLocked(deviceID, ReasonRejected, req.Rev, now)
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
		DeviceID:        d.ID,
		Online:          d.Online,
		Status:          st,
		StatusText:      statusText(st),
		DesiredRev:      d.DesiredRev,
		DesiredConfig:   cloneConfig(d.DesiredConfig),
		DeliveredRev:    d.DeliveredRev,
		ReportedRev:     d.ReportedRev,
		ReportedConfig:  cloneConfig(d.ReportedConfig),
		ConfirmedRev:    d.ConfirmedRev,
		ConfirmedConfig: cloneConfig(d.ConfirmedConfig),
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

// Changes 返回一台设备的全部期望发布记录，包括已被替代、从未完成的版本。
func (s *Store) Changes(deviceID string) []*Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[deviceID]
	if !ok {
		return nil
	}
	out := make([]*Change, 0, len(d.Changes))
	for _, ch := range d.Changes {
		cp := *ch
		out = append(out, &cp)
	}
	return out
}

// Target 视图：整体状态只由每台设备自身的当前孪生状态聚合，
// 任何一台已下发未确认或部分生效，整体都不是 EFFECTIVE。
func (s *Store) TargetView(id string, now time.Time) (*TargetView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.targets[id]
	if !ok {
		return nil, false
	}
	return s.targetViewLocked(t, now), true
}

func (s *Store) targetViewLocked(t *Target, now time.Time) *TargetView {
	v := &TargetView{
		ID:          t.ID,
		Kind:        t.Kind,
		Compensates: t.Compensates,
		CreatedAt:   t.CreatedAt,
		Items:       []TargetItemView{},
	}
	overall := StatusEffective
	for _, item := range t.Items {
		d := s.deviceLocked(item.DeviceID)
		st := StatusPending
		if ch := s.changeLocked(d, item.Rev); ch != nil {
			st = ch.Status
		}
		switch {
		case st == StatusSuperseded:
			if overall == StatusEffective {
				overall = StatusSuperseded
			}
		case st == StatusPartial:
			overall = StatusPartial
		case st == StatusDelivered && overall != StatusPartial:
			overall = StatusDelivered
		case st == StatusPending && overall != StatusPartial && overall != StatusDelivered:
			overall = StatusPending
		}
		iv := TargetItemView{
			DeviceID:        item.DeviceID,
			Rev:             item.Rev,
			Status:          st,
			StatusText:      statusText(st),
			DesiredRev:      d.DesiredRev,
			ReportedRev:     d.ReportedRev,
			ConfirmedRev:    d.ConfirmedRev,
			ConfirmedConfig: cloneConfig(d.ConfirmedConfig),
		}
		v.Items = append(v.Items, iv)
	}
	v.Overall = overall
	v.OverallText = statusText(overall)
	return v
}

func (s *Store) ListTargets(now time.Time) []*TargetView {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := make([]*Target, 0, len(s.targets))
	for _, t := range s.targets {
		ts = append(ts, t)
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].ID < ts[j].ID })
	out := make([]*TargetView, 0, len(ts))
	for _, t := range ts {
		out = append(out, s.targetViewLocked(t, now))
	}
	return out
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
		return "部分生效，设备拒绝了部分配置"
	case StatusSuperseded:
		return "该版本已被更新目标替代"
	default:
		return "待下发"
	}
}
