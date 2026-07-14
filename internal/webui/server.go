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
	"time"

	"github.com/sirupsen/logrus"

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
	ImageID          string            `json:"image_id"`
	HasPreviousImage bool              `json:"has_previous_image"`
	PreviousImage    string            `json:"previous_image,omitempty"`
	IsSelf           bool              `json:"is_self"`
	CheckError       string            `json:"check_error,omitempty"`
	IsDatabase       bool              `json:"is_database"`
	IsSidecar        bool              `json:"is_sidecar"`
}

var dbImagePatterns = regexp.MustCompile(`(?i)^(mysql|mariadb|postgres(?:ql)?|mongo(?:db)?|redis|memcached|influxdb|timescaledb|cockroach(?:db)?|cassandra|elasticsearch|opensearch|clickhouse|neo4j|couchdb|valkey|keydb|scylladb|mssql|percona|tidb|planetscale|dragonflydb|ferretdb)`)

var sidecarImagePatterns = regexp.MustCompile(`(?i)(tika|gotenberg|pdfjs|libreoffice|chromium|wkhtmltopdf|collabora|onlyoffice|embedder|exiftool|ghostscript|imgproxy|pdfcpu|qpdf|poppler|pandoc)`)

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

func isSidecarImage(image string) bool {
	repo := image
	if idx := strings.Index(image, ":"); idx != -1 {
		repo = image[:idx]
	}
	if idx := strings.LastIndex(repo, "/"); idx != -1 {
		repo = repo[idx+1:]
	}
	return sidecarImagePatterns.MatchString(repo)
}

func NewServer(state *State, events *EventHub, auth *AuthStore, client container.Client, filter types.Filter, addr, version string) *Server {
	logrus.WithFields(logrus.Fields{
		"version": version,
		"addr":    addr,
	}).Info("Creating web UI server")
	s := &Server{
		state:   state,
		events:  events,
		auth:    auth,
		client:  client,
		filter:  filter,
		addr:    addr,
		version: version,
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
	protected.HandleFunc("/api/settings", limitRequestBody(s.handleAPISettings))
	protected.HandleFunc("/api/history", s.handleAPIHistory)
	protected.HandleFunc("/api/check", s.handleAPICheckNow)
	protected.HandleFunc("/api/events", s.handleSSE)
	protected.HandleFunc("/api/update/check", s.handleAPICheckUpdate)
	protected.HandleFunc("/api/update/self", s.handleAPISelfUpdate)
	protected.HandleFunc("/api/notifications/test", s.handleAPITestNotification)
	protected.HandleFunc("/api/logs", s.handleAPILogs)
	protected.HandleFunc("/api/user/change-password", limitRequestBody(s.handleAPIChangePassword))
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

	// Server-side auto-check ticker.
	go func() {
		var checkTicker *time.Ticker
		var checkCh <-chan time.Time
		for {
			// Read interval each loop so it picks up settings changes.
			interval := s.state.GetAutoCheckInterval()
			if interval > 0 {
				if checkTicker != nil {
					checkTicker.Stop()
				}
				checkTicker = time.NewTicker(interval)
				checkCh = checkTicker.C
				logrus.WithField("interval", interval).Info("Auto-check enabled")
			} else {
				if checkTicker != nil {
					checkTicker.Stop()
					checkTicker = nil
				}
				checkCh = nil
				logrus.Info("Auto-check disabled (set to never)")
			}

			// Block until either a check fires or context is done.
			// Use a shorter poll (5 min) to re-check settings in case the user changes the interval.
			poll := time.NewTicker(5 * time.Minute)
			defer poll.Stop()

			select {
			case <-ctx.Done():
				if checkTicker != nil {
					checkTicker.Stop()
				}
				return
			case <-checkCh:
				s.runAutoCheck(ctx)
			case <-poll.C:
				// Loop back to re-read the interval setting.
			}
		}
	}()

	logrus.WithField("addr", s.addr).Info("Dockyard web UI starting")
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

	info, err := CheckForUpdate(s.version)
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

	go func() {
		if err := PerformSelfUpdate(s.version, s.events); err != nil {
			logrus.WithError(err).Error("Self-update failed")
		}
	}()

	s.writeJSON(w, map[string]string{"status": "ok", "message": "update started"})
}

func (s *Server) getContainerList() []ContainerInfo {
	ctx := context.Background()
	containers, err := s.client.ListContainers(ctx)
	if err != nil {
		logrus.WithError(err).Error("Failed to list containers")
		return nil
	}

	result := make([]ContainerInfo, 0, len(containers))
	for _, c := range containers {
		name := c.Name()
		cs := s.state.GetContainerState(name)

		ci := ContainerInfo{
			Name:         name,
			Image:        c.ImageName(),
			Stale:        c.IsStale(),
			UpdateMode:   string(cs.UpdateMode),
			IsDeferred:   s.state.IsDeferred(name),
			ChangelogURL: cs.ChangelogURL,
			ImageID:      string(c.ImageID()),
			IsSelf:       s.selfContainerID != "" && string(c.ID()) == s.selfContainerID,
			IsDatabase:   isDatabaseImage(c.ImageName()),
			IsSidecar:    isSidecarImage(c.ImageName()),
		}

		if cs.PreviousImage != "" {
			ci.HasPreviousImage = true
			ci.PreviousImage = cs.PreviousImage
		}

		if cs.DeferUntil != nil {
			ci.DeferUntil = cs.DeferUntil.Format("2006-01-02 15:04")
		}

		if c.IsRunning() {
			ci.State = "running"
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
