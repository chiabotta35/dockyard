package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	GitHubOwner = "chiabotta35"
	GitHubRepo  = "dockyard"
)

type UpdateInfo struct {
	Available   bool   `json:"available"`
	CurrentVer  string `json:"current_version"`
	LatestVer   string `json:"latest_version"`
	ReleaseURL  string `json:"release_url"`
	PublishedAt string `json:"published_at"`
	Body        string `json:"body"`
}

type GitHubRelease struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	Body        string `json:"body"`
	PublishedAt string `json:"published_at"`
	HTMLURL     string `json:"html_url"`
	Assets      []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

type gitTagRef struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"object"`
}

var (
	lastUpdateCheck *UpdateInfo
	lastCheckTime   time.Time
	updateCheckMu   sync.Mutex
)

// normalizeVersion strips a leading "v" prefix and whitespace so that
// "v0.1.1" and "0.1.1" compare equal.
func normalizeVersion(v string) string {
	return strings.TrimSpace(strings.TrimPrefix(v, "v"))
}

// parseVersion parses "0.1.4" into [0, 1, 4] for comparison.
func parseVersion(v string) []int {
	v = normalizeVersion(v)
	parts := strings.Split(v, ".")
	var nums []int
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		nums = append(nums, n)
	}
	return nums
}

// versionLess returns true if a < b.
func versionLess(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] < b[i] {
			return true
		}
		if a[i] > b[i] {
			return false
		}
	}
	return len(a) < len(b)
}

func CheckForUpdate(currentVersion string) (*UpdateInfo, error) {
	updateCheckMu.Lock()
	defer updateCheckMu.Unlock()

	if time.Since(lastCheckTime) < 60*time.Second && lastUpdateCheck != nil {
		return lastUpdateCheck, nil
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/refs/tags", GitHubOwner, GitHubRepo)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to check GitHub tags: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var tags []gitTagRef
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, fmt.Errorf("failed to parse tags: %w", err)
	}

	// Collect all v* tags, parse their versions, and find the latest.
	curVer := parseVersion(currentVersion)
	var latestTag string
	var latestVer []int

	for _, t := range tags {
		tagName := strings.TrimPrefix(t.Ref, "refs/tags/")
		if !strings.HasPrefix(tagName, "v") {
			continue
		}
		ver := parseVersion(tagName)
		if ver == nil {
			continue
		}
		if latestVer == nil || versionLess(latestVer, ver) {
			latestTag = tagName
			latestVer = ver
		}
	}

	if latestTag == "" || latestVer == nil {
		// No versioned tags found.
		info := &UpdateInfo{
			Available:  false,
			CurrentVer: currentVersion,
			LatestVer:  currentVersion,
		}
		lastUpdateCheck = info
		lastCheckTime = time.Now()
		return info, nil
	}

	available := curVer == nil || versionLess(curVer, latestVer)
	info := &UpdateInfo{
		Available:  available,
		CurrentVer: currentVersion,
		LatestVer:  latestTag,
		ReleaseURL: fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s", GitHubOwner, GitHubRepo, latestTag),
	}

	lastUpdateCheck = info
	lastCheckTime = time.Now()
	return info, nil
}

// PerformSelfUpdate updates Dockyard. When running in Docker, it pulls the new
// image and restarts the container. When running as a bare binary, it replaces
// the executable on disk and tells the user to restart manually.
func PerformSelfUpdate(currentVersion string, events *EventHub) error {
	info, err := CheckForUpdate(currentVersion)
	if err != nil {
		return err
	}

	if !info.Available {
		return fmt.Errorf("already on latest version %s", currentVersion)
	}

	events.Broadcast(Event{
		Type:    EventUpdateStarted,
		Message: fmt.Sprintf("Updating Dockyard from %s to %s...", currentVersion, info.LatestVer),
	})

	if isRunningInDocker() {
		return performDockerSelfUpdate(info, events)
	}

	return performBinarySelfUpdate(info, events)
}

// isRunningInDocker detects whether the process is inside a Docker container.
func isRunningInDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	data, err := os.ReadFile("/proc/1/cgroup")
	if err == nil && strings.Contains(string(data), "docker") {
		return true
	}
	return false
}

// performDockerSelfUpdate pulls the new Docker image and restarts the container
// by stopping it — Docker's restart policy (unless-stopped/always) will bring
// it back up with the new image.
func performDockerSelfUpdate(info *UpdateInfo, events *EventHub) error {
	imageRef := fmt.Sprintf("ghcr.io/%s/%s:%s", GitHubOwner, GitHubRepo, info.LatestVer)
	events.BroadcastLog("", "Pulling new image: "+imageRef)

	events.BroadcastLog("", "Pulling image (this may take a moment)...")
	if err := exec.Command("docker", "pull", imageRef).Run(); err != nil {
		events.Broadcast(Event{Type: EventUpdateFailed, Message: "Failed to pull new Docker image: " + err.Error()})
		return fmt.Errorf("docker pull failed: %w", err)
	}

	events.BroadcastLog("", "Image pulled successfully")

	// Find our own container ID
	containerID, err := getSelfContainerID()
	if err != nil {
		events.Broadcast(Event{Type: EventUpdateFailed, Message: "Failed to identify container: " + err.Error()})
		return fmt.Errorf("failed to get container ID: %w", err)
	}

	events.BroadcastLog("", "Restarting container: "+containerID[:12])

	// Stop the container — Docker's restart policy will bring it back with the new image
	if err := exec.Command("docker", "restart", containerID).Run(); err != nil {
		events.Broadcast(Event{Type: EventUpdateFailed, Message: "Failed to restart container: " + err.Error()})
		return fmt.Errorf("docker restart failed: %w", err)
	}

	events.Broadcast(Event{
		Type:    EventUpdateComplete,
		Message: fmt.Sprintf("Updated to %s successfully! Container is restarting with the new image.", info.LatestVer),
	})

	return nil
}

// getSelfContainerID reads the container ID from the cgroup or hostname.
func getSelfContainerID() (string, error) {
	// Try /proc/self/cgroup first
	data, err := os.ReadFile("/proc/self/cgroup")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			// Docker cgroup v2: "0::/docker/abc123..."
			// Docker cgroup v1: "12:cpu:/docker/abc123..."
			if idx := strings.LastIndex(line, "/docker/"); idx != -1 {
				id := strings.TrimSpace(line[idx+len("/docker/"):])
				if len(id) > 0 {
					return id, nil
				}
			}
		}
	}

	// Fallback: use hostname (Docker sets it to container ID)
	hostname, err := os.Hostname()
	if err == nil && len(hostname) >= 12 {
		return hostname, nil
	}

	return "", fmt.Errorf("could not determine container ID")
}
func performBinarySelfUpdate(info *UpdateInfo, events *EventHub) error {
	newTag := strings.TrimPrefix(info.LatestVer, "v")
	arch := runtime.GOARCH

	binaryName := fmt.Sprintf("dockyard_%s_%s", newTag, arch)
	downloadURL := fmt.Sprintf(
		"https://github.com/%s/%s/releases/download/%s/%s",
		GitHubOwner, GitHubRepo, info.LatestVer, binaryName,
	)

	events.BroadcastLog("", "Downloading update...")

	tmpDir, err := os.MkdirTemp("", "dockyard-update-*")
	if err != nil {
		events.Broadcast(Event{Type: EventUpdateFailed, Message: "Failed to create temp directory"})
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	tmpFile := filepath.Join(tmpDir, "dockyard-new")
	if runtime.GOOS == "windows" {
		tmpFile = filepath.Join(tmpDir, "dockyard-new.exe")
	}

	if err := downloadFile(downloadURL, tmpFile); err != nil {
		events.Broadcast(Event{Type: EventUpdateFailed, Message: "Failed to download update"})
		return fmt.Errorf("download failed: %w", err)
	}

	events.BroadcastLog("", "Verifying download...")

	currentExe, err := os.Executable()
	if err != nil {
		events.Broadcast(Event{Type: EventUpdateFailed, Message: "Failed to determine current binary path"})
		return fmt.Errorf("cannot find current executable: %w", err)
	}
	currentExe, err = filepath.EvalSymlinks(currentExe)
	if err != nil {
		return fmt.Errorf("cannot resolve executable path: %w", err)
	}

	if err := os.Chmod(tmpFile, 0755); err != nil {
		events.Broadcast(Event{Type: EventUpdateFailed, Message: "Failed to set permissions on update"})
		return fmt.Errorf("chmod failed: %w", err)
	}

	backupPath := currentExe + ".bak"
	if err := copyFile(currentExe, backupPath); err != nil {
		logrus.WithError(err).Warn("Failed to backup current binary, continuing anyway")
	}

	if err := copyFile(tmpFile, currentExe); err != nil {
		if backupErr := copyFile(backupPath, currentExe); backupErr != nil {
			events.Broadcast(Event{Type: EventUpdateFailed, Message: "Update failed and rollback also failed"})
			return fmt.Errorf("update failed and rollback failed: %w", err)
		}
		events.Broadcast(Event{Type: EventUpdateFailed, Message: "Update failed, rolled back to previous version"})
		return fmt.Errorf("update failed, rolled back: %w", err)
	}

	os.Remove(backupPath)

	events.Broadcast(Event{
		Type:    EventUpdateComplete,
		Message: fmt.Sprintf("Updated to %s successfully! Restart to apply.", info.LatestVer),
	})

	return nil
}

func downloadFile(url, dest string) error {
	if !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("invalid download URL scheme")
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	hasher := sha256.New()
	writer := io.MultiWriter(out, hasher)

	if _, err := io.Copy(writer, resp.Body); err != nil {
		return err
	}

	checksum := hex.EncodeToString(hasher.Sum(nil))
	logrus.WithField("sha256", checksum).Info("Downloaded update checksum")

	return out.Close()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	info, err := in.Stat()
	if err == nil {
		os.Chmod(dst, info.Mode())
	}

	return out.Close()
}

func findDockerUpdateCommand(info *UpdateInfo) string {
	return fmt.Sprintf(
		"docker pull ghcr.io/%s/%s:%s && docker restart dockyard",
		GitHubOwner, GitHubRepo, info.LatestVer,
	)
}

func fetchReleaseNotes(url string) string {
	if !strings.HasPrefix(url, "https://") {
		return ""
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	return string(body)
}
