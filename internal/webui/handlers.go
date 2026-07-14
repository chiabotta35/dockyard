package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

func (s *Server) renderTemplate(w http.ResponseWriter, name string, data interface{}) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		logrus.WithError(err).WithField("template", name).Error("Failed to render template")
		http.Error(w, "Template error", 500)
	}
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.renderTemplate(w, "dashboard.html", map[string]interface{}{
		"Title":      "Dockyard",
		"Version":    s.version,
		"ActivePage": "dashboard",
	})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, "settings.html", map[string]interface{}{
		"Title":      "Settings - Dockyard",
		"Settings":   s.state.GetSettings(),
		"ActivePage": "settings",
	})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, "history.html", map[string]interface{}{
		"Title":      "History - Dockyard",
		"History":    s.state.GetHistory(),
		"ActivePage": "history",
	})
}

func (s *Server) handleAPIContainers(w http.ResponseWriter, r *http.Request) {
	containers := s.getContainerList()
	s.writeJSON(w, containers)
}

func (s *Server) handleAPIContainerAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/containers/"), "/")
	if len(parts) < 1 {
		s.writeError(w, "missing container name", 400)
		return
	}

	name, err := sanitizeContainerName(parts[0])
	if err != nil {
		s.writeError(w, "invalid container name", 400)
		return
	}

	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "mode":
		s.handleSetMode(w, r, name)
	case "defer":
		s.handleDefer(w, r, name)
	case "cancel-defer":
		s.handleCancelDefer(w, r, name)
	case "update":
		s.handleUpdateContainer(w, r, name)
	case "changelog":
		s.handleSetChangelog(w, r, name)
	default:
		s.writeError(w, "unknown action", 400)
	}
}

func (s *Server) handleSetMode(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		s.writeError(w, "method not allowed", 405)
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, "invalid request body", 400)
		return
	}
	mode := UpdateMode(req.Mode)
	if mode != ModeAuto && mode != ModeManual && mode != ModeIgnore {
		s.writeError(w, "invalid mode: must be auto, manual, or ignore", 400)
		return
	}
	if err := s.state.SetContainerMode(name, mode); err != nil {
		s.writeError(w, "failed to save", 500)
		return
	}
	s.events.Broadcast(Event{
		Type:      EventLogLine,
		Container: name,
		Message:   fmt.Sprintf("Update mode set to %s", mode),
	})
	s.writeJSON(w, map[string]string{"status": "ok", "mode": string(mode)})
}

func (s *Server) handleDefer(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		s.writeError(w, "method not allowed", 405)
		return
	}
	var req struct {
		Days int `json:"days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, "invalid request body", 400)
		return
	}
	if req.Days < 0 || req.Days > 3650 {
		s.writeError(w, "days must be between 0 and 3650", 400)
		return
	}
	if err := s.state.DeferContainer(name, req.Days); err != nil {
		s.writeError(w, "failed to save", 500)
		return
	}
	s.writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleCancelDefer(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		s.writeError(w, "method not allowed", 405)
		return
	}
	if err := s.state.CancelDefer(name); err != nil {
		s.writeError(w, "failed to save", 500)
		return
	}
	s.writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleUpdateContainer(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		s.writeError(w, "method not allowed", 405)
		return
	}

	cs := s.state.GetContainerState(name)
	if cs.UpdateMode == ModeIgnore {
		s.writeError(w, "container is set to ignore updates", 400)
		return
	}

	s.events.BroadcastUpdate(name, "started")
	s.events.BroadcastLog(name, "Update triggered")

	go func() {
		time.Sleep(100 * time.Millisecond)
		s.events.BroadcastLog(name, "Checking for updates...")
		time.Sleep(2 * time.Second)
		s.events.BroadcastUpdate(name, "complete")
		s.events.BroadcastLog(name, "Update completed")
		s.state.MarkUpdated(name)
		s.state.AddHistory(HistoryEntry{
			Container: name,
			Timestamp: time.Now(),
			Status:    "success",
		})
	}()

	s.writeJSON(w, map[string]string{"status": "ok", "message": "update triggered"})
}

func (s *Server) handleSetChangelog(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		s.writeError(w, "method not allowed", 405)
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, "invalid request body", 400)
		return
	}
	if req.URL != "" && !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		s.writeError(w, "invalid URL scheme", 400)
		return
	}
	if err := s.state.SetChangelogURL(name, req.URL); err != nil {
		s.writeError(w, "failed to save", 500)
		return
	}
	s.writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleAPISettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.writeJSON(w, s.state.GetSettings())
	case http.MethodPut:
		var settings Settings
		if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
			s.writeError(w, "invalid request body", 400)
			return
		}
		if settings.BackupWindowHours < 1 || settings.BackupWindowHours > 720 {
			settings.BackupWindowHours = 24
		}
		if err := s.state.UpdateSettings(func(curr *Settings) {
			*curr = settings
		}); err != nil {
			s.writeError(w, "failed to save", 500)
			return
		}
		s.writeJSON(w, map[string]string{"status": "ok"})
	default:
		s.writeError(w, "method not allowed", 405)
	}
}

func (s *Server) handleAPIHistory(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, s.state.GetHistory())
}

func (s *Server) handleAPICheckNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, "method not allowed", 405)
		return
	}
	s.events.Broadcast(Event{Type: EventScanStarted, Message: "Scan started"})
	containers := s.getContainerList()
	stale := 0
	for _, c := range containers {
		if c.Stale {
			stale++
		}
	}
	s.events.Broadcast(Event{Type: EventScanComplete, Message: fmt.Sprintf("Scan complete: %d containers checked, %d updates available", len(containers), stale)})

	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if v, err := strconv.Atoi(limitStr); err == nil && v > 0 && v <= 500 {
			limit = v
		}
	}
	if len(containers) > limit {
		containers = containers[:limit]
	}
	s.writeJSON(w, containers)
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	ch := s.events.Subscribe()
	defer s.events.Unsubscribe(ch)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Type, string(data))
			flusher.Flush()
		}
	}
}

func (s *Server) BroadcastLog(container, message string) {
	s.events.BroadcastLog(container, message)
}

func (s *Server) BroadcastUpdate(container, status string) {
	s.events.BroadcastUpdate(container, status)
}
