package webui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/robfig/cron/v3"

	"github.com/dockyard/dockyard/pkg/container"
	"github.com/dockyard/dockyard/pkg/types"
)

//go:embed templates static
var embeddedFS embed.FS

const maxRequestBodySize = 1 << 20 // 1 MB

var validContainerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.\-/]{0,127}$`)

type Server struct {
	state           *State
	events          *EventHub
	auth            *AuthStore
	client          container.Client
	filter          types.Filter
	addr            string
	tmpl            *template.Template
	server          *http.Server
	version         string
	selfContainerID string
	lastAutoCheck   time.Time
	nextAutoCheck   time.Time
	autoCheckMu     sync.RWMutex
	lastPurge       time.Time
	updating        map[string]bool
	updatingMu      sync.Mutex
	checkMu         sync.Mutex // prevents concurrent check operations
	scheduleChanged  chan struct{} // signals the cron goroutine to re-evaluate
}

type ContainerInfo struct {
	Name             string            `json:"name"`
	Image            string            `json:"image"`
	Status           string            `json:"status"`
	State            string            `json:"state"`
	Stale            bool              `json:"stale"`
	Ports            string            `json:"ports"`
	ComposeStack     string            `json:"compose_stack"`
	UpdateMode       string            `json:"update_mode"`
	IsDeferred       bool              `json:"is_deferred"`
	DeferUntil       string            `json:"defer_until,omitempty"`
	ChangelogURL     string            `json:"changelog_url,omitempty"`
	Labels           map[string]string `json:"labels"`
	Created          string            `json:"created"`
	ImageID          string              `json:"image_id"`
	HasPreviousImage bool                `json:"has_previous_image"`
	PreviousImages   []PreviousImageEntry `json:"previous_images,omitempty"`
	IsSelf           bool                `json:"is_self"`
	CheckError       string            `json:"check_error,omitempty"`
	IsDatabase       bool              `json:"is_database"`
	IsSidecar        bool              `json:"is_sidecar"`
	RoleOverride     string            `json:"role_override,omitempty"`
	CheckedAt        string            `json:"checked_at,omitempty"`
	LatestImage      string            `json:"latest_image,omitempty"`
	CurrentVersion   string            `json:"current_version,omitempty"`
	LatestVersion    string            `json:"latest_version,omitempty"`
}

var dbImagePatterns = regexp.MustCompile(`(?i)^(mysql|mariadb|postgres(?:ql)?|mongo(?:db)?|redis|memcached|influxdb|timescaledb|cockroach(?:db)?|cassandra|elasticsearch|opensearch|clickhouse|neo4j|couchdb|valkey|keydb|scylladb|mssql|percona|tidb|planetscale|dragonflydb|ferretdb)`)

func isDatabaseImage(image string) bool {
	repo := image
	if idx := strings.Index(image, ":"); idx != -1 {
		repo = image[:idx]
	}
	if idx := strings.LastIndex(repo, "/"); idx != -1 {
		repo = repo[idx+1:]
	}
	return dbImagePatterns.MatchString(repo)
}

var versionFromTagRe = regexp.MustCompile(`^(?:v)?(\d+\.\d+(?:\.\d+)?)`)

func parseImageVersion(imageName string) string {
	tag := imageName
	if idx := strings.LastIndex(imageName, ":"); idx != -1 {
		tag = imageName[idx+1:]
	}
	if idx := strings.Index(tag, "@"); idx != -1 {
		tag = tag[:idx]
	}
	tag = strings.TrimPrefix(tag, "v")
	m := versionFromTagRe.FindStringSubmatch(tag)
	if m != nil {
		return m[1]
	}
	if tag != "latest" && tag != "stable" && tag != "edge" && tag != "alpine" && tag != "slim" {
		if len(tag) > 0 && len(tag) < 30 && strings.ContainsAny(tag, "0123456789") {
			return tag
		}
	}
	return ""
}

var imageVersionLabels = []string{
	"org.opencontainers.image.version",
	"org.label-schema.version",
}

func versionFromLabels(labels map[string]string) string {
	if labels == nil {
		return ""
	}
	for _, key := range imageVersionLabels {
		if v, ok := labels[key]; ok && v != "" {
			return v
		}
	}
	return ""
}

// inferChangelogURL tries to derive a release/changelog URL for an image by
// checking OCI labels, well-known image mappings, and registry URL patterns.
func inferChangelogURL(imageName string, labels map[string]string) string {
	// 1. Explicit changelog label (Watchtower convention).
	if labels != nil {
		if u, ok := labels["com.centurylinklabs.watchtower.changelog-url"]; ok && u != "" {
			return u
		}
	}
	// 2. OCI source label → GitHub/GitLab releases.
	if labels != nil {
		for _, key := range []string{"org.opencontainers.image.source", "org.label-schema.vcs-url"} {
			if src, ok := labels[key]; ok && src != "" {
				if u := sourceToReleasesURL(src); u != "" {
					return u
				}
			}
		}
	}
	// 3. Derive from image name.
	repo := imageName
	if idx := strings.Index(repo, ":"); idx != -1 {
		repo = repo[:idx]
	}

	// Strip well-known registry prefixes to normalize Docker Hub references.
	// e.g. "docker.io/prom/prometheus" → "prom/prometheus"
	//      "docker.io/library/redis" → "redis"
	if strings.HasPrefix(repo, "docker.io/") {
		repo = strings.TrimPrefix(repo, "docker.io/")
	}
	if strings.HasPrefix(repo, "library/") {
		repo = strings.TrimPrefix(repo, "library/")
	}

	// GHCR: always GitHub releases.
	if strings.HasPrefix(repo, "ghcr.io/") {
		parts := strings.SplitN(strings.TrimPrefix(repo, "ghcr.io/"), "/", 2)
		if len(parts) == 2 {
			return "https://github.com/" + parts[0] + "/" + parts[1] + "/releases"
		}
		return ""
	}

	// Check well-known image mappings for GitHub releases.
	if u := wellKnownGitHubRelease(repo); u != "" {
		return u
	}

	// Docker Hub: official library images (no org) — link to tags page.
	if !strings.Contains(repo, "/") {
		return "https://hub.docker.com/_/" + repo + "/tags"
	}

	// Docker Hub: org/repo images — guess GitHub releases from the image name.
	// Most open-source Docker Hub images have a matching GitHub repo at
	// github.com/{org}/{repo}. This is a best-effort guess; the user gets a
	// 404 if the repo doesn't exist, which is still more useful than a
	// Docker Hub tags page for release notes.
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) == 2 {
		return "https://github.com/" + parts[0] + "/" + parts[1] + "/releases"
	}
	return ""
}

// wellKnownGitHubRelease maps common Docker Hub image prefixes to their
// GitHub releases page. This covers the most popular self-hosted images.
func wellKnownGitHubRelease(repo string) string {
	// repo is normalized: no tag, no "library/" prefix.
	known := map[string]string{
		"grafana/grafana":              "https://github.com/grafana/grafana/releases",
		"prom/prometheus":              "https://github.com/prometheus/prometheus/releases",
		"gcr.io/cadvisor/cadvisor":     "https://github.com/google/cadvisor/releases",
		"portainer/portainer-ce":       "https://github.com/portainer/portainer/releases",
		"linuxserver/bookstack":        "https://github.com/BookStackApp/BookStack/releases",
		"linuxserver/paperless-ngx":    "https://github.com/paperless-ngx/paperless-ngx/releases",
		"ghcr.io/paperless-ngx/paperless-ngx": "https://github.com/paperless-ngx/paperless-ngx/releases",
		"jc21/nginx-proxy-manager":     "https://github.com/NginxProxyManager/nginx-proxy-manager/releases",
		"infisical/infisical":          "https://github.com/Infisical/infisical/releases",
		"advplyr/audiobookshelf":       "https://github.com/advplyr/audiobookshelf/releases",
		"louislam/uptime-kuma":         "https://github.com/louislam/uptime-kuma/releases",
		"vaultwarden/server":           "https://github.com/dani-garcia/vaultwarden/releases",
		"homeassistant/home-assistant": "https://github.com/home-assistant/core/releases",
		"jellyfin/jellyfin":            "https://github.com/jellyfin/jellyfin/releases",
		"linuxserver/jellyfin":         "https://github.com/jellyfin/jellyfin/releases",
		"containrrr/watchtower":        "https://github.com/containrrr/watchtower/releases",
		"linuxserver/transmission":     "https://github.com/transmission/transmission/releases",
		"linuxserver/qbittorrent":      "https://github.com/qbittorrent/qBittorrent/releases",
		"nextcloud":                    "https://github.com/nextcloud/server/releases",
		"immich-app/immich-server":     "https://github.com/immich-app/immich/releases",
		"fireflyiii/core":              "https://github.com/firefly-iii/firefly-iii/releases",
		"mealie-recipes/mealie":        "https://github.com/mealie-recipes/mealie/releases",
		"ghcr.io/mealie-recipes/mealie": "https://github.com/mealie-recipes/mealie/releases",
		"crowdsecurity/crowdsec":       "https://github.com/crowdsecurity/crowdsec/releases",
	}
	if u, ok := known[repo]; ok {
		return u
	}
	// For linuxserver/* images without a specific mapping, link to their index.
	if strings.HasPrefix(repo, "linuxserver/") {
		return "https://github.com/linuxserver/docker-" + strings.TrimPrefix(repo, "linuxserver/") + "/releases"
	}
	return ""
}

func sourceToReleasesURL(src string) string {
	src = strings.TrimSuffix(src, ".git")
	if strings.Contains(src, "github.com") {
		parts := strings.Split(strings.TrimPrefix(src, "https://"), "/")
		if len(parts) >= 3 {
			return "https://github.com/" + parts[1] + "/" + parts[2] + "/releases"
		}
	}
	if strings.Contains(src, "gitlab.com") {
		parts := strings.Split(strings.TrimPrefix(src, "https://"), "/")
		if len(parts) >= 3 {
			return "https://gitlab.com/" + parts[1] + "/" + parts[2] + "/-/releases"
		}
	}
	return ""
}

func NewServer(state *State, events *EventHub, auth *AuthStore, client container.Client, filter types.Filter, addr, version string) *Server {
	logrus.WithFields(logrus.Fields{
		"version": version,
		"addr":    addr,
	}).Info("Creating web UI server")
	s := &Server{
		state:           state,
		events:          events,
		auth:            auth,
		client:          client,
		filter:          filter,
		addr:            addr,
		version:         version,
		updating:        make(map[string]bool),
		scheduleChanged: make(chan struct{}, 1),
	}
	s.detectSelfContainer()
	s.loadTemplates()
	return s
}

// detectSelfContainer finds our own container ID so we can handle self-updates specially.
func (s *Server) detectSelfContainer() {
	ctx := context.Background()
	id, err := container.GetCurrentContainerID(ctx, s.client)
	if err != nil {
		logrus.WithError(err).Debug("Could not detect own container ID — self-update via pull+restart unavailable")
		return
	}
	s.selfContainerID = string(id)
	logrus.WithField("container_id", string(id)[:12]).Info("Detected own container for self-update")
}

func (s *Server) loadTemplates() {
	tmplFS, err := fs.Sub(embeddedFS, "templates")
	if err != nil {
		logrus.WithError(err).Fatal("Failed to get templates sub-filesystem")
	}

	funcMap := template.FuncMap{
		"upper": strings.ToUpper,
		"lower": strings.ToLower,
		"truncate": func(str string, n int) string {
			if len(str) > n {
				return str[:n] + "..."
			}
			return str
		},
		"jsEscape": func(s string) string {
			s = strings.ReplaceAll(s, `\`, `\\`)
			s = strings.ReplaceAll(s, `'`, `\'`)
			s = strings.ReplaceAll(s, `"`, `\"`)
			s = strings.ReplaceAll(s, "\n", `\n`)
			s = strings.ReplaceAll(s, "\r", `\r`)
			s = strings.ReplaceAll(s, "<", `\x3c`)
			s = strings.ReplaceAll(s, ">", `\x3e`)
			return s
		},
	}

	s.tmpl = template.Must(template.New("").Funcs(funcMap).ParseFS(tmplFS, "*.html"))
	logrus.Debug("Templates loaded successfully")
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' 'unsafe-inline' https://cdn.tailwindcss.com https://unpkg.com; "+
				"style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; "+
				"connect-src 'self'; "+
				"font-src 'self'; "+
				"frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func limitRequestBody(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
		next(w, r)
	}
}

func sanitizeErrorParam(msg string) string {
	safe := []string{"signin", "invalid", "mismatch", "exists", "failed", "password", "characters"}
	lower := strings.ToLower(msg)
	for _, s := range safe {
		if strings.Contains(lower, s) {
			return msg
		}
	}
	return "authentication failed"
}

func sanitizeContainerName(name string) (string, error) {
	name = strings.TrimPrefix(name, "/")
	if name == "" {
		return "", errors.New("empty container name")
	}
	if !validContainerName.MatchString(name) {
		return "", errors.New("invalid container name")
	}
	return name, nil
}

func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	logrus.WithField("addr", s.addr).Info("Initializing web UI routes")

	staticFS, _ := fs.Sub(embeddedFS, "static")
	mux.Handle("/static/", securityHeaders(http.StripPrefix("/static/", http.FileServer(http.FS(staticFS)))))
	logrus.Debug("Registered static file server")

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	logrus.Debug("Registered /health endpoint")

	mux.HandleFunc("/login", s.handleLoginPage)
	logrus.Debug("Registered /login route")
	mux.HandleFunc("/auth/login", limitRequestBody(s.handleLogin))
	mux.HandleFunc("/auth/register", limitRequestBody(s.handleRegister))
	mux.HandleFunc("/auth/logout", s.handleLogout)
	logrus.Debug("Registered auth routes")

	protected := http.NewServeMux()
	protected.HandleFunc("/", s.handleDashboard)
	protected.HandleFunc("/settings", s.handleSettings)
	protected.HandleFunc("/history", s.handleHistory)
	protected.HandleFunc("/logs", s.handleLogsPage)

	protected.HandleFunc("/api/containers", s.handleAPIContainers)
	protected.HandleFunc("/api/containers/", s.handleAPIContainerAction)
	protected.HandleFunc("/api/stacks/", s.handleAPIStackAction)
	protected.HandleFunc("/api/settings", limitRequestBody(s.handleAPISettings))
	protected.HandleFunc("/api/history", s.handleAPIHistory)
	protected.HandleFunc("/api/check", s.handleAPICheckNow)
	protected.HandleFunc("/api/events", s.handleSSE)
	protected.HandleFunc("/api/update/check", s.handleAPICheckUpdate)
	protected.HandleFunc("/api/update/self", s.handleAPISelfUpdate)
	protected.HandleFunc("/api/notifications/test", s.handleAPITestNotification)
	protected.HandleFunc("/api/logs", s.handleAPILogs)
	protected.HandleFunc("/api/auto-check-status", s.handleAPICheckStatus)
	protected.HandleFunc("/api/user/change-password", limitRequestBody(s.handleAPIChangePassword))
	protected.HandleFunc("/api/debug/containers", s.handleAPIDebugContainers)
	logrus.Debug("Registered protected page and API routes")

	mux.Handle("/", securityHeaders(s.auth.AuthMiddleware(protected.ServeHTTP)))
	logrus.Debug("All routes registered")

	s.server = &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		s.state.FlushLogs()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.server.Shutdown(shutdownCtx)
	}()

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.state.FlushLogs()
			}
		}
	}()

	// Server-side auto-check using the cron schedule.
	go func() {
		parser := cron.NewParser(
			cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
		)

		for {
			settings := s.state.GetSettings()
			schedule := settings.Schedule
			timezone := settings.Timezone
			if timezone == "" {
				timezone = "Local"
			}
			if schedule == "" || schedule == "@yearly" {
				s.autoCheckMu.Lock()
				s.nextAutoCheck = time.Time{}
				s.autoCheckMu.Unlock()

				select {
				case <-ctx.Done():
					return
				case <-s.scheduleChanged:
				case <-time.After(60 * time.Second):
				}
				continue
			}

			// Use the configured timezone from Settings.
			parseSchedule := schedule
			if !strings.HasPrefix(schedule, "CRON_TZ=") {
				parseSchedule = "CRON_TZ=" + timezone + " " + schedule
			}
			sched, err := parser.Parse(parseSchedule)
			if err != nil {
				logrus.WithError(err).WithField("schedule", schedule).Warn("Invalid cron schedule for auto-check")
				select {
				case <-ctx.Done():
					return
				case <-s.scheduleChanged:
				case <-time.After(60 * time.Second):
				}
				continue
			}

			next := sched.Next(time.Now())
			s.autoCheckMu.Lock()
			s.nextAutoCheck = next
			s.autoCheckMu.Unlock()

			logrus.WithFields(logrus.Fields{
				"schedule": schedule,
				"timezone": timezone,
				"next":     next.Format(time.RFC3339),
			}).Info("Auto-check scheduled")

			// Sleep until next check, but re-evaluate if settings change.
			for time.Until(next) > 0 {
				select {
				case <-ctx.Done():
					return
				case <-s.scheduleChanged:
					logrus.Info("Auto-check schedule change signaled, recalculating")
					break
				case <-time.After(30 * time.Second):
				}
				// Re-read schedule each tick in case user changed it.
				newSettings := s.state.GetSettings()
				if newSettings.Schedule != schedule || newSettings.Timezone != timezone {
					logrus.WithFields(logrus.Fields{"new_schedule": newSettings.Schedule, "new_tz": newSettings.Timezone}).Info("Auto-check schedule changed, recalculating")
					break
				}
			}

			s.autoCheckMu.Lock()
			s.lastAutoCheck = time.Now()
			s.autoCheckMu.Unlock()
			s.runAutoCheck(ctx)
		}
	}()

	logrus.WithField("addr", s.addr).Info("Dockyard web UI starting")

	// Background goroutine: purge expired old images based on retention setting.
	go func() {
		s.autoCheckMu.Lock()
		s.lastPurge = time.Now()
		s.autoCheckMu.Unlock()
		time.Sleep(30 * time.Second)
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.purgeExpiredImages()
				s.autoCheckMu.Lock()
				s.lastPurge = time.Now()
				s.autoCheckMu.Unlock()
			}
		}
	}()

	// Auto-check on startup after a short delay (gives containers time to register).
	go func() {
		time.Sleep(10 * time.Second)
		s.autoCheckMu.Lock()
		s.lastAutoCheck = time.Now()
		s.autoCheckMu.Unlock()
		s.runAutoCheck(context.Background())
	}()

	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logrus.WithError(err).WithField("addr", s.addr).Error("HTTP server failed")
		return err
	}
	logrus.WithField("addr", s.addr).Info("HTTP server shut down gracefully")
	return nil
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/login" {
		http.NotFound(w, r)
		return
	}

	errorMsg := r.URL.Query().Get("error")
	if errorMsg != "" {
		errorMsg = sanitizeErrorParam(errorMsg)
	}

	s.renderTemplate(w, "login.html", map[string]interface{}{
		"IsSetup": !s.auth.HasUsers(),
		"Error":   errorMsg,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	token, err := s.auth.Login(username, password)
	if err != nil {
		http.Redirect(w, r, "/login?error="+sanitizeErrorParam(err.Error()), http.StatusFound)
		return
	}

	s.auth.SetSessionCookie(w, token)
	s.events.Broadcast(Event{
		Type:    EventLogLine,
		Message: "User signed in",
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	confirmPassword := r.FormValue("confirm_password")

	if password != confirmPassword {
		http.Redirect(w, r, "/login?error=passwords+do+not+match", http.StatusFound)
		return
	}

	if len(password) < 8 {
		http.Redirect(w, r, "/login?error=password+must+be+at+least+8+characters", http.StatusFound)
		return
	}

	if err := s.auth.Register(username, password); err != nil {
		http.Redirect(w, r, "/login?error="+sanitizeErrorParam(err.Error()), http.StatusFound)
		return
	}

	token, err := s.auth.Login(username, password)
	if err != nil {
		http.Redirect(w, r, "/login?error=account+created+but+login+failed", http.StatusFound)
		return
	}

	s.auth.SetSessionCookie(w, token)
	s.events.Broadcast(Event{
		Type:    EventLogLine,
		Message: "Admin account created",
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("dockyard_session")
	if err == nil {
		s.auth.Logout(cookie.Value)
	}
	s.auth.ClearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *Server) handleAPIChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, "method not allowed", 405)
		return
	}

	username := r.Header.Get("X-Auth-User")
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, "invalid request body", 400)
		return
	}

	if len(req.NewPassword) < 8 {
		s.writeError(w, "password must be at least 8 characters", 400)
		return
	}

	if err := s.auth.ChangePassword(username, req.OldPassword, req.NewPassword); err != nil {
		s.writeError(w, "incorrect current password", 401)
		return
	}

	s.writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleAPICheckUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, "method not allowed", 405)
		return
	}

	force := r.URL.Query().Get("force") == "true"
	info, err := CheckForUpdateForce(s.version, force)
	if err != nil {
		s.writeError(w, err.Error(), 500)
		return
	}

	s.writeJSON(w, info)
}

func (s *Server) handleAPISelfUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, "method not allowed", 405)
		return
	}

	if s.selfContainerID == "" {
		s.writeError(w, "self container not detected — cannot self-update", 400)
		return
	}

	// Find our own container name by ID, then route through the normal
	// update flow which handles self-updates correctly (rename → start → remove).
	ctx := context.Background()
	dockerContainers, err := s.client.ListContainers(ctx)
	if err != nil {
		s.writeError(w, "failed to list containers: "+err.Error(), 500)
		return
	}

	var selfName string
	for _, c := range dockerContainers {
		if string(c.ID()) == s.selfContainerID {
			selfName = c.Name()
			break
		}
	}

	if selfName == "" {
		s.writeError(w, "self container not found in Docker", 404)
		return
	}

	go s.performContainerUpdate(selfName)

	s.writeJSON(w, map[string]string{"status": "ok", "message": "update started", "container": selfName})
}

func (s *Server) getContainerList() []ContainerInfo {
	ctx := context.Background()
	containers, err := s.client.ListContainers(ctx)
	if err != nil {
		logrus.WithError(err).Error("Failed to list containers")
		return nil
	}
	names := make([]string, len(containers))
	for i, c := range containers {
		names[i] = c.Name()
	}
	logrus.WithFields(logrus.Fields{
		"count":   len(containers),
		"names":   names,
		"self_id": s.selfContainerID,
	}).Info("Docker container list")
	return s.buildContainerList(containers)
}

// buildContainerList converts Docker container objects to ContainerInfo structs.
// This is used by both getContainerList and the check handlers to avoid
// calling ListContainers() twice (which can cause name mismatches if a
// container is recreated between calls).
func (s *Server) buildContainerList(containers []types.Container) []ContainerInfo {
	result := make([]ContainerInfo, 0, len(containers))
	for _, c := range containers {
		// Per-container panic recovery so one broken container can't crash the list.
		func() {
			defer func() {
				if r := recover(); r != nil {
					logrus.WithFields(logrus.Fields{
						"container": c.Name(),
						"panic":     r,
					}).Error("Panic processing container in buildContainerList")
				}
			}()

			name := c.Name()
			cs := s.state.GetContainerState(name)

			// Auto-populate ChangelogURL if not manually set.
			changelogURL := cs.ChangelogURL
			if changelogURL == "" {
				var labels map[string]string
				if ci := c.ContainerInfo(); ci != nil && ci.Config != nil {
					labels = ci.Config.Labels
				}
				changelogURL = inferChangelogURL(c.ImageName(), labels)
			}

			ci := ContainerInfo{
				Name:           name,
				Image:          c.ImageName(),
				Stale:          c.IsStale() || cs.IsStale,
				UpdateMode:     string(cs.UpdateMode),
				IsDeferred:     s.state.IsDeferred(name),
				ChangelogURL:   changelogURL,
				ImageID:        string(c.ImageID()),
				IsSelf:         s.selfContainerID != "" && string(c.ID()) == s.selfContainerID,
				IsDatabase:     isDatabaseImage(c.ImageName()),
				IsSidecar:      false, // determined below
				RoleOverride:   cs.RoleOverride,
				CheckError:     cs.CheckError,
				LatestImage:    cs.LatestImage,
				CurrentVersion: parseImageVersion(c.ImageName()),
				LatestVersion:  cs.LatestVersion,
			}

			if ci.CurrentVersion == "" {
				if ci2 := c.ContainerInfo(); ci2 != nil && ci2.Config != nil {
					ci.CurrentVersion = versionFromLabels(ci2.Config.Labels)
				}
			}

			if cs.CheckedAt != nil {
				ci.CheckedAt = cs.CheckedAt.Format(time.RFC3339)
			}

			if len(cs.PreviousImages) > 0 {
				ci.HasPreviousImage = true
				ci.PreviousImages = cs.PreviousImages
			}

			if cs.DeferUntil != nil {
				ci.DeferUntil = cs.DeferUntil.Format("2006-01-02 15:04")
			}

			if c.IsRunning() {
				ci.State = "running"
			} else if info := c.ContainerInfo(); info != nil && info.State != nil && info.State.Restarting {
				ci.State = "restarting"
			} else {
				ci.State = "stopped"
			}

			ci.Labels = make(map[string]string)
			if inspect := c.ContainerInfo(); inspect != nil {
				if inspect.Config != nil && inspect.Config.Labels != nil {
					ci.Labels = inspect.Config.Labels
					ci.ComposeStack = inspect.Config.Labels["com.docker.compose.project"]
				}

				if inspect.HostConfig != nil {
					for port := range inspect.HostConfig.PortBindings {
						ci.Ports += port.Port() + "/" + string(port.Proto()) + " "
					}
				}
			}

			ci.Ports = strings.TrimSpace(ci.Ports)
			result = append(result, ci)
		}()
	}

	// --- Compose-based sidecar detection ---
	// Group by compose project. Containers without published ports in
	// multi-container projects are sidecars (workers, migrators, etc.).
	// If the project has databases and all non-DB containers are portless,
	// they're all sidecars (app behind reverse proxy pattern).
	type projectInfo struct {
		hasPorts   bool
		hasDB      bool
		containers []int // indices into result (non-self only)
		dbs        []int // indices of detected DB containers in same project
	}
	projects := make(map[string]*projectInfo)
	for i := range result {
		if result[i].IsSelf {
			continue
		}
		if result[i].ComposeStack == "" {
			continue
		}
		proj := projects[result[i].ComposeStack]
		if proj == nil {
			proj = &projectInfo{}
			projects[result[i].ComposeStack] = proj
		}
		if result[i].IsDatabase {
			proj.hasDB = true
			proj.dbs = append(proj.dbs, i)
		} else {
			proj.containers = append(proj.containers, i)
			if result[i].Ports != "" {
				proj.hasPorts = true
			}
		}
	}
	for _, proj := range projects {
		if len(proj.containers) == 0 {
			continue
		}
		if proj.hasPorts {
			// At least one container has ports — portless ones are sidecars.
			for _, idx := range proj.containers {
				if result[idx].Ports == "" {
					result[idx].IsSidecar = true
				}
			}
		} else if proj.hasDB && len(proj.containers) > 1 {
			// DB project with no port bindings (app behind reverse proxy) —
			// all non-DB containers are sidecars (workers, migrators, etc.).
			for _, idx := range proj.containers {
				result[idx].IsSidecar = true
			}
		}
	}

	// --- Apply user overrides ---
	for i := range result {
		cs := s.state.GetContainerState(result[i].Name)
		switch cs.RoleOverride {
		case "sidecar":
			result[i].IsSidecar = true
			result[i].IsDatabase = false
		case "database":
			result[i].IsDatabase = true
			result[i].IsSidecar = false
		case "main":
			result[i].IsSidecar = false
			result[i].IsDatabase = false
		}
	}

	return result
}

func (s *Server) writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func (s *Server) writeError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
