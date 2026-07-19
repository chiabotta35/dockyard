package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nicholas-fedor/shoutrrr"
	"github.com/sirupsen/logrus"

	dockerContainer "github.com/moby/moby/api/types/container"

	"github.com/dockyard/dockyard/pkg/types"
)

// rollbackContainer wraps a Container and overrides GetCreateConfig and
// ImageName so that StartContainer recreates the container using the
// previous image instead of the current one.
type rollbackContainer struct {
	types.Container
	overrideImage string
}

func (r *rollbackContainer) GetCreateConfig() *dockerContainer.Config {
	cfg := r.Container.GetCreateConfig()
	if cfg != nil {
		cfg.Image = r.overrideImage
	}
	return cfg
}

func (r *rollbackContainer) ImageName() string {
	return r.overrideImage
}

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

func (s *Server) handleLogsPage(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, "logs.html", map[string]interface{}{
		"Title":      "Logs - Dockyard",
		"ActivePage": "logs",
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
	case "clear-image":
		s.handleClearOldImage(w, r, name)
	case "pin-image":
		s.handlePinImage(w, r, name)
	case "check":
		s.handleCheckContainer(w, r, name)
	case "role":
		s.handleSetRole(w, r, name)
	case "logs":
		s.handleContainerLogs(w, r, name)
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

// performSelfUpdate handles self-updates by pulling the new version tag,
// then creating an ephemeral orchestrator container that stops the old
// container, creates and starts a new one with the updated image, and
// cleans up. The orchestrator survives the old container being stopped
// because it is a separate container with its own lifecycle.
func (s *Server) performSelfUpdate(ctx context.Context, name string, target types.Container, updateInfo *UpdateInfo, startTime time.Time, sessionID string) {
	newImageRef := fmt.Sprintf("ghcr.io/%s/%s:%s", GitHubOwner, GitHubRepo, updateInfo.LatestVer)

	s.events.BroadcastLog(name, "Pulling new image: "+newImageRef)

	// Pull the new image via Docker API (not CLI — container is read_only).
	if err := s.client.PullImageByName(ctx, newImageRef); err != nil {
		s.events.BroadcastLog(name, "Failed to pull new image: "+err.Error())
		s.events.Broadcast(Event{Type: EventUpdateFailed, Container: name, Message: "Pull failed: " + err.Error()})
		return
	}

	s.events.BroadcastLog(name, "Image pulled successfully")

	// Remember the old image for cleanup.
	oldImageID := target.ImageID()
	oldImageName := target.ImageName()

	// Create ephemeral orchestrator: a separate container that handles the
	// full replacement sequence (stop old → create new → start new → remove old).
	// This avoids port conflicts because the orchestrator is independent of
	// the old container's lifecycle.
	s.events.BroadcastLog(name, "Creating ephemeral orchestrator for self-update...")
	s.events.Broadcast(Event{
		Type:      EventSelfUpdate,
		Container: name,
		Message:   fmt.Sprintf("Updating to %s", updateInfo.LatestVer),
		Data:      map[string]string{"version": updateInfo.LatestVer},
	})
	_, err := s.client.CreateEphemeralOrchestrator(ctx, target, newImageRef, "")
	if err != nil {
		s.events.BroadcastLog(name, "Failed to create orchestrator: "+err.Error())
		s.events.Broadcast(Event{Type: EventUpdateFailed, Container: name, Message: "Orchestrator failed: " + err.Error()})
		return
	}

	s.events.BroadcastLog(name, "Orchestrator started — replacement in progress")

	elapsed := time.Since(startTime).Truncate(time.Millisecond)
	s.events.BroadcastLog(name, fmt.Sprintf("Self-update complete (%s) — container is restarting", elapsed))
	s.events.Broadcast(Event{Type: EventUpdateComplete, Container: name, Message: fmt.Sprintf("Updated to %s", updateInfo.LatestVer)})
	s.state.MarkUpdated(name)
	s.state.ClearUpdateDetected(name)
	s.state.SaveCheckResult(name, false, "", "")
	s.state.AddHistory(HistoryEntry{
		Container: name,
		Timestamp: time.Now(),
		Status:    "success",
		Duration:  time.Since(startTime),
		SessionID: sessionID,
	})

	// Save old image for rollback.
	if oldImageID != "" {
		s.state.SavePreviousImage(name, oldImageName, string(oldImageID))
	}
}

// performContainerUpdate does the actual Docker pull + restart for a single container.
func (s *Server) performContainerUpdate(name string) {
	sessionID := s.state.StartSession(name)
	defer s.state.EndSession(name)

	// Guard: prevent concurrent updates for the same container.
	s.updatingMu.Lock()
	if s.updating[name] {
		s.updatingMu.Unlock()
		s.events.BroadcastLog(name, "Update already in progress — skipping")
		s.events.Broadcast(Event{Type: EventUpdateComplete, Container: name, Message: "Skipped"})
		s.state.EndSession(name)
		return
	}
	s.updating[name] = true
	s.updatingMu.Unlock()
	defer func() {
		s.updatingMu.Lock()
		delete(s.updating, name)
		s.updatingMu.Unlock()
	}()

	startTime := time.Now()
	s.events.Broadcast(Event{Type: EventUpdateStarted, Container: name, Message: "Updating", Data: map[string]string{"session_id": sessionID}})
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

	// Save current image for rollback before updating
	s.state.SavePreviousImage(name, imageName, string(target.ImageID()))

	if !target.IsRunning() {
		s.events.BroadcastLog(name, "Container is not running")
		s.events.Broadcast(Event{Type: EventUpdateFailed, Container: name, Message: "Not running"})
		return
	}

	isSelf := s.selfContainerID != "" && string(target.ID()) == s.selfContainerID

	// For self-update, check GitHub version first to avoid unnecessary pull.
	// The self container uses a version tag (e.g. v0.1.13), so IsContainerStale
	// would pull the same tag and always report "not stale". Instead, when a
	// new version is found, pull the new tag and recreate directly.
	if isSelf {
		s.events.BroadcastLog(name, "Checking GitHub for new version...")
		updateInfo, err := CheckForUpdate(s.version)
		if err != nil {
			s.events.BroadcastLog(name, "Version check failed: "+err.Error())
			// Fall through to normal stale check as fallback.
		} else if !updateInfo.Available {
			elapsed := time.Since(startTime).Truncate(time.Millisecond)
			s.events.BroadcastLog(name, fmt.Sprintf("Already on latest version %s (%s)", s.version, elapsed))
			s.events.Broadcast(Event{Type: EventUpdateComplete, Container: name, Message: "Up to date"})
			s.state.MarkUpdated(name)
			s.state.ClearUpdateDetected(name)
			s.state.SaveCheckResult(name, false, "", "")
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
			s.performSelfUpdate(ctx, name, target, updateInfo, startTime, sessionID)
			return
		}
	}

	// Pull the image and check for staleness in one call.
	stale, newImage, err := s.client.IsContainerStale(ctx, target, types.UpdateParams{})
	if err != nil {
		s.events.BroadcastLog(name, "Pull/check failed: "+err.Error())
		s.events.Broadcast(Event{Type: EventUpdateFailed, Container: name, Message: "Pull failed"})
		return
	}

	if !stale {
		elapsed := time.Since(startTime).Truncate(time.Millisecond)
		s.events.BroadcastLog(name, "Already up to date ("+elapsed.String()+")")
		s.events.Broadcast(Event{Type: EventUpdateComplete, Container: name, Message: "Up to date"})
		s.state.MarkUpdated(name)
		s.state.SaveCheckResult(name, false, "", "")
		s.state.ClearUpdateDetected(name)
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

	// Remember the old image ID before we replace the container.
	oldImageID := target.ImageID()

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
	s.state.ClearUpdateDetected(name)
	s.state.SaveCheckResult(name, false, "", "")
	s.state.AddHistory(HistoryEntry{
		Container: name,
		Timestamp: time.Now(),
		Status:    "success",
		Duration:  time.Since(startTime),
		SessionID: sessionID,
	})

	// Save old image for rollback.
	if oldImageID != "" && newImage != oldImageID {
		s.state.SavePreviousImage(name, imageName, string(oldImageID))
		s.events.BroadcastLog(name, "Old image saved for rollback")
	}
}

// handleRollbackContainer reverts a container to a specific previous image.
// Accepts POST with body {"image": "...", "image_id": "..."} or falls back to most recent.
func (s *Server) handleRollbackContainer(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		s.writeError(w, "method not allowed", 405)
		return
	}

	var req struct {
		Image   string `json:"image"`
		ImageID string `json:"image_id"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	prevImages := s.state.GetPreviousImages(name)
	if len(prevImages) == 0 {
		s.writeError(w, "no previous image recorded — cannot rollback", 400)
		return
	}

	// Find the requested image or fall back to the most recent.
	prevImage := prevImages[0].Image
	if req.Image != "" {
		for _, pi := range prevImages {
			if pi.Image == req.Image {
				prevImage = pi.Image
				break
			}
		}
	}

	s.events.BroadcastLog(name, "Rolling back to: "+prevImage)

	sessionID := s.state.StartSession(name)
	defer s.state.EndSession(name)

	go func() {
		s.events.Broadcast(Event{Type: EventUpdateStarted, Container: name, Message: "Rollback", Data: map[string]string{"session_id": sessionID}})
		startTime := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
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

		// Wrap the target so StartContainer uses the previous image.
		wrapped := &rollbackContainer{Container: target, overrideImage: prevImage}
		newID, err := s.client.StartContainer(ctx, wrapped)
		if err != nil {
			s.events.BroadcastLog(name, "Failed to start: "+err.Error())
			s.events.Broadcast(Event{Type: EventUpdateFailed, Container: name, Message: "Start failed"})
			return
		}

		if isSelf {
			elapsed := time.Since(startTime).Truncate(time.Millisecond)
			s.events.BroadcastLog(name, fmt.Sprintf("Self-rollback complete (%s) — container is restarting", elapsed))
			s.state.ClearPreviousImages(name)
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
		s.state.ClearPreviousImages(name)
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

func (s *Server) handleClearOldImage(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		s.writeError(w, "method not allowed", 405)
		return
	}

	var req struct {
		Image   string `json:"image"`
		ImageID string `json:"image_id"`
		All     bool   `json:"all"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	prevImages := s.state.GetPreviousImages(name)
	if len(prevImages) == 0 {
		s.writeError(w, "no previous images to remove", 400)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if req.All {
		removed := 0
		for _, pi := range prevImages {
			if pi.Pinned {
				s.events.BroadcastLog(name, "Skipping pinned image: "+pi.Image)
				continue
			}
			s.events.BroadcastLog(name, "Removing old image: "+pi.Image)
			if err := s.client.RemoveImageByID(ctx, types.ImageID(pi.ImageID), pi.Image); err != nil {
				s.events.BroadcastLog(name, "Failed to remove: "+err.Error())
			}
			removed++
		}
		s.state.ClearUnpinnedPreviousImages(name)
		if removed > 0 {
			s.events.BroadcastLog(name, fmt.Sprintf("Removed %d old image(s)", removed))
		}
	} else {
		// Remove a specific image from the list.
		found := false
		for i, pi := range prevImages {
			if pi.Image == req.Image || (req.ImageID != "" && pi.ImageID == req.ImageID) {
				if pi.Pinned {
					s.writeError(w, "image is pinned, unpin first", 400)
					return
				}
				s.events.BroadcastLog(name, "Removing old image: "+pi.Image)
				if err := s.client.RemoveImageByID(ctx, types.ImageID(pi.ImageID), pi.Image); err != nil {
					s.events.BroadcastLog(name, "Failed to remove: "+err.Error())
					s.writeError(w, err.Error(), 500)
					return
				}
				s.state.RemovePreviousImage(name, i)
				found = true
				break
			}
		}
		if !found {
			s.writeError(w, "image not found in previous images", 400)
			return
		}
	}

	s.writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handlePinImage(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		s.writeError(w, "method not allowed", 405)
		return
	}
	var req struct {
		Index  int  `json:"index"`
		Pinned bool `json:"pinned"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, "invalid request body", 400)
		return
	}
	if err := s.state.PinPreviousImage(name, req.Index, req.Pinned); err != nil {
		s.writeError(w, err.Error(), 500)
		return
	}
	s.writeJSON(w, map[string]string{"status": "ok"})
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

func (s *Server) handleSetRole(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		s.writeError(w, "method not allowed", 405)
		return
	}
	var req struct {
		Role string `json:"role"` // "sidecar", "database", "main", or "" for auto-detect
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, "invalid request body", 400)
		return
	}
	switch req.Role {
	case "sidecar", "database", "main", "":
	default:
		s.writeError(w, "invalid role: must be sidecar, database, main, or empty", 400)
		return
	}
	s.state.SetRoleOverride(name, req.Role)
	s.writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleAPIStackAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/stacks/"), "/")
	if len(parts) < 2 {
		s.writeError(w, "usage: /api/stacks/{name}/role", 400)
		return
	}
	stackName := parts[0]
	action := parts[1]
	switch action {
	case "role":
		s.handleSetStackRole(w, r, stackName)
	default:
		s.writeError(w, "unknown action", 400)
	}
}

func (s *Server) handleSetStackRole(w http.ResponseWriter, r *http.Request, stackName string) {
	if r.Method != http.MethodPost {
		s.writeError(w, "method not allowed", 405)
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, "invalid request body", 400)
		return
	}
	switch req.Role {
	case "sidecar", "database", "main", "":
	default:
		s.writeError(w, "invalid role", 400)
		return
	}
	count := 0
	for _, ci := range s.getContainerList() {
		if ci.ComposeStack == stackName {
			s.state.SetRoleOverride(ci.Name, req.Role)
			count++
		}
	}
	s.writeJSON(w, map[string]interface{}{"status": "ok", "updated": count})
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
		if settings.ImageRetentionHrs < 0 || settings.ImageRetentionHrs > 10080 {
			settings.ImageRetentionHrs = 1440
		}

		// Capture old schedule/tz to detect changes.
		old := s.state.GetSettings()

		if err := s.state.UpdateSettings(func(curr *Settings) {
			*curr = settings
		}); err != nil {
			s.writeError(w, "failed to save", 500)
			return
		}

		// Signal the cron goroutine if schedule or timezone changed.
		if settings.Schedule != old.Schedule || settings.Timezone != old.Timezone {
			select {
			case s.scheduleChanged <- struct{}{}:
			default:
			}
		}

		s.writeJSON(w, map[string]string{"status": "ok"})
	default:
		s.writeError(w, "method not allowed", 405)
	}
}

func (s *Server) handleAPIHistory(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, s.state.GetHistory())
}

// handleAPICheckNow performs a real staleness check against Docker for all containers in parallel.
func (s *Server) handleAPICheckNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, "method not allowed", 405)
		return
	}

	if !s.checkMu.TryLock() {
		s.writeError(w, "check already in progress", 409)
		return
	}
	defer s.checkMu.Unlock()

	s.events.Broadcast(Event{Type: EventScanStarted, Message: "Scan started"})
	s.events.BroadcastLog("", "Checking all containers for updates...")

	ctx := context.Background()

	// Fetch Docker containers once — used for both the UI list and staleness checks.
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

	// Build ContainerInfo list from the same Docker containers.
	containers := s.buildContainerList(dockerContainers)

	// Filter by scope: "main" excludes databases and sidecars.
	scope := r.URL.Query().Get("scope")
	if scope == "main" {
		filtered := containers[:0]
		for _, c := range containers {
			if !c.IsDatabase && !c.IsSidecar {
				filtered = append(filtered, c)
			}
		}
		containers = filtered
	}

	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if v, err := strconv.Atoi(limitStr); err == nil && v > 0 && v <= 500 {
			limit = v
		}
	}

	// Check containers in parallel (max 10 concurrent for faster scans).
	type result struct {
		index        int
		stale        bool
		err          string
		latestVersion string
	}
	results := make(chan result, limit)
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup

	toCheck := len(containers)
	if toCheck > limit {
		toCheck = limit
	}

	for i := 0; i < toCheck; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			dc, ok := dockerByName[containers[i].Name]
			if !ok {
				results <- result{index: i, err: "container not found in Docker"}
				return
			}

			// For the self container, prefer GitHub version check.
			// Fall back to digest comparison if GitHub is unreachable.
			if containers[i].IsSelf {
				updateInfo, err := CheckForUpdate(s.version)
				if err != nil {
					logrus.WithField("container", containers[i].Name).Debug("GitHub version check failed, falling back to digest: ", err)
				} else {
					results <- result{index: i, stale: updateInfo.Available}
					return
				}
			}

			isStale, _, err := s.client.IsContainerStale(ctx, dc, types.UpdateParams{})
			if err != nil {
				results <- result{index: i, err: err.Error()}
				return
			}
			var latestVersion string
			if isStale {
				latestVersion = s.client.ImageVersion(ctx, dc.ImageName())
			}
			results <- result{index: i, stale: isStale, latestVersion: latestVersion}
		}(i)
	}

	// Wait for all goroutines and close results channel.
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results.
	stale := 0
	failed := 0
	upToDate := 0
	var details []checkDetail
	for res := range results {
		if res.err != "" {
			containers[res.index].CheckError = res.err
			failed++
			s.events.BroadcastLog(containers[res.index].Name, "Check failed: "+res.err)
			logrus.WithField("container", containers[res.index].Name).Warn("Staleness check failed: ", res.err)
			details = append(details, checkDetail{name: containers[res.index].Name, image: containers[res.index].Image, errMsg: res.err, isSelf: containers[res.index].IsSelf, changelogURL: containers[res.index].ChangelogURL})
			s.state.SaveCheckResult(containers[res.index].Name, false, res.err, "")
		} else if res.stale {
			containers[res.index].Stale = true
			stale++
			s.events.BroadcastLog(containers[res.index].Name, "Update available")
			details = append(details, checkDetail{name: containers[res.index].Name, image: containers[res.index].Image, stale: true, isSelf: containers[res.index].IsSelf, changelogURL: containers[res.index].ChangelogURL})
			s.state.SaveCheckResult(containers[res.index].Name, true, "", "")
			s.state.MarkUpdateDetected(containers[res.index].Name)
			if res.latestVersion != "" {
				s.state.SaveLatestVersion(containers[res.index].Name, res.latestVersion)
			}
		} else {
			upToDate++
			s.state.SaveCheckResult(containers[res.index].Name, false, "", "")
			s.state.ClearUpdateDetected(containers[res.index].Name)
			s.state.SaveLatestVersion(containers[res.index].Name, "")
		}
	}

	msg := fmt.Sprintf("Scan complete: %d checked, %d updates, %d up to date, %d failed", toCheck, stale, upToDate, failed)
	s.events.Broadcast(Event{Type: EventScanComplete, Message: msg})
	s.events.BroadcastLog("", msg)

	// Check self container separately for notification (hidden from dashboard grid).
	if s.selfContainerID != "" {
		selfInfo, err := CheckForUpdate(s.version)
		if err == nil && selfInfo.Available {
			stale++
			details = append(details, checkDetail{name: "dockyard", image: "ghcr.io/" + GitHubOwner + "/" + GitHubRepo + ":" + selfInfo.LatestVer, stale: true, isSelf: true, changelogURL: selfInfo.ReleaseURL})
			s.state.SetChangelogURL("dockyard", selfInfo.ReleaseURL)
		}
	}

	// Send notification if there are updates or errors.
	s.sendCheckNotification(stale, failed, upToDate, toCheck, details)

	if len(containers) > limit {
		containers = containers[:limit]
	}
	s.writeJSON(w, containers)
}

// sendCheckNotification sends a notification if there are updates or errors.
func (s *Server) sendCheckNotification(updates, errors, upToDate, total int, details []checkDetail) {
	settings := s.state.GetSettings()
	if settings.NotificationURL == "" {
		return
	}

	now := time.Now().UTC().Format("2006-01-02 15:04 UTC")
	var msgParts []string

	// Determine if we need to @everyone
	shouldMention := errors > 0 // always @ for errors
	if !shouldMention {
		for _, d := range details {
			if d.stale && s.state.ShouldMention(d.name) {
				shouldMention = true
				break
			}
		}
	}

	if shouldMention {
		msgParts = append(msgParts, "@everyone")
	}

	// Header
	header := fmt.Sprintf("Dockyard Scan \u2014 %s", now)
	msgParts = append(msgParts, header)
	msgParts = append(msgParts, fmt.Sprintf("%d/%d checked \u2022 %d up to date", total, total, upToDate))
	msgParts = append(msgParts, "")

	// Containers needing updates with details.
	if updates > 0 {
		msgParts = append(msgParts, fmt.Sprintf("\u2B50 **%d update(s) available:**", updates))
		for _, d := range details {
			if d.stale {
				s.state.MarkMentioned(d.name)
				msgParts = append(msgParts, fmt.Sprintf("  \u2022 **%s**", d.name))
				msgParts = append(msgParts, fmt.Sprintf("    Image: `%s`", d.image))
				if d.changelogURL != "" {
					msgParts = append(msgParts, fmt.Sprintf("    Release Notes: %s", d.changelogURL))
				}
			}
		}
		msgParts = append(msgParts, "")
	}

	// Containers with check errors.
	if errors > 0 {
		msgParts = append(msgParts, fmt.Sprintf("\u274C **%d check error(s):**", errors))
		for _, d := range details {
			if d.errMsg != "" {
				msgParts = append(msgParts, fmt.Sprintf("  \u2022 **%s** (`%s`)", d.name, d.image))
				msgParts = append(msgParts, fmt.Sprintf("    Error: %s", d.errMsg))
			}
		}
		msgParts = append(msgParts, "")
	}

	if updates == 0 && errors == 0 {
		msgParts = append(msgParts, "\u2705 All containers up to date")
	}

	msgParts = append(msgParts, fmt.Sprintf("Dockyard %s", s.version))

	msg := strings.Join(msgParts, "\n")

	notifyURL := s.convertDiscordURL(settings.NotificationURL)
	if err := shoutrrr.Send(notifyURL, msg); err != nil {
		logrus.WithError(err).Warn("Failed to send check notification")
	}
}

type checkDetail struct {
	name        string
	image       string
	stale       bool
	errMsg      string
	isSelf      bool
	changelogURL string
}

// convertDiscordURL auto-converts Discord webhook URLs to shoutrrr format.
func (s *Server) convertDiscordURL(url string) string {
	if strings.Contains(url, "discord.com/api/webhooks/") || strings.Contains(url, "discordapp.com/api/webhooks/") {
		trimmed := strings.TrimRight(url, "/")
		trimmed = strings.TrimPrefix(trimmed, "https://")
		trimmed = strings.TrimPrefix(trimmed, "http://")
		parts := strings.Split(trimmed, "/")
		if len(parts) >= 5 {
			webhookID := parts[len(parts)-2]
			token := parts[len(parts)-1]
			return fmt.Sprintf("discord://%s@%s", token, webhookID)
		}
	}
	return url
}

// runAutoCheck performs a background staleness check (same as handleAPICheckNow but without HTTP response).
func (s *Server) runAutoCheck(ctx context.Context) {
	if !s.checkMu.TryLock() {
		logrus.Info("Auto-check skipped: another check is already in progress")
		return
	}
	defer s.checkMu.Unlock()

	logrus.Info("Running auto-check for container updates")
	s.events.Broadcast(Event{Type: EventScanStarted, Message: "Auto-check started"})

	dockerContainers, err := s.client.ListContainers(ctx)
	if err != nil {
		logrus.WithError(err).Error("Auto-check: failed to list Docker containers")
		s.events.Broadcast(Event{Type: EventScanComplete, Message: "Auto-check failed: " + err.Error()})
		return
	}

	dockerByName := make(map[string]types.Container, len(dockerContainers))
	for _, dc := range dockerContainers {
		dockerByName[dc.Name()] = dc
	}

	containers := s.buildContainerList(dockerContainers)
	if len(containers) == 0 {
		return
	}

	// Auto-check only checks main containers (skip databases and sidecars).
	filtered := containers[:0]
	for _, c := range containers {
		if !c.IsDatabase && !c.IsSidecar {
			filtered = append(filtered, c)
		}
	}
	containers = filtered
	if len(containers) == 0 {
		return
	}

	type result struct {
		index         int
		stale         bool
		err           string
		latestVersion string
	}
	results := make(chan result, len(containers))
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup

	for i := range containers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Skip containers checked within the last 2 minutes to avoid
			// redundant checks when the cron fires or manual check just ran.
			if cs := s.state.GetContainerState(containers[i].Name); cs.CheckedAt != nil {
				if time.Since(*cs.CheckedAt) < 2*time.Minute {
					results <- result{index: i}
					return
				}
			}

			// Skip containers that are currently being updated.
			s.updatingMu.Lock()
			updating := s.updating[containers[i].Name]
			s.updatingMu.Unlock()
			if updating {
				results <- result{index: i, err: "update in progress"}
				return
			}

			dc, ok := dockerByName[containers[i].Name]
			if !ok {
				results <- result{index: i, err: "not found in Docker"}
				return
			}

			// For the self container, prefer GitHub version check.
			// Fall back to digest comparison if GitHub is unreachable.
			if containers[i].IsSelf {
				updateInfo, err := CheckForUpdate(s.version)
				if err != nil {
					logrus.WithField("container", containers[i].Name).Debug("GitHub version check failed, falling back to digest: ", err)
				} else {
					results <- result{index: i, stale: updateInfo.Available}
					return
				}
			}

			isStale, _, err := s.client.IsContainerStale(ctx, dc, types.UpdateParams{})
			if err != nil {
				results <- result{index: i, err: err.Error()}
				return
			}
			var latestVersion string
			if isStale {
				latestVersion = s.client.ImageVersion(ctx, dc.ImageName())
			}
			results <- result{index: i, stale: isStale, latestVersion: latestVersion}
		}(i)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	stale := 0
	failed := 0
	upToDate := 0
	var details []checkDetail
	for res := range results {
		if res.err != "" {
			failed++
			details = append(details, checkDetail{name: containers[res.index].Name, image: containers[res.index].Image, errMsg: res.err, isSelf: containers[res.index].IsSelf, changelogURL: containers[res.index].ChangelogURL})
			s.state.SaveCheckResult(containers[res.index].Name, false, res.err, "")
		} else if res.stale {
			stale++
			details = append(details, checkDetail{name: containers[res.index].Name, image: containers[res.index].Image, stale: true, isSelf: containers[res.index].IsSelf, changelogURL: containers[res.index].ChangelogURL})
			s.state.SaveCheckResult(containers[res.index].Name, true, "", "")
			s.state.MarkUpdateDetected(containers[res.index].Name)
			if res.latestVersion != "" {
				s.state.SaveLatestVersion(containers[res.index].Name, res.latestVersion)
			}
		} else {
			upToDate++
			s.state.SaveCheckResult(containers[res.index].Name, false, "", "")
			s.state.ClearUpdateDetected(containers[res.index].Name)
			s.state.SaveLatestVersion(containers[res.index].Name, "")
		}
	}

	// Check self container separately for notification (hidden from dashboard grid).
	if s.selfContainerID != "" {
		selfInfo, err := CheckForUpdate(s.version)
		if err == nil && selfInfo.Available {
			stale++
			details = append(details, checkDetail{name: "dockyard", image: "ghcr.io/" + GitHubOwner + "/" + GitHubRepo + ":" + selfInfo.LatestVer, stale: true, isSelf: true, changelogURL: selfInfo.ReleaseURL})
			s.state.SetChangelogURL("dockyard", selfInfo.ReleaseURL)
		}
	}

	s.sendCheckNotification(stale, failed, upToDate, len(containers), details)

	msg := fmt.Sprintf("Auto-check: %d checked, %d updates, %d up to date, %d failed", len(containers), stale, upToDate, failed)
	s.events.Broadcast(Event{Type: EventScanComplete, Message: msg})

	logrus.WithFields(logrus.Fields{
		"total":    len(containers),
		"stale":    stale,
		"failed":   failed,
		"upToDate": upToDate,
	}).Info("Auto-check complete")

	// Auto-update stale containers in auto mode (same as manual Check Now).
	for _, d := range details {
		if !d.stale {
			continue
		}
		if d.isSelf {
			continue
		}
		cs := s.state.GetContainerState(d.name)
		if cs.UpdateMode != ModeAuto {
			continue
		}
		if s.state.IsDeferred(d.name) {
			continue
		}
		skip := isDatabaseImage(d.image)
		if !skip {
			for _, c := range containers {
				if c.Name == d.name {
					skip = c.IsSidecar || c.IsDatabase
					break
				}
			}
		}
		if skip {
			continue
		}
		// Skip if already being updated or recently updated.
		s.updatingMu.Lock()
		updating := s.updating[d.name]
		s.updatingMu.Unlock()
		if updating {
			continue
		}
		logrus.WithField("container", d.name).Info("Auto-check: triggering auto-update")
		s.events.BroadcastLog(d.name, "Auto-update triggered by scheduled check")
		go s.performContainerUpdate(d.name)
	}
}

// handleCheckContainer checks staleness for a single container.
func (s *Server) handleCheckContainer(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		s.writeError(w, "method not allowed", 405)
		return
	}

	ctx := context.Background()
	dockerContainers, err := s.client.ListContainers(ctx)
	if err != nil {
		s.writeError(w, "failed to list containers: "+err.Error(), 500)
		return
	}

	for _, dc := range dockerContainers {
		if dc.Name() == name {
			// For the self container, prefer GitHub version check.
			// Fall back to digest comparison if GitHub is unreachable.
			isSelf := s.selfContainerID != "" && string(dc.ID()) == s.selfContainerID
			var isStale bool
			if isSelf {
				updateInfo, err := CheckForUpdate(s.version)
				if err != nil {
					logrus.WithField("container", name).Debug("GitHub version check failed, falling back to digest: ", err)
				} else {
					if updateInfo.Available {
						s.events.BroadcastLog(name, "Update available")
					} else {
						s.events.BroadcastLog(name, "Up to date")
					}
					s.state.SaveCheckResult(name, updateInfo.Available, "", "")
					s.writeJSON(w, map[string]interface{}{
						"name":  name,
						"stale": updateInfo.Available,
					})
					return
				}
			}

			isStale, _, err := s.client.IsContainerStale(ctx, dc, types.UpdateParams{})
			if err != nil {
				s.events.BroadcastLog(name, "Check failed: "+err.Error())
				s.state.SaveCheckResult(name, false, err.Error(), "")
				s.writeJSON(w, map[string]interface{}{
					"name":        name,
					"stale":       false,
					"check_error": err.Error(),
				})
				return
			}

			if isStale {
				s.events.BroadcastLog(name, "Update available")
			} else {
				s.events.BroadcastLog(name, "Up to date")
			}
			s.state.SaveCheckResult(name, isStale, "", "")

			s.writeJSON(w, map[string]interface{}{
				"name":  name,
				"stale": isStale,
			})
			return
		}
	}

	s.writeError(w, "container not found", 404)
}

// handleContainerLogs returns recent Docker container logs.
func (s *Server) handleContainerLogs(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		s.writeError(w, "method not allowed", 405)
		return
	}

	ctx := context.Background()
	dockerContainers, err := s.client.ListContainers(ctx)
	if err != nil {
		s.writeError(w, "failed to list containers: "+err.Error(), 500)
		return
	}

	lines := 200
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			lines = n
		}
	}

	for _, dc := range dockerContainers {
		if dc.Name() == name {
			logs, err := s.client.GetContainerLogs(ctx, dc.ID(), lines)
			if err != nil {
				logrus.WithError(err).WithField("container", name).Warn("Failed to get container logs")
				s.writeJSON(w, map[string]interface{}{
					"name":  name,
					"logs":  "Failed to retrieve logs: " + err.Error(),
					"error": err.Error(),
				})
				return
			}
			s.writeJSON(w, map[string]interface{}{
				"name": name,
				"logs": logs,
			})
			return
		}
	}

	s.writeError(w, "container not found", 404)
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

	// Disable write timeout for SSE so the server doesn't kill the connection
	// every 60 seconds (the default WriteTimeout). Without this, EventSource
	// reconnects and replays history, causing duplicate log messages.
	if rc := http.NewResponseController(w); rc != nil {
		rc.SetWriteDeadline(time.Time{})
	}

	ch := s.events.Subscribe()
	defer s.events.Unsubscribe(ch)

	// Send keepalive pings every 30s to prevent proxies/browsers from
	// dropping idle SSE connections (which would trigger history replay).
	ctx := r.Context()
	pingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				fmt.Fprintf(w, ": ping\n\n")
				flusher.Flush()
			}
		}
	}()
	defer close(pingDone)
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

func (s *Server) purgeExpiredImages() {
	ctx := context.Background()
	removed := s.state.PurgeExpiredImages()
	for _, ri := range removed {
		s.events.BroadcastLog(ri.Name, "Auto-purged old image: "+ri.Image)
		if err := s.client.RemoveImageByID(ctx, types.ImageID(ri.ImageID), ri.Image); err != nil {
			s.events.BroadcastLog(ri.Name, "Failed to remove old image: "+err.Error())
		}
	}
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

	notifyURL := s.convertDiscordURL(settings.NotificationURL)
	if notifyURL != settings.NotificationURL {
		logrus.WithFields(logrus.Fields{
			"original":  settings.NotificationURL,
			"converted": notifyURL,
		}).Debug("Auto-converted Discord webhook URL to shoutrrr format")
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

func (s *Server) handleAPICheckStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, "method not allowed", 405)
		return
	}

	s.autoCheckMu.RLock()
	lastCheck := s.lastAutoCheck
	nextCheck := s.nextAutoCheck
	lastPurge := s.lastPurge
	s.autoCheckMu.RUnlock()

	schedule := s.state.GetSettings().Schedule
	retentionMin := s.state.GetSettings().ImageRetentionHrs

	resp := map[string]interface{}{
		"last_check":         nil,
		"next_check":         nil,
		"schedule":           schedule,
		"interval_ms":        0,
		"image_retention_min": retentionMin,
		"last_purge":         nil,
	}

	if !lastCheck.IsZero() {
		resp["last_check"] = lastCheck.Format(time.RFC3339)
	}
	if !nextCheck.IsZero() {
		resp["next_check"] = nextCheck.Format(time.RFC3339)
		ms := time.Until(nextCheck).Milliseconds()
		if ms > 0 {
			resp["interval_ms"] = ms
		}
	}
	if !lastPurge.IsZero() {
		resp["last_purge"] = lastPurge.Format(time.RFC3339)
	}

	s.writeJSON(w, resp)
}

// handleAPIDebugContainers returns raw Docker API container data + processed list for debugging.
func (s *Server) handleAPIDebugContainers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, "method not allowed", 405)
		return
	}

	ctx := context.Background()
	rawContainers, err := s.client.ListContainers(ctx)
	if err != nil {
		s.writeError(w, "ListContainers failed: "+err.Error(), 500)
		return
	}

	type rawInfo struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Image   string `json:"image"`
		Running bool   `json:"running"`
	}

	type processedInfo struct {
		rawInfo
		InResult bool   `json:"in_result"`
		Error    string `json:"error,omitempty"`
	}

	raw := make([]rawInfo, len(rawContainers))
	for i, c := range rawContainers {
		raw[i] = rawInfo{
			ID:      string(c.ID()),
			Name:    c.Name(),
			Image:   c.ImageName(),
			Running: c.IsRunning(),
		}
	}

	processed := s.buildContainerList(rawContainers)
	processedMap := make(map[string]bool, len(processed))
	for _, p := range processed {
		processedMap[p.Name] = true
	}

	pi := make([]processedInfo, len(rawContainers))
	for i, c := range rawContainers {
		name := c.Name()
		pi[i] = processedInfo{
			rawInfo: rawInfo{
				ID:      string(c.ID()),
				Name:    name,
				Image:   c.ImageName(),
				Running: c.IsRunning(),
			},
			InResult: processedMap[name],
		}
	}

	s.writeJSON(w, map[string]interface{}{
		"self_container_id": s.selfContainerID,
		"raw_count":         len(rawContainers),
		"processed_count":   len(processed),
		"raw":               raw,
		"processed":         pi,
	})
}
