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
}

type snapshot struct {
	Devices     []*Device   `json:"devices"`
	Conflicts   []*Conflict `json:"conflicts"`
	ConflictSeq int64       `json:"conflict_seq"`
}

func NewStore(path string) (*Store, error) {
	s := &Store{
		path:      path,
		devices:   map[string]*Device{},
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
	s.conflictSeq = snap.ConflictSeq
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

// SetDesired 写入新的期望快照。离线期间的连续修改只推进 DesiredRev；
// 若设备上一轮下发的版本尚未确认，则记一条 SUPERSEDED 冲突，恢复后不会补执行旧版本。
func (s *Store) SetDesired(deviceID string, cfg Config, now time.Time) (*Device, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.deviceLocked(deviceID)
	if d.DesiredRev > 0 && d.DeliveredRev >= d.DesiredRev && d.ConfirmedRev < d.DesiredRev {
		s.addConflictLocked(deviceID, ReasonSuperseded, d.DesiredRev, d.DesiredConfig, now)
	}
	d.DesiredRev++
	d.DesiredConfig = cloneConfig(cfg)
	if err := s.persistLocked(); err != nil {
		return nil, 0, err
	}
	return d, d.DesiredRev, nil
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
	default:
		return "待下发"
	}
}
