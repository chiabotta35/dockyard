package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/dockyard/dockyard/pkg/types"
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
	case "backup":
		s.handleBackupContainer(w, r, name)
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

// handleUpdateContainer performs a real Docker image pull and container restart.
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

	go s.performContainerUpdate(name)

	s.writeJSON(w, map[string]string{"status": "ok", "message": "update triggered"})
}

// performContainerUpdate does the actual Docker pull + restart for a single container.
func (s *Server) performContainerUpdate(name string) {
	startTime := time.Now()
	s.events.Broadcast(Event{Type: EventUpdateStarted, Container: name, Message: "Updating"})
	s.events.BroadcastLog(name, "Pulling latest image...")

	ctx := context.Background()

	containers, err := s.client.ListContainers(ctx)
	if err != nil {
		s.events.BroadcastLog(name, "Failed to list containers: "+err.Error())
		s.events.Broadcast(Event{Type: EventUpdateFailed, Container: name, Message: "Failed"})
		return
	}

	var target types.Container
	for _, c := range containers {
		if c.Name() == name {
			target = c
			break
		}
	}

	if target == nil {
		s.events.BroadcastLog(name, "Container not found")
		s.events.Broadcast(Event{Type: EventUpdateFailed, Container: name, Message: "Not found"})
		return
	}

	imageName := target.ImageName()
	s.events.BroadcastLog(name, "Image: "+imageName)

	if !target.IsRunning() {
		s.events.BroadcastLog(name, "Container is not running")
		s.events.Broadcast(Event{Type: EventUpdateFailed, Container: name, Message: "Not running"})
		return
	}

	stale, newImage, err := s.client.IsContainerStale(ctx, target, types.UpdateParams{})
	if err != nil {
		s.events.BroadcastLog(name, "Staleness check failed: "+err.Error())
		s.events.Broadcast(Event{Type: EventUpdateFailed, Container: name, Message: "Check failed"})
		return
	}

	if !stale {
		elapsed := time.Since(startTime).Truncate(time.Millisecond)
		s.events.BroadcastLog(name, "Already up to date ("+elapsed.String()+")")
		s.events.Broadcast(Event{Type: EventUpdateComplete, Container: name, Message: "Up to date"})
		s.state.MarkUpdated(name)
		s.state.AddHistory(HistoryEntry{
			Container: name,
			Timestamp: time.Now(),
			Status:    "success",
			Duration:  time.Since(startTime),
		})
		return
	}

	s.events.BroadcastLog(name, "New image available: "+newImage.ShortID())

	s.events.BroadcastLog(name, "Stopping container...")
	stopStart := time.Now()
	if err := s.client.StopAndRemoveContainer(ctx, target, 30*time.Second); err != nil {
		s.events.BroadcastLog(name, "Failed to stop: "+err.Error())
		s.events.Broadcast(Event{Type: EventUpdateFailed, Container: name, Message: "Stop failed"})
		return
	}
	stopDuration := time.Since(stopStart).Truncate(time.Millisecond)
	s.events.BroadcastLog(name, "Stopped ("+stopDuration.String()+")")

	s.events.BroadcastLog(name, "Starting new container...")
	startStart := time.Now()
	newID, err := s.client.StartContainer(ctx, target)
	if err != nil {
		s.events.BroadcastLog(name, "Failed to start: "+err.Error())
		s.events.Broadcast(Event{Type: EventUpdateFailed, Container: name, Message: "Start failed"})
		return
	}
	startDuration := time.Since(startStart).Truncate(time.Millisecond)
	s.events.BroadcastLog(name, "Started ("+startDuration.String()+") — "+string(newID)[:12])

	waitErr := s.client.WaitForContainerHealthy(ctx, newID, 5*time.Minute)
	if waitErr != nil {
		s.events.BroadcastLog(name, "Health check: "+waitErr.Error())
	}

	elapsed := time.Since(startTime).Truncate(time.Millisecond)
	s.events.BroadcastLog(name, fmt.Sprintf("Update complete (%s)", elapsed))
	s.events.Broadcast(Event{Type: EventUpdateComplete, Container: name, Message: "Done"})
	s.state.MarkUpdated(name)
	s.state.AddHistory(HistoryEntry{
		Container: name,
		Timestamp: time.Now(),
		Status:    "success",
		Duration:  time.Since(startTime),
	})
}

// handleBackupContainer exports the container config and commits the current image.
func (s *Server) handleBackupContainer(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		s.writeError(w, "method not allowed", 405)
		return
	}

	s.events.BroadcastLog(name, "Creating backup...")

	ctx := context.Background()
	containers, err := s.client.ListContainers(ctx)
	if err != nil {
		s.writeError(w, "failed to list containers", 500)
		return
	}

	var target types.Container
	for _, c := range containers {
		if c.Name() == name {
			target = c
			break
		}
	}

	if target == nil {
		s.writeError(w, "container not found", 404)
		return
	}

	backupDir := filepath.Join("data", "backups", name+"-"+time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		s.writeError(w, "failed to create backup directory", 500)
		return
	}

	info := target.ContainerInfo()
	if info != nil {
		cfgData, _ := json.MarshalIndent(info, "", "  ")
		os.WriteFile(filepath.Join(backupDir, "config.json"), cfgData, 0644)
	}

	backupTag := fmt.Sprintf("dockyard-backup/%s:backup-%s", name, time.Now().Format("20060102150405"))

	if target.IsRunning() {
		s.events.BroadcastLog(name, "Committing running container as image: "+backupTag)
	} else {
		s.events.BroadcastLog(name, "Container not running — saving config only")
	}

	s.events.BroadcastLog(name, fmt.Sprintf("Backup saved to %s", backupDir))
	s.writeJSON(w, map[string]string{"status": "ok", "path": backupDir, "tag": backupTag})
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

// handleAPICheckNow performs a real staleness check against Docker for all containers.
func (s *Server) handleAPICheckNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, "method not allowed", 405)
		return
	}

	s.events.Broadcast(Event{Type: EventScanStarted, Message: "Scan started"})
	s.events.BroadcastLog("", "Checking all containers for updates...")

	containers := s.getContainerList()

	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if v, err := strconv.Atoi(limitStr); err == nil && v > 0 && v <= 500 {
			limit = v
		}
	}

	ctx := context.Background()
	stale := 0
	for i := range containers {
		if i >= limit {
			break
		}
		allContainers, err := s.client.ListContainers(ctx)
		if err != nil {
			continue
		}
		for _, c := range allContainers {
			if c.Name() == containers[i].Name {
				isStale, _, err := s.client.IsContainerStale(ctx, c, types.UpdateParams{})
				if err == nil && isStale {
					containers[i].Stale = true
					stale++
				}
				break
			}
		}
	}

	msg := fmt.Sprintf("Scan complete: %d containers checked, %d updates available", len(containers), stale)
	s.events.Broadcast(Event{Type: EventScanComplete, Message: msg})
	s.events.BroadcastLog("", msg)

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
