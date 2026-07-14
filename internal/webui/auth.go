package webui

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrUserExists         = errors.New("username already exists")
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrPasswordsNoMatch   = errors.New("passwords do not match")
)

type User struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	CreatedAt    string `json:"created_at"`
}

type Session struct {
	Token     string    `json:"token"`
	Username  string    `json:"username"`
	ExpiresAt time.Time `json:"expires_at"`
}

type AuthStore struct {
	users    map[string]*User
	sessions map[string]*Session
	mu       sync.RWMutex
	filePath string
	isHTTPS  bool
}

func NewAuthStore(dataDir string) *AuthStore {
	logrus.WithField("dataDir", dataDir).Info("Initializing auth store")
	a := &AuthStore{
		users:    make(map[string]*User),
		sessions: make(map[string]*Session),
		filePath: filepath.Join(dataDir, "auth.json"),
	}
	a.load()
	a.autoProvision()
	logrus.WithField("users", len(a.users)).Info("Auth store initialized")
	return a
}

func (a *AuthStore) autoProvision() {
	username := os.Getenv("DOCKYARD_ADMIN_USER")
	password := os.Getenv("DOCKYARD_ADMIN_PASSWORD")
	if username == "" || password == "" {
		logrus.Debug("No DOCKYARD_ADMIN_USER/DOCKYARD_ADMIN_PASSWORD set, skipping auto-provision")
		return
	}
	username = strings.TrimSpace(username)
	if len(password) < 8 {
		logrus.Warn("DOCKYARD_ADMIN_PASSWORD must be at least 8 characters, skipping auto-provision")
		return
	}

	a.mu.Lock()

	if existing, exists := a.users[username]; exists {
		if err := bcrypt.CompareHashAndPassword([]byte(existing.PasswordHash), []byte(password)); err != nil {
			hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
			if err != nil {
				a.mu.Unlock()
				logrus.WithError(err).Error("Failed to hash admin password")
				return
			}
			existing.PasswordHash = string(hash)
			a.sessions = make(map[string]*Session)
			a.mu.Unlock()
			a.save()
			logrus.WithField("user", username).Info("Admin password updated from environment")
		} else {
			a.mu.Unlock()
		}
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		a.mu.Unlock()
		logrus.WithError(err).Error("Failed to hash admin password")
		return
	}
	a.users[username] = &User{
		Username:     username,
		PasswordHash: string(hash),
		CreatedAt:    time.Now().Format(time.RFC3339),
	}
	a.mu.Unlock()
	a.save()
	logrus.WithField("user", username).Info("Admin account created from environment")
}

func (a *AuthStore) SetHTTPS(enabled bool) {
	a.isHTTPS = enabled
}

func (a *AuthStore) load() {
	data, err := os.ReadFile(a.filePath)
	if err != nil {
		logrus.WithField("filePath", a.filePath).Debug("No existing auth file found, starting fresh")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	json.Unmarshal(data, a)
	logrus.WithField("users", len(a.users)).Debug("Loaded auth data from disk")
}

func (a *AuthStore) save() error {
	a.mu.RLock()
	data, err := json.MarshalIndent(a, "", "  ")
	a.mu.RUnlock()
	if err != nil {
		return err
	}
	dir := filepath.Dir(a.filePath)
	os.MkdirAll(dir, 0755)
	return os.WriteFile(a.filePath, data, 0600)
}

func (a *AuthStore) HasUsers() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.users) > 0
}

func (a *AuthStore) Register(username, password string) error {
	username = strings.TrimSpace(username)
	if len(username) < 1 || len(username) > 64 {
		return errors.New("username must be 1-64 characters")
	}
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	a.mu.Lock()
	if _, exists := a.users[username]; exists {
		a.mu.Unlock()
		return ErrUserExists
	}

	a.users[username] = &User{
		Username:     username,
		PasswordHash: string(hash),
		CreatedAt:    time.Now().Format(time.RFC3339),
	}
	a.mu.Unlock()

	return a.save()
}

func (a *AuthStore) Login(username, password string) (string, error) {
	a.mu.RLock()
	user, exists := a.users[username]
	a.mu.RUnlock()

	if !exists {
		return "", ErrInvalidCredentials
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return "", ErrInvalidCredentials
	}

	token := generateToken()
	a.mu.Lock()
	a.sessions[token] = &Session{
		Token:     token,
		Username:  username,
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour),
	}
	a.mu.Unlock()

	a.save()
	return token, nil
}

func (a *AuthStore) ValidateSession(token string) (*Session, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	session, exists := a.sessions[token]
	if !exists {
		return nil, false
	}

	if time.Now().After(session.ExpiresAt) {
		delete(a.sessions, token)
		return nil, false
	}

	return session, true
}

func (a *AuthStore) Logout(token string) {
	a.mu.Lock()
	delete(a.sessions, token)
	a.mu.Unlock()
	a.save()
}

func (a *AuthStore) InvalidateAllSessions() {
	a.mu.Lock()
	a.sessions = make(map[string]*Session)
	a.mu.Unlock()
}

func (a *AuthStore) ChangePassword(username, oldPass, newPass string) error {
	if len(newPass) < 8 {
		return errors.New("password must be at least 8 characters")
	}

	a.mu.RLock()
	user, exists := a.users[username]
	a.mu.RUnlock()

	if !exists {
		return ErrInvalidCredentials
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(oldPass)); err != nil {
		return ErrInvalidCredentials
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	a.mu.Lock()
	user.PasswordHash = string(hash)
	a.sessions = make(map[string]*Session)
	a.mu.Unlock()

	return a.save()
}

func (a *AuthStore) DeleteUser(username string) error {
	a.mu.Lock()
	delete(a.users, username)
	a.sessions = make(map[string]*Session)
	a.mu.Unlock()
	return a.save()
}

func generateToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *AuthStore) AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.HasUsers() {
			next(w, r)
			return
		}

		cookie, err := r.Cookie("dockyard_session")
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}

		session, valid := a.ValidateSession(cookie.Value)
		if !valid {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}

		r.Header.Set("X-Auth-User", session.Username)
		next(w, r)
	}
}

func (a *AuthStore) SetSessionCookie(w http.ResponseWriter, token string) {
	cookie := &http.Cookie{
		Name:     "dockyard_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   30 * 24 * 60 * 60,
		Secure:   a.isHTTPS,
	}
	http.SetCookie(w, cookie)
}

func (a *AuthStore) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "dockyard_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}
