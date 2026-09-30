package server

import (
	"encoding/json"
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
	mux.HandleFunc("PUT /devices/{id}/desired", s.setDesired)
	mux.HandleFunc("POST /devices/{id}/preview", s.preview)
	mux.HandleFunc("GET /devices/{id}/changes", s.listChanges)
	mux.HandleFunc("POST /devices/{id}/pull", s.pull)
	mux.HandleFunc("POST /devices/{id}/report", s.report)
	mux.HandleFunc("POST /devices/{id}/offline", s.offline)
	mux.HandleFunc("GET /devices/{id}/twin", s.getTwin)
	mux.HandleFunc("GET /devices/{id}/conflicts", s.listConflicts)
	mux.HandleFunc("POST /targets", s.publishTarget)
	mux.HandleFunc("GET /targets", s.listTargets)
	mux.HandleFunc("GET /targets/{id}", s.getTarget)
	mux.HandleFunc("POST /targets/{id}/compensate", s.compensate)
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
	t, err := s.store.SetDesired(id, cfg, s.now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var rev int64
	if len(t.Items) > 0 {
		rev = t.Items[0].Rev
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"device_id":   id,
		"target_id":   t.ID,
		"desired_rev": rev,
		"status":      string(twin.StatusPending),
		"status_text": statusText(twin.StatusPending),
	})
}

type targetSpec struct {
	Devices map[string]twin.Config `json:"devices"`
}

// preview 只预演差异，不发布、不推进版本。
func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cfg := twin.Config{}
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErr(w, http.StatusBadRequest, "期望配置必须是 JSON 对象: "+err.Error())
		return
	}
	diff := s.store.Preview(id, cfg)
	writeJSON(w, http.StatusOK, map[string]any{"device_id": id, "diff": diff})
}

func (s *Server) listChanges(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	writeJSON(w, http.StatusOK, map[string]any{"device_id": id, "changes": s.store.Changes(id)})
}

func (s *Server) publishTarget(w http.ResponseWriter, r *http.Request) {
	spec := targetSpec{}
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体必须是 {\"devices\":{...}}: "+err.Error())
		return
	}
	t, err := s.store.PublishTarget(spec.Devices, s.now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, s.mustTarget(t.ID))
}

func (s *Server) compensate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	spec := targetSpec{}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体必须是 {\"devices\":{...}}: "+err.Error())
			return
		}
	}
	t, err := s.store.Compensate(id, spec.Devices, s.now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, s.mustTarget(t.ID))
}

func (s *Server) listTargets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"targets": s.store.ListTargets(s.now())})
}

func (s *Server) getTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v := s.mustTarget(id)
	if v == nil {
		writeErr(w, http.StatusNotFound, "目标不存在")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) mustTarget(id string) *twin.TargetView {
	v, _ := s.store.TargetView(id, s.now())
	return v
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
		return "部分生效，设备拒绝了部分配置"
	case twin.StatusSuperseded:
		return "该版本已被更新目标替代"
	default:
		return "待下发"
	}
}
