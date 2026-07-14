package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nicholas-fedor/shoutrrr"
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
	case "rollback":
		s.handleRollbackContainer(w, r, name)
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
	sessionID := s.state.StartSession(name)
	defer s.state.EndSession(name)

	startTime := time.Now()
	s.events.Broadcast(Event{Type: EventUpdateStarted, Container: name, Message: "Updating", Data: map[string]string{"session_id": sessionID}})
	s.events.BroadcastLog(name, "Checking for updates...")

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

	// Save current image for rollback before updating
	s.state.SavePreviousImage(name, imageName, string(target.ImageID()))

	if !target.IsRunning() {
		s.events.BroadcastLog(name, "Container is not running")
		s.events.Broadcast(Event{Type: EventUpdateFailed, Container: name, Message: "Not running"})
		return
	}

	isSelf := s.selfContainerID != "" && string(target.ID()) == s.selfContainerID

	// For self-update, check GitHub version first to avoid unnecessary pull.
	if isSelf {
		s.events.BroadcastLog(name, "Checking GitHub for new version...")
		updateInfo, err := CheckForUpdate(s.version)
		if err != nil {
			s.events.BroadcastLog(name, "Version check failed: "+err.Error())
		} else if !updateInfo.Available {
			elapsed := time.Since(startTime).Truncate(time.Millisecond)
			s.events.BroadcastLog(name, fmt.Sprintf("Already on latest version %s (%s)", s.version, elapsed))
			s.events.Broadcast(Event{Type: EventUpdateComplete, Container: name, Message: "Up to date"})
			s.state.MarkUpdated(name)
			s.state.AddHistory(HistoryEntry{
				Container: name,
				Timestamp: time.Now(),
				Status:    "success",
				Duration:  time.Since(startTime),
				SessionID: sessionID,
			})
			return
		} else {
			s.events.BroadcastLog(name, fmt.Sprintf("New version available: %s (current: %s)", updateInfo.LatestVer, s.version))
		}
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
			SessionID: sessionID,
		})
		return
	}

	s.events.BroadcastLog(name, "New image available: "+newImage.ShortID())

	if isSelf {
		s.events.BroadcastLog(name, "Self-update detected — starting new container first, then stopping this one")

		s.events.BroadcastLog(name, "Starting replacement container...")
		startStart := time.Now()
		newID, err := s.client.StartContainer(ctx, target)
		if err != nil {
			s.events.BroadcastLog(name, "Failed to start replacement: "+err.Error())
			s.events.Broadcast(Event{Type: EventUpdateFailed, Container: name, Message: "Start failed"})
			return
		}
		startDuration := time.Since(startStart).Truncate(time.Millisecond)
		s.events.BroadcastLog(name, "Replacement started ("+startDuration.String()+") — "+string(newID)[:12])

		elapsed := time.Since(startTime).Truncate(time.Millisecond)
		s.events.BroadcastLog(name, fmt.Sprintf("Self-update complete (%s) — container is restarting", elapsed))
		s.state.MarkUpdated(name)
		s.state.AddHistory(HistoryEntry{
			Container: name,
			Timestamp: time.Now(),
			Status:    "success",
			Duration:  time.Since(startTime),
			SessionID: sessionID,
		})

		if err := s.client.StopAndRemoveContainer(ctx, target, 30*time.Second); err != nil {
			logrus.WithError(err).Warn("Failed to stop old self container (may already be gone)")
		}
		return
	}

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
		SessionID: sessionID,
	})
}

// handleRollbackContainer reverts a container to its previous image.
func (s *Server) handleRollbackContainer(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		s.writeError(w, "method not allowed", 405)
		return
	}

	prevImage, _, hasPrevious := s.state.GetPreviousImage(name)
	if !hasPrevious {
		s.writeError(w, "no previous image recorded — cannot rollback", 400)
		return
	}

	s.events.BroadcastLog(name, "Rolling back to: "+prevImage)

	sessionID := s.state.StartSession(name)
	defer s.state.EndSession(name)

	go func() {
		s.events.Broadcast(Event{Type: EventUpdateStarted, Container: name, Message: "Rollback", Data: map[string]string{"session_id": sessionID}})
		startTime := time.Now()

		ctx := context.Background()
		containers, err := s.client.ListContainers(ctx)
		if err != nil {
			s.events.BroadcastLog(name, "Failed to list containers: "+err.Error())
			s.events.Broadcast(Event{Type: EventUpdateFailed, Container: name, Message: "Rollback failed"})
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

		if !target.IsRunning() {
			s.events.BroadcastLog(name, "Container is not running")
			s.events.Broadcast(Event{Type: EventUpdateFailed, Container: name, Message: "Not running"})
			return
		}

		isSelf := s.selfContainerID != "" && string(target.ID()) == s.selfContainerID

		if isSelf {
			s.events.BroadcastLog(name, "Self-rollback detected — starting old version first, then stopping this one")
		} else {
			s.events.BroadcastLog(name, "Stopping container...")
			if err := s.client.StopAndRemoveContainer(ctx, target, 30*time.Second); err != nil {
				s.events.BroadcastLog(name, "Failed to stop: "+err.Error())
				s.events.Broadcast(Event{Type: EventUpdateFailed, Container: name, Message: "Stop failed"})
				return
			}
		}

		s.events.BroadcastLog(name, "Starting previous version: "+prevImage)
		newID, err := s.client.StartContainer(ctx, target)
		if err != nil {
			s.events.BroadcastLog(name, "Failed to start: "+err.Error())
			s.events.Broadcast(Event{Type: EventUpdateFailed, Container: name, Message: "Start failed"})
			return
		}

		if isSelf {
			elapsed := time.Since(startTime).Truncate(time.Millisecond)
			s.events.BroadcastLog(name, fmt.Sprintf("Self-rollback complete (%s) — container is restarting", elapsed))
			s.state.ClearPreviousImage(name)
			s.state.AddHistory(HistoryEntry{
				Container: name,
				Timestamp: time.Now(),
				Status:    "rollback",
				Duration:  time.Since(startTime),
				SessionID: sessionID,
			})

			if err := s.client.StopAndRemoveContainer(ctx, target, 30*time.Second); err != nil {
				logrus.WithError(err).Warn("Failed to stop old self container during rollback (may already be gone)")
			}
			return
		}

		s.client.WaitForContainerHealthy(ctx, newID, 5*time.Minute)

		elapsed := time.Since(startTime).Truncate(time.Millisecond)
		s.events.BroadcastLog(name, fmt.Sprintf("Rollback complete (%s)", elapsed))
		s.events.Broadcast(Event{Type: EventUpdateComplete, Container: name, Message: "Rolled back"})
		s.state.ClearPreviousImage(name)
		s.state.AddHistory(HistoryEntry{
			Container: name,
			Timestamp: time.Now(),
			Status:    "rollback",
			Duration:  time.Since(startTime),
			SessionID: sessionID,
		})
	}()

	s.writeJSON(w, map[string]string{"status": "ok", "message": "rollback started", "target_image": prevImage})
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

	// Fetch Docker containers once (avoid O(n^2) re-listing).
	dockerContainers, err := s.client.ListContainers(ctx)
	if err != nil {
		logrus.WithError(err).Error("Failed to list Docker containers for staleness check")
		s.writeError(w, "failed to list Docker containers: "+err.Error(), 500)
		return
	}

	// Index Docker containers by name for O(1) lookup.
	dockerByName := make(map[string]types.Container, len(dockerContainers))
	for _, dc := range dockerContainers {
		dockerByName[dc.Name()] = dc
	}

	stale := 0
	failed := 0
	upToDate := 0
	for i := range containers {
		if i >= limit {
			break
		}
		dc, ok := dockerByName[containers[i].Name]
		if !ok {
			containers[i].CheckError = "container not found in Docker"
			failed++
			s.events.BroadcastLog(containers[i].Name, "Check failed: container not found in Docker")
			continue
		}

		isStale, _, err := s.client.IsContainerStale(ctx, dc, types.UpdateParams{})
		if err != nil {
			errMsg := err.Error()
			containers[i].CheckError = errMsg
			failed++
			s.events.BroadcastLog(containers[i].Name, "Check failed: "+errMsg)
			logrus.WithError(err).WithField("container", containers[i].Name).Warn("Staleness check failed")
			continue
		}
		if isStale {
			containers[i].Stale = true
			stale++
			s.events.BroadcastLog(containers[i].Name, "Update available")
		} else {
			upToDate++
		}
	}

	msg := fmt.Sprintf("Scan complete: %d checked, %d updates, %d up to date, %d failed", len(containers), stale, upToDate, failed)
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

// handleAPILogs returns persisted log entries filtered by container, session, or date range.
func (s *Server) handleAPILogs(w http.ResponseWriter, r *http.Request) {
	container := r.URL.Query().Get("container")
	sessionID := r.URL.Query().Get("session")

	var since *time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err == nil {
			since = &t
		}
	}

	logs := s.state.GetLogs(container, sessionID, since)
	if logs == nil {
		logs = []LogEntry{}
	}
	s.writeJSON(w, logs)
}

// handleAPITestNotification sends a test message to the configured notification URL.
func (s *Server) handleAPITestNotification(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, "method not allowed", 405)
		return
	}

	settings := s.state.GetSettings()
	if settings.NotificationURL == "" {
		s.writeError(w, "no notification URL configured", 400)
		return
	}

	notifyURL := settings.NotificationURL

	// Auto-convert Discord webhook URLs to shoutrrr format.
	// Input: https://discord.com/api/webhooks/WEBHOOK_ID/WEBHOOK_TOKEN
	// Output: discord://WEBHOOK_TOKEN@WEBHOOK_ID
	if strings.Contains(notifyURL, "discord.com/api/webhooks/") || strings.Contains(notifyURL, "discordapp.com/api/webhooks/") {
		trimmedURL := strings.TrimRight(notifyURL, "/")
		trimmedURL = strings.TrimPrefix(trimmedURL, "https://")
		trimmedURL = strings.TrimPrefix(trimmedURL, "http://")
		parts := strings.Split(trimmedURL, "/")
		// Expected: [discord.com, api, webhooks, WEBHOOK_ID, WEBHOOK_TOKEN]
		if len(parts) >= 5 {
			webhookID := parts[len(parts)-2]
			token := parts[len(parts)-1]
			notifyURL = fmt.Sprintf("discord://%s@%s", token, webhookID)
			logrus.WithFields(logrus.Fields{
				"original":  settings.NotificationURL,
				"converted": notifyURL,
			}).Debug("Auto-converted Discord webhook URL to shoutrrr format")
		}
	}

	msg := fmt.Sprintf("Dockyard test notification\nVersion: %s\nTime: %s",
		s.version, time.Now().Format("2006-01-02 15:04:05"))

	if err := shoutrrr.Send(notifyURL, msg); err != nil {
		logrus.WithError(err).Error("Test notification failed")
		s.writeError(w, "failed to send: "+err.Error(), 500)
		return
	}

	logrus.Info("Test notification sent successfully")
	s.writeJSON(w, map[string]string{"status": "ok", "message": "test notification sent"})
}
