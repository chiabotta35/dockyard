package webui

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

type UpdateMode string

const (
	ModeAuto    UpdateMode = "auto"
	ModeManual  UpdateMode = "manual"
	ModeIgnore  UpdateMode = "ignore"
)

type ContainerState struct {
	UpdateMode       UpdateMode `json:"update_mode"`
	DeferUntil       *time.Time `json:"defer_until,omitempty"`
	ChangelogURL     string     `json:"changelog_url,omitempty"`
	LastUpdated      *time.Time `json:"last_updated,omitempty"`
	PreviousImage    string     `json:"previous_image,omitempty"`
	PreviousImageID  string     `json:"previous_image_id,omitempty"`
	CheckError       string     `json:"check_error,omitempty"`
	IsStale          bool       `json:"is_stale,omitempty"`
	CheckedAt        *time.Time `json:"checked_at,omitempty"`
	LatestImage      string     `json:"latest_image,omitempty"`
	UpdateDetectedAt *time.Time `json:"update_detected_at,omitempty"`
	LastMentionAt    *time.Time `json:"last_mention_at,omitempty"`
}

type Settings struct {
	Schedule          string `json:"schedule"`
	Timezone          string `json:"timezone"`
	Cleanup           bool   `json:"cleanup"`
	CooldownDelay     string `json:"cooldown_delay"`
	StopTimeout       string `json:"stop_timeout"`
	BackupRetention   bool   `json:"backup_retention"`
	BackupWindowHours int    `json:"backup_window_hours"`
	MonitorOnly       bool   `json:"monitor_only"`
	RollingRestart    bool   `json:"rolling_restart"`
	LifecycleHooks    bool   `json:"lifecycle_hooks"`
	NotificationURL   string `json:"notification_url"`
	UpdateOnStart     bool   `json:"update_on_start"`
}

type HistoryEntry struct {
	Container string        `json:"container"`
	OldDigest string        `json:"old_digest"`
	NewDigest string        `json:"new_digest"`
	ImageName string        `json:"image_name"`
	Timestamp time.Time     `json:"timestamp"`
	Status    string        `json:"status"`
	Error     string        `json:"error,omitempty"`
	Duration  time.Duration `json:"duration,omitempty"`
	SessionID string        `json:"session_id"`
}

type LogEntry struct {
	Container string    `json:"container"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
	SessionID string    `json:"session_id"`
}

type State struct {
	Containers map[string]*ContainerState `json:"containers"`
	Settings   Settings                   `json:"settings"`
	History    []HistoryEntry             `json:"history"`
	Logs       []LogEntry                 `json:"-"`
	sessions   map[string]string          // container name -> session ID
	mu         sync.RWMutex
	sessionMu  sync.RWMutex
	filePath   string
	logPath    string
	logDirty   bool
	logMu      sync.Mutex
}

func NewState(dataDir string) *State {
	logrus.WithField("dataDir", dataDir).Info("Initializing state store")
	s := &State{
		Containers: make(map[string]*ContainerState),
		Settings: Settings{
			Schedule:          "0 3 * * *",
			Timezone:          "UTC",
			Cleanup:           true,
			CooldownDelay:     "0s",
			StopTimeout:       "30s",
			BackupRetention:   false,
			BackupWindowHours: 24,
			MonitorOnly:       false,
			RollingRestart:    false,
			LifecycleHooks:    false,
			UpdateOnStart:     false,
		},
		History:  make([]HistoryEntry, 0),
		Logs:     make([]LogEntry, 0),
		sessions: make(map[string]string),
		filePath: filepath.Join(dataDir, "state.json"),
		logPath:  filepath.Join(dataDir, "logs.json"),
	}
	s.load()
	s.loadLogs()
	s.loadFromEnv()
	s.CleanOldLogs(7 * 24 * time.Hour)
	logrus.WithFields(logrus.Fields{
		"schedule":        s.Settings.Schedule,
		"monitor_only":    s.Settings.MonitorOnly,
		"cleanup":         s.Settings.Cleanup,
		"update_on_start": s.Settings.UpdateOnStart,
	}).Info("State store initialized")
	return s
}

func (s *State) loadFromEnv() {
	s.mu.Lock()
	defer s.mu.Unlock()

	changed := false

	if v := os.Getenv("DOCKYARD_SCHEDULE"); v != "" {
		s.Settings.Schedule = v
		changed = true
	}
	if v := os.Getenv("DOCKYARD_TIMEZONE"); v != "" {
		s.Settings.Timezone = v
		changed = true
	}
	if v := os.Getenv("DOCKYARD_CLEANUP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			s.Settings.Cleanup = b
			changed = true
		}
	}
	if v := os.Getenv("DOCKYARD_COOLDOWN_DELAY"); v != "" {
		s.Settings.CooldownDelay = v
		changed = true
	}
	if v := os.Getenv("DOCKYARD_STOP_TIMEOUT"); v != "" {
		s.Settings.StopTimeout = v
		changed = true
	}
	if v := os.Getenv("DOCKYARD_MONITOR_ONLY"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			s.Settings.MonitorOnly = b
			changed = true
		}
	}
	if v := os.Getenv("DOCKYARD_ROLLING_RESTART"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			s.Settings.RollingRestart = b
			changed = true
		}
	}
	if v := os.Getenv("DOCKYARD_LIFECYCLE_HOOKS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			s.Settings.LifecycleHooks = b
			changed = true
		}
	}
	if v := os.Getenv("DOCKYARD_UPDATE_ON_START"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			s.Settings.UpdateOnStart = b
			changed = true
		}
	}
	if v := os.Getenv("DOCKYARD_BACKUP_RETENTION"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			s.Settings.BackupRetention = b
			changed = true
		}
	}
	if v := os.Getenv("DOCKYARD_BACKUP_WINDOW_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 720 {
			s.Settings.BackupWindowHours = n
			changed = true
		}
	}
	if v := os.Getenv("DOCKYARD_NOTIFICATION_URL"); v != "" {
		s.Settings.NotificationURL = v
		changed = true
	}

	if changed {
		logrus.Info("State updated from environment variables")
		s.mu.Unlock()
		s.save()
		s.mu.Lock()
	}
}

func (s *State) load() {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		logrus.WithField("filePath", s.filePath).Debug("No existing state file found, using defaults")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	json.Unmarshal(data, s)
	logrus.WithField("filePath", s.filePath).Debug("Loaded state from disk")
}

func (s *State) save() error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.filePath)
	os.MkdirAll(dir, 0755)
	return os.WriteFile(s.filePath, data, 0600)
}

func (s *State) GetContainerState(name string) *ContainerState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cs, ok := s.Containers[name]
	if !ok {
		mode := ModeManual
		return &ContainerState{UpdateMode: mode}
	}
	return cs
}

func (s *State) SetContainerMode(name string, mode UpdateMode) error {
	s.mu.Lock()
	cs, ok := s.Containers[name]
	if !ok {
		cs = &ContainerState{UpdateMode: mode}
		s.Containers[name] = cs
	}
	cs.UpdateMode = mode
	s.mu.Unlock()
	return s.save()
}

func (s *State) DeferContainer(name string, days int) error {
	s.mu.Lock()
	cs, ok := s.Containers[name]
	if !ok {
		cs = &ContainerState{UpdateMode: ModeManual}
		s.Containers[name] = cs
	}
	if days <= 0 {
		far := time.Now().AddDate(0, 0, 365*10)
		cs.DeferUntil = &far
	} else {
		t := time.Now().AddDate(0, 0, days)
		cs.DeferUntil = &t
	}
	s.mu.Unlock()
	return s.save()
}

func (s *State) CancelDefer(name string) error {
	s.mu.Lock()
	if cs, ok := s.Containers[name]; ok {
		cs.DeferUntil = nil
	}
	s.mu.Unlock()
	return s.save()
}

func (s *State) IsDeferred(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cs, ok := s.Containers[name]
	if !ok || cs.DeferUntil == nil {
		return false
	}
	return time.Now().Before(*cs.DeferUntil)
}

func (s *State) GetSettings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Settings
}

func (s *State) UpdateSettings(fn func(*Settings)) error {
	s.mu.Lock()
	fn(&s.Settings)
	s.mu.Unlock()
	return s.save()
}

func (s *State) AddHistory(entry HistoryEntry) error {
	s.mu.Lock()
	s.History = append([]HistoryEntry{entry}, s.History...)
	if len(s.History) > 500 {
		s.History = s.History[:500]
	}
	s.mu.Unlock()
	return s.save()
}

func (s *State) GetHistory() []HistoryEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]HistoryEntry, len(s.History))
	copy(out, s.History)
	return out
}

func (s *State) SetChangelogURL(name, url string) error {
	s.mu.Lock()
	cs, ok := s.Containers[name]
	if !ok {
		cs = &ContainerState{UpdateMode: ModeManual}
		s.Containers[name] = cs
	}
	cs.ChangelogURL = url
	s.mu.Unlock()
	return s.save()
}

func (s *State) MarkUpdated(name string) error {
	s.mu.Lock()
	if cs, ok := s.Containers[name]; ok {
		now := time.Now()
		cs.LastUpdated = &now
	}
	s.mu.Unlock()
	return s.save()
}

// WasRecentlyUpdated returns true if the container was successfully updated
// within the given cooldown duration. This prevents re-triggering updates
// for containers that were just restarted.
func (s *State) WasRecentlyUpdated(name string, cooldown time.Duration) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cs, ok := s.Containers[name]
	if !ok || cs.LastUpdated == nil {
		return false
	}
	return time.Since(*cs.LastUpdated) < cooldown
}

func (s *State) GetAllContainerStates() map[string]*ContainerState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*ContainerState, len(s.Containers))
	for k, v := range s.Containers {
		cp := *v
		out[k] = &cp
	}
	return out
}

func (s *State) SavePreviousImage(name, image, imageID string) error {
	s.mu.Lock()
	cs, ok := s.Containers[name]
	if !ok {
		cs = &ContainerState{UpdateMode: ModeManual}
		s.Containers[name] = cs
	}
	cs.PreviousImage = image
	cs.PreviousImageID = imageID
	s.mu.Unlock()
	return s.save()
}

func (s *State) GetPreviousImage(name string) (string, string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cs, ok := s.Containers[name]
	if !ok || cs.PreviousImage == "" {
		return "", "", false
	}
	return cs.PreviousImage, cs.PreviousImageID, true
}

func (s *State) ClearPreviousImage(name string) error {
	s.mu.Lock()
	if cs, ok := s.Containers[name]; ok {
		cs.PreviousImage = ""
		cs.PreviousImageID = ""
	}
	s.mu.Unlock()
	return s.save()
}

func (s *State) SaveCheckResult(name string, isStale bool, checkErr string, latestImage string) error {
	s.mu.Lock()
	cs, ok := s.Containers[name]
	if !ok {
		cs = &ContainerState{UpdateMode: ModeManual}
		s.Containers[name] = cs
	}
	now := time.Now()
	cs.IsStale = isStale
	cs.CheckError = checkErr
	cs.CheckedAt = &now
	cs.LatestImage = latestImage
	s.mu.Unlock()
	return s.save()
}

func (s *State) ClearCheckResult(name string) error {
	s.mu.Lock()
	if cs, ok := s.Containers[name]; ok {
		cs.IsStale = false
		cs.CheckError = ""
		cs.CheckedAt = nil
		cs.LatestImage = ""
	}
	s.mu.Unlock()
	return s.save()
}

func (s *State) MarkUpdateDetected(name string) {
	s.mu.Lock()
	cs, ok := s.Containers[name]
	if !ok {
		cs = &ContainerState{UpdateMode: ModeManual}
		s.Containers[name] = cs
	}
	if cs.UpdateDetectedAt == nil {
		now := time.Now()
		cs.UpdateDetectedAt = &now
	}
	s.mu.Unlock()
	s.save()
}

func (s *State) ClearUpdateDetected(name string) {
	s.mu.Lock()
	if cs, ok := s.Containers[name]; ok {
		cs.UpdateDetectedAt = nil
		cs.LastMentionAt = nil
	}
	s.mu.Unlock()
	s.save()
}

func (s *State) ShouldMention(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cs, ok := s.Containers[name]
	if !ok || cs.UpdateDetectedAt == nil {
		return false
	}
	detected := *cs.UpdateDetectedAt
	if time.Since(detected) < 24*time.Hour {
		return false
	}
	if cs.LastMentionAt == nil {
		return true
	}
	return time.Since(*cs.LastMentionAt) >= 7*24*time.Hour
}

func (s *State) MarkMentioned(name string) {
	s.mu.Lock()
	if cs, ok := s.Containers[name]; ok {
		now := time.Now()
		cs.LastMentionAt = &now
	}
	s.mu.Unlock()
	s.save()
}

func (s *State) loadLogs() {
	data, err := os.ReadFile(s.logPath)
	if err != nil {
		return
	}
	s.mu.Lock()
	json.Unmarshal(data, &s.Logs)
	s.mu.Unlock()
}

func (s *State) saveLogs() error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s.Logs, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.logPath)
	os.MkdirAll(dir, 0755)
	return os.WriteFile(s.logPath, data, 0600)
}

func (s *State) AddLogEntry(entry LogEntry) {
	s.mu.Lock()
	s.Logs = append(s.Logs, entry)
	if len(s.Logs) > 5000 {
		s.Logs = s.Logs[len(s.Logs)-5000:]
	}
	s.mu.Unlock()

	s.logMu.Lock()
	s.logDirty = true
	s.logMu.Unlock()
}

func (s *State) FlushLogs() error {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if !s.logDirty {
		return nil
	}
	s.logDirty = false
	return s.saveLogs()
}

func (s *State) GetLogs(container, sessionID string, since *time.Time) []LogEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []LogEntry
	for _, e := range s.Logs {
		if container != "" && e.Container != container {
			continue
		}
		if sessionID != "" && e.SessionID != sessionID {
			continue
		}
		if since != nil && e.Timestamp.Before(*since) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (s *State) CleanOldLogs(maxAge time.Duration) int {
	cutoff := time.Now().Add(-maxAge)
	s.mu.Lock()
	n := 0
	for i := 0; i < len(s.Logs); i++ {
		if s.Logs[i].Timestamp.Before(cutoff) {
			n++
		} else {
			break
		}
	}
	if n > 0 {
		s.Logs = s.Logs[n:]
	}
	s.mu.Unlock()
	return n
}

func (s *State) StartSession(container string) string {
	b := make([]byte, 8)
	rand.Read(b)
	id := hex.EncodeToString(b)
	s.sessionMu.Lock()
	s.sessions[container] = id
	s.sessionMu.Unlock()
	return id
}

func (s *State) GetSessionForLog(container string) string {
	s.sessionMu.RLock()
	defer s.sessionMu.RUnlock()
	return s.sessions[container]
}

func (s *State) EndSession(container string) {
	s.sessionMu.Lock()
	delete(s.sessions, container)
	s.sessionMu.Unlock()
}
