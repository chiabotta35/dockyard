// Package meta provides centralized application metadata for Watchtower.
//
// It holds compile-time values such as the version string and derived
// identifiers (e.g., the HTTP User-Agent header). Version is typically
// injected via linker flags at build time; if unset it falls back to
// "v0.0.0-unknown".
package meta

var (
	// Version is the compile-time set version of Dockyard.
	Version = "v0.1.0"

	// UserAgent is the HTTP client identifier derived from Version.
	UserAgent string
)

func init() {
	UserAgent = "Dockyard/" + Version
}
