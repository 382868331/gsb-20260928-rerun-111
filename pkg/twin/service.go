package twin

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var ErrNotFound = errors.New("device not found")

type Service struct {
	mu      sync.Mutex
	devices map[string]*DeviceTwin
	store   *Store
}

func NewService(store *Store) (*Service, error) {
	devices, err := store.Load()
	if err != nil {
		return nil, err
	}
	return &Service{devices: devices, store: store}, nil
}

func (svc *Service) saveLocked() error {
	return svc.store.Save(svc.devices)
}

// Register 注册设备；已存在则原样返回。
func (svc *Service) Register(id string) (*DeviceTwin, error) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if d, ok := svc.devices[id]; ok {
		return d, nil
	}
	d := newDeviceTwin(id)
	svc.devices[id] = d
	if err := svc.saveLocked(); err != nil {
		delete(svc.devices, id)
		return nil, err
	}
	return d, nil
}

func (svc *Service) Get(id string) (*DeviceTwin, error) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	d, ok := svc.devices[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneDevice(d), nil
}

func (svc *Service) List() []*DeviceTwin {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	ids := make([]string, 0, len(svc.devices))
	for id := range svc.devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*DeviceTwin, 0, len(ids))
	for _, id := range ids {
		out = append(out, cloneDevice(svc.devices[id]))
	}
	return out
}

// SetDesired 更新期望状态。离线时连续修改同一 key：版本递增，
// 始终只保留最新目标；尚未确认的旧目标不会排队等设备执行。
func (svc *Service) SetDesired(id string, cfg map[string]string) (*DeviceTwin, error) {
	if len(cfg) == 0 {
		return nil, fmt.Errorf("config must not be empty")
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	d, ok := svc.devices[id]
	if !ok {
		return nil, ErrNotFound
	}
	now := time.Now().UTC()
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := cfg[k]
		cur := d.Desired[k]
		// 值未变且已生效，无需重复下发。
		if cur.Version > 0 && cur.Value == v && cur.Status == StatusEffective {
			continue
		}
		cur.Value = v
		cur.Version++
		cur.Status = StatusPending
		cur.UpdatedAt = now
		d.Desired[k] = cur
	}
	if err := svc.saveLocked(); err != nil {
		return nil, err
	}
	return cloneDevice(d), nil
}

// Poll 设备拉取全部待下发配置，拉取即标记为 delivered（已下发不等于已生效）。
// 同时上报先前已应用项，等价于随轮询携带 applied 报告。
func (svc *Service) Poll(id string, req PollRequest) (*PollResponse, error) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	d, ok := svc.devices[id]
	if !ok {
		return nil, ErrNotFound
	}
	now := time.Now().UTC()

	for key, item := range req.Applied {
		svc.applyReportLocked(d, ReportItem{
			Key:     key,
			Value:   item.Value,
			Version: item.Version,
			Applied: true,
		}, now)
	}

	resp := &PollResponse{DeviceID: id}
	keys := make([]string, 0, len(d.Desired))
	for k, st := range d.Desired {
		if st.Status == StatusPending {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		st := d.Desired[k]
		st.Status = StatusDelivered
		d.Desired[k] = st
		resp.Config = append(resp.Config, DesiredItem{
			Key:     k,
			Value:   st.Value,
			Version: st.Version,
		})
	}
	if err := svc.saveLocked(); err != nil {
		return nil, err
	}
	return resp, nil
}

// Report 处理设备上报：带版本的确认/拒绝，或无版本的实际状态快照。
func (svc *Service) Report(id string, req ReportRequest) error {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	d, ok := svc.devices[id]
	if !ok {
		return ErrNotFound
	}
	now := time.Now().UTC()
	for _, item := range req.Items {
		svc.applyReportLocked(d, item, now)
	}
	keys := make([]string, 0, len(req.Snapshot))
	for k := range req.Snapshot {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		svc.applySnapshotLocked(d, k, req.Snapshot[k], now)
	}
	return svc.saveLocked()
}

func (svc *Service) applyReportLocked(d *DeviceTwin, item ReportItem, now time.Time) {
	des, ok := d.Desired[item.Key]
	d.Reported[item.Key] = ReportedState{
		Value:   item.Value,
		At:      now,
		Version: item.Version,
	}
	if !ok || item.Version == 0 {
		// 无版本上报只进 reported，不确认任何期望版本。
		if ok {
			svc.applySnapshotLocked(d, item.Key, item.Value, now)
		}
		return
	}
	if item.Version < des.Version {
		// 迟到的旧版本确认：旧确认不能盖掉最新目标。
		d.Conflicts = append(d.Conflicts, Conflict{
			At:              now,
			Key:             item.Key,
			Kind:            ConflictStaleAck,
			Reason:          fmt.Sprintf("reported version %d older than desired version %d", item.Version, des.Version),
			DesiredValue:    des.Value,
			DesiredVersion:  des.Version,
			ReportedValue:   item.Value,
			ReportedVersion: item.Version,
		})
		return
	}
	if item.Applied && item.Value == des.Value {
		des.Status = StatusEffective
		d.Desired[item.Key] = des
		d.Confirmed[item.Key] = ConfirmedState{
			Value:   item.Value,
			Version: item.Version,
			At:      now,
		}
		return
	}
	// 设备拒绝，或设备声称应用但值与目标不一致，都按未生效处理。
	if !item.Applied {
		des.Status = StatusRejected
	} else {
		des.Status = StatusDelivered
	}
	d.Desired[item.Key] = des
	kind := ConflictRejected
	reason := item.Reason
	if item.Applied {
		kind = ConflictDiverged
		if reason == "" {
			reason = "applied value differs from desired value"
		}
	}
	d.Conflicts = append(d.Conflicts, Conflict{
		At:              now,
		Key:             item.Key,
		Kind:            kind,
		Reason:          reason,
		DesiredValue:    des.Value,
		DesiredVersion:  des.Version,
		ReportedValue:   item.Value,
		ReportedVersion: item.Version,
	})
}

func (svc *Service) applySnapshotLocked(d *DeviceTwin, key, value string, now time.Time) {
	d.Reported[key] = ReportedState{Value: value, At: now}
	if des, ok := d.Desired[key]; ok && des.Status != StatusEffective {
		if value != des.Value {
			d.Conflicts = append(d.Conflicts, Conflict{
				At:             now,
				Key:            key,
				Kind:           ConflictDiverged,
				Reason:         "reported value differs from pending desired value",
				DesiredValue:   des.Value,
				DesiredVersion: des.Version,
				ReportedValue:  value,
			})
		}
		// 无版本上报永远不能把期望标记为 effective：已下发/上报不等于已生效。
	}
}

func (svc *Service) Conflicts(id string) ([]Conflict, error) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	d, ok := svc.devices[id]
	if !ok {
		return nil, ErrNotFound
	}
	out := make([]Conflict, len(d.Conflicts))
	copy(out, d.Conflicts)
	return out, nil
}

func cloneDevice(d *DeviceTwin) *DeviceTwin {
	cp := *d
	cp.Desired = cloneMap(d.Desired)
	cp.Reported = cloneMap(d.Reported)
	cp.Confirmed = cloneMap(d.Confirmed)
	cp.Conflicts = append([]Conflict(nil), d.Conflicts...)
	return &cp
}

func cloneMap[T any](m map[string]T) map[string]T {
	out := make(map[string]T, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
