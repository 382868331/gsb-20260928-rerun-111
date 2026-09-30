// 虚拟设备：通过 HTTP 与孪生服务交互，模拟轮询、应用配置、部分拒绝和迟到上报。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type localState struct {
	ID             string            `json:"id"`
	Online         bool              `json:"online"`
	CachedRev      int64             `json:"cached_rev"`
	CachedConfig   map[string]string `json:"cached_config"`
	CachedChangeID string            `json:"cached_change_id,omitempty"`
	RunningConfig  map[string]string `json:"running_config"`
	LastSeen       time.Time         `json:"last_seen"`
}

func main() {
	serverURL := flag.String("server", "http://127.0.0.1:8080", "孪生服务地址")
	id := flag.String("id", "dev-1", "设备 ID")
	statePath := flag.String("state", "", "本地状态文件，默认 data/device-<id>.json")
	flag.Parse()
	if *statePath == "" {
		*statePath = fmt.Sprintf("data/device-%s.json", *id)
	}

	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: virtualdevice -id dev-1 <poll|apply|late-report|offline|online|resume|restart|state> [-reject k1,k2]")
		os.Exit(2)
	}
	d := &device{server: strings.TrimRight(*serverURL, "/"), id: *id, statePath: *statePath}
	if err := d.load(); err != nil {
		fmt.Fprintf(os.Stderr, "读取本地状态失败: %v\n", err)
		os.Exit(1)
	}

	var err error
	switch args[0] {
	case "poll":
		err = d.poll()
	case "apply":
		reject := map[string]bool{}
		fs := flag.NewFlagSet("apply", flag.ExitOnError)
		rejectFlag := fs.String("reject", "", "逗号分隔的要拒绝的配置键")
		_ = fs.Parse(args[1:])
		for _, k := range strings.Split(*rejectFlag, ",") {
			if k = strings.TrimSpace(k); k != "" {
				reject[k] = true
			}
		}
		err = d.apply(reject, d.st.CachedRev)
	case "late-report":
		// 模拟恢复网络后排队的旧上报才到达：用上一版本号上报。
		oldRev := d.st.CachedRev - 1
		if oldRev < 1 {
			oldRev = 1
		}
		err = d.apply(map[string]bool{}, oldRev)
	case "offline":
		d.st.Online = false
		err = d.post("/offline", nil, nil)
	case "online":
		d.st.Online = true
	case "resume", "restart":
		err = d.resume(rejectFlags(args[1:]))
		args = nil
	case "state":
		d.printState()
	default:
		err = fmt.Errorf("未知子命令 %q", args[0])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
	if err := d.save(); err != nil {
		fmt.Fprintf(os.Stderr, "保存本地状态失败: %v\n", err)
		os.Exit(1)
	}
}

func rejectFlags(args []string) map[string]bool {
	reject := map[string]bool{}
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	rejectFlag := fs.String("reject", "", "逗号分隔的要拒绝的配置键")
	_ = fs.Parse(args)
	for _, k := range strings.Split(*rejectFlag, ",") {
		if k = strings.TrimSpace(k); k != "" {
			reject[k] = true
		}
	}
	return reject
}

type device struct {
	server, id, statePath string
	st                    *localState
}

func (d *device) load() error {
	d.st = &localState{ID: d.id, Online: true, CachedConfig: map[string]string{}, RunningConfig: map[string]string{}}
	data, err := os.ReadFile(d.statePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, d.st)
}

func (d *device) save() error {
	d.st.LastSeen = time.Now()
	data, _ := json.MarshalIndent(d.st, "", "  ")
	return os.WriteFile(d.statePath, data, 0o644)
}

func (d *device) httpJSON(method, path string, body any, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, d.server+"/devices/"+d.id+path, reader)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("解析响应失败: %w: %s", err, string(raw))
		}
	}
	return resp.StatusCode, nil
}

func (d *device) post(path string, body, out any) error {
	code, err := d.httpJSON(http.MethodPost, path, body, out)
	if err != nil {
		return err
	}
	if code >= 300 {
		return fmt.Errorf("服务返回 %d", code)
	}
	return nil
}

func (d *device) poll() error {
	if !d.st.Online {
		return fmt.Errorf("设备离线，无法轮询")
	}
	var resp struct {
		Changed       bool              `json:"changed"`
		DesiredRev    int64             `json:"desired_rev"`
		DesiredConfig map[string]string `json:"desired_config"`
		ChangeID      string            `json:"change_id"`
	}
	if err := d.post("/pull", nil, &resp); err != nil {
		return err
	}
	if resp.Changed {
		d.st.CachedRev = resp.DesiredRev
		d.st.CachedConfig = resp.DesiredConfig
		d.st.CachedChangeID = resp.ChangeID
		fmt.Printf("[设备 %s] 拉取到新期望 change=%s rev=%d %v（仅已下发，尚未生效）\n", d.id, resp.ChangeID, resp.DesiredRev, resp.DesiredConfig)
	} else {
		fmt.Printf("[设备 %s] 无新配置（期望 rev=%d）\n", d.id, resp.DesiredRev)
	}
	return nil
}

func (d *device) resume(reject map[string]bool) error {
	if err := d.poll(); err != nil {
		return err
	}
	if d.st.CachedRev == 0 {
		fmt.Printf("[设备 %s] 重启后没有待继续的配置操作\n", d.id)
		return nil
	}
	fmt.Printf("[设备 %s] 重启恢复本地缓存 change=%s rev=%d，继续确认\n", d.id, d.st.CachedChangeID, d.st.CachedRev)
	return d.apply(reject, d.st.CachedRev)
}

func (d *device) apply(reject map[string]bool, reportRev int64) error {
	applied := map[string]string{}
	rejected := map[string]string{}
	for k, v := range d.st.CachedConfig {
		if reject[k] {
			rejected[k] = v
			continue
		}
		applied[k] = v
		d.st.RunningConfig[k] = v
	}
	// 已拒绝的键从运行配置中删除，模拟设备不支持该参数。
	for k := range reject {
		delete(d.st.RunningConfig, k)
	}
	body := map[string]any{
		"rev":      reportRev,
		"applied":  applied,
		"rejected": rejected,
	}
	var resp map[string]any
	if err := d.post("/report", body, &resp); err != nil {
		return err
	}
	pretty, _ := json.MarshalIndent(resp, "", "  ")
	fmt.Printf("[设备 %s] 按 rev=%d 上报 applied=%v rejected=%v\n服务响应: %s\n", d.id, reportRev, applied, rejected, pretty)
	return nil
}

func (d *device) printState() {
	pretty, _ := json.MarshalIndent(d.st, "", "  ")
	fmt.Println(string(pretty))
}
