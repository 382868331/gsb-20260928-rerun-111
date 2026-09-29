package twin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Store 负责孪生状态的持久化，采用临时文件 + rename 原子写入。
type Store struct {
	path string
}

func NewStore(path string) *Store {
	return &Store{path: path}
}

type snapshot struct {
	Devices map[string]*DeviceTwin `json:"devices"`
}

func (s *Store) Load() (map[string]*DeviceTwin, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]*DeviceTwin{}, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return map[string]*DeviceTwin{}, nil
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	if snap.Devices == nil {
		snap.Devices = map[string]*DeviceTwin{}
	}
	for id, d := range snap.Devices {
		if d == nil {
			d = newDeviceTwin(id)
			snap.Devices[id] = d
		}
		if d.Desired == nil {
			d.Desired = map[string]DesiredState{}
		}
		if d.Reported == nil {
			d.Reported = map[string]ReportedState{}
		}
		if d.Confirmed == nil {
			d.Confirmed = map[string]ConfirmedState{}
		}
	}
	return snap.Devices, nil
}

func (s *Store) Save(devices map[string]*DeviceTwin) error {
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(snapshot{Devices: devices}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
