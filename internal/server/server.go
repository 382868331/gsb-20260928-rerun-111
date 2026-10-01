package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"twindemo/internal/twin"
)

type Server struct {
	store *twin.Store
	now   func() time.Time
}

func New(store *twin.Store) *Server {
	return &Server{store: store, now: time.Now}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /devices", s.listDevices)
	mux.HandleFunc("POST /changes/preview", s.previewChange)
	mux.HandleFunc("POST /changes", s.publishChange)
	mux.HandleFunc("GET /changes", s.listChanges)
	mux.HandleFunc("GET /changes/{id}", s.getChange)
	mux.HandleFunc("POST /changes/{id}/compensate", s.compensateChange)
	mux.HandleFunc("PUT /devices/{id}/desired", s.setDesired)
	mux.HandleFunc("POST /devices/{id}/pull", s.pull)
	mux.HandleFunc("POST /devices/{id}/report", s.report)
	mux.HandleFunc("POST /devices/{id}/offline", s.offline)
	mux.HandleFunc("GET /devices/{id}/twin", s.getTwin)
	mux.HandleFunc("GET /devices/{id}/verify", s.verifyDevice)
	mux.HandleFunc("GET /devices/{id}/conflicts", s.listConflicts)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"devices": s.store.ListViews(s.now())})
}

func (s *Server) setDesired(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cfg := twin.Config{}
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErr(w, http.StatusBadRequest, "期望配置必须是 JSON 对象: "+err.Error())
		return
	}
	_, rev, err := s.store.SetDesired(id, cfg, s.now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"device_id":   id,
		"desired_rev": rev,
		"change_id":   s.mustView(id).DesiredChangeID,
		"status":      string(twin.StatusPending),
		"status_text": statusText(twin.StatusPending),
	})
}

type changeRequest struct {
	Targets map[string]twin.Config `json:"targets"`
	Mode    string                 `json:"mode"`
}

func decodeChangeRequest(r *http.Request) (changeRequest, error) {
	var req changeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return req, err
	}
	return req, nil
}

func parseMode(raw string) (twin.PublishMode, error) {
	switch twin.PublishMode(raw) {
	case "", twin.PublishModeSupersede:
		return twin.PublishModeSupersede, nil
	case twin.PublishModeQueue:
		return twin.PublishModeQueue, nil
	}
	return "", fmt.Errorf("未知发布模式 %q，可选 queue 或 supersede", raw)
}

func (s *Server) previewChange(w http.ResponseWriter, r *http.Request) {
	req, err := decodeChangeRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "请求必须是 {\"targets\":{\"设备ID\":{...}}} JSON: "+err.Error())
		return
	}
	p, err := s.store.PreviewChange(req.Targets, s.now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) publishChange(w http.ResponseWriter, r *http.Request) {
	req, err := decodeChangeRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "请求必须是 {\"targets\":{\"设备ID\":{...}}} JSON: "+err.Error())
		return
	}
	mode, err := parseMode(req.Mode)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ch, err := s.store.PublishChange(req.Targets, twin.ChangeKindTarget, "", mode, s.now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, ch)
}

func (s *Server) getChange(w http.ResponseWriter, r *http.Request) {
	ch, ok := s.store.Change(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "变更不存在")
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

func (s *Server) listChanges(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"changes": s.store.ListChanges()})
}

func (s *Server) compensateChange(w http.ResponseWriter, r *http.Request) {
	var req changeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "补偿请求必须是 JSON 对象: "+err.Error())
		return
	}
	ch, err := s.store.PublishCompensation(r.PathValue("id"), req.Targets, s.now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, ch)
}

func (s *Server) pull(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d, changed, err := s.store.Pull(id, s.now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 对设备侧只暴露期望与下发版本；设备看不到已确认状态。
	writeJSON(w, http.StatusOK, map[string]any{
		"changed":        changed,
		"desired_rev":    d.DesiredRev,
		"desired_config": d.DesiredConfig,
		"change_id":      d.DesiredChangeID,
	})
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	req := twin.ReportRequest{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "上报内容必须是 JSON 对象: "+err.Error())
		return
	}
	_, conflict, stale, err := s.store.Report(id, req, s.now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := map[string]any{
		"stale": stale,
		"twin":  s.mustView(id),
	}
	if stale {
		resp["message"] = "上报版本已过时，已记录冲突，未更新确认状态"
	}
	if conflict != nil {
		resp["conflict"] = conflict
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) mustView(id string) *twin.TwinView {
	v, ok := s.store.View(id, s.now())
	if !ok {
		return nil
	}
	return v
}

func (s *Server) offline(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.MarkOffline(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"device_id": id, "online": "false"})
}

func (s *Server) getTwin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v, ok := s.store.View(id, s.now())
	if !ok {
		writeErr(w, http.StatusNotFound, "设备不存在")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) verifyDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v, ok := s.store.Verify(id, s.now())
	if !ok {
		writeErr(w, http.StatusNotFound, "设备不存在")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) listConflicts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	includeResolved, _ := strconv.ParseBool(r.URL.Query().Get("resolved"))
	list := s.store.Conflicts(id, includeResolved || strings.EqualFold(r.URL.Query().Get("all"), "true"))
	writeJSON(w, http.StatusOK, map[string]any{"device_id": id, "conflicts": list})
}

func statusText(st twin.Status) string {
	switch st {
	case twin.StatusEffective:
		return "已生效"
	case twin.StatusDelivered:
		return "已下发，待设备确认生效"
	case twin.StatusPartial:
		return "部分生效，仍有配置项未确认"
	case twin.StatusFailed:
		return "未生效，设备确认失败"
	case twin.StatusSuperseded:
		return "已被新目标替代，历史保留"
	case twin.StatusQueued:
		return "排队中，等待先前操作结束"
	default:
		return "待下发"
	}
}
