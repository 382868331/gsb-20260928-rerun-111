package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"twinsvc/pkg/twin"
)

type rejectRule struct {
	key   string
	value string // 空表示该 key 的任何值都拒绝
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// localState 是虚拟设备本地实际保存的配置。
type localState struct {
	Config map[string]string `json:"config"`
}

func main() {
	server := flag.String("server", "http://127.0.0.1:8080", "twin service URL")
	id := flag.String("id", "vdev-1", "device id")
	interval := flag.Duration("interval", 2*time.Second, "poll interval")
	db := flag.String("db", "data/vdev.json", "local state file")
	var rejects stringList
	flag.Var(&rejects, "reject", "reject rule key or key=value; repeatable")
	var stale stringList
	flag.Var(&stale, "stale", "after applying key, send one late ack for an old version; repeatable")
	flag.Parse()

	rules, err := parseRules(rejects)
	if err != nil {
		log.Fatal(err)
	}
	staleSet := map[string]bool{}
	for _, k := range stale {
		staleSet[strings.TrimSpace(k)] = true
	}

	dev := &vdevice{
		server:   strings.TrimRight(*server, "/"),
		id:       *id,
		db:       *db,
		rules:    rules,
		stale:    staleSet,
		client:   &http.Client{Timeout: 5 * time.Second},
		interval: *interval,
	}
	if err := dev.run(); err != nil {
		log.Fatal(err)
	}
}

func parseRules(list []string) ([]rejectRule, error) {
	rules := make([]rejectRule, 0, len(list))
	for _, r := range list {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if k, v, ok := strings.Cut(r, "="); ok {
			rules = append(rules, rejectRule{key: k, value: v})
		} else {
			rules = append(rules, rejectRule{key: r})
		}
	}
	return rules, nil
}

type vdevice struct {
	server   string
	id       string
	db       string
	rules    []rejectRule
	stale    map[string]bool
	client   *http.Client
	interval time.Duration
	state    localState
}

func (v *vdevice) run() error {
	if err := v.load(); err != nil {
		return err
	}
	log.Printf("virtual device %s started, local config: %v", v.id, v.state.Config)

	// 启动时上报一次本地真实配置快照（无版本，只进 reported，不产生确认）。
	if len(v.state.Config) > 0 {
		if err := v.sendSnapshot(); err != nil {
			log.Printf("startup snapshot: %v", err)
		}
	}

	t := time.NewTicker(v.interval)
	defer t.Stop()
	for range t.C {
		if err := v.pollOnce(); err != nil {
			log.Printf("poll: %v", err)
		}
	}
	select {}
}

func (v *vdevice) pollOnce() error {
	resp, err := v.post("/api/devices/"+v.id+"/poll", twin.PollRequest{})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("poll status %d", resp.StatusCode)
	}
	var pr twin.PollResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return err
	}
	for _, item := range pr.Config {
		v.handle(item)
	}
	return nil
}

func (v *vdevice) handle(item twin.DesiredItem) {
	oldValue := v.state.Config[item.Key]
	if v.isRejected(item.Key, item.Value) {
		log.Printf("REJECT %s=%s (v%d): 设备不接受该配置", item.Key, item.Value, item.Version)
		if err := v.sendReport(twin.ReportItem{
			Key:     item.Key,
			Value:   oldValue,
			Version: item.Version,
			Applied: false,
			Reason:  "rejected by local policy",
		}); err != nil {
			log.Printf("reject report: %v", err)
		}
		return
	}
	v.state.Config[item.Key] = item.Value
	if err := v.save(); err != nil {
		log.Printf("save local state: %v", err)
	}
	log.Printf("APPLIED %s=%s (v%d)", item.Key, item.Value, item.Version)

	if v.stale[item.Key] && item.Version > 1 {
		// 模拟迟到上报：设备实际已应用 v3，但网络层先到了 v2 的旧确认。
		stale := twin.ReportItem{
			Key:     item.Key,
			Value:   oldValue,
			Version: item.Version - 1,
			Applied: true,
		}
		log.Printf("STALE ACK %s v%d (旧确认，服务应判定过期)", item.Key, stale.Version)
		if err := v.sendReport(stale); err != nil {
			log.Printf("stale report: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
		// 随后正确的 v3 确认到达，期望状态才变为 effective。
	}

	if err := v.sendReport(twin.ReportItem{
		Key:     item.Key,
		Value:   item.Value,
		Version: item.Version,
		Applied: true,
	}); err != nil {
		log.Printf("applied report: %v", err)
	}
}

func (v *vdevice) isRejected(key, value string) bool {
	for _, r := range v.rules {
		if r.key == key && (r.value == "" || r.value == value) {
			return true
		}
	}
	return false
}

func (v *vdevice) sendSnapshot() error {
	return v.reportBody(twin.ReportRequest{Snapshot: v.state.Config})
}

func (v *vdevice) sendReport(item twin.ReportItem) error {
	return v.reportBody(twin.ReportRequest{Items: []twin.ReportItem{item}})
}

func (v *vdevice) reportBody(body twin.ReportRequest) error {
	resp, err := v.post("/api/devices/"+v.id+"/report", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("report status %d", resp.StatusCode)
	}
	return nil
}

func (v *vdevice) post(path string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return v.client.Post(v.server+path, "application/json", bytes.NewReader(data))
}

func (v *vdevice) load() error {
	v.state = localState{Config: map[string]string{}}
	data, err := os.ReadFile(v.db)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, &v.state)
}

func (v *vdevice) save() error {
	data, err := json.MarshalIndent(v.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := v.db + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, v.db)
}
