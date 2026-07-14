package webui

import (
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
	UpdateMode  UpdateMode `json:"update_mode"`
	DeferUntil  *time.Time `json:"defer_until,omitempty"`
	ChangelogURL string    `json:"changelog_url,omitempty"`
	LastUpdated  *time.Time `json:"last_updated,omitempty"`
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
}

type State struct {
	Containers map[string]*ContainerState `json:"containers"`
	Settings   Settings                   `json:"settings"`
	History    []HistoryEntry             `json:"history"`
	mu         sync.RWMutex
	filePath   string
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
		filePath: filepath.Join(dataDir, "state.json"),
	}
	s.load()
	s.loadFromEnv()
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
