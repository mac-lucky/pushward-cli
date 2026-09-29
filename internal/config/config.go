// Package config resolves the API URL and integration key: environment
// first, then the file written by `pushward auth login`.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mac-lucky/pushward-cli/internal/api"
)

const (
	EnvToken  = "PUSHWARD_API_TOKEN" // #nosec G101 -- the variable name, not a credential
	EnvURL    = "PUSHWARD_API_URL"
	EnvConfig = "PUSHWARD_CONFIG_DIR"
	fileName  = "config.json"
)

// File is the on-disk config. The token sits in a 0600 file in a 0700
// directory, the same trust level as ~/.netrc or gh's hosts.yml.
type File struct {
	APIURL string `json:"api_url,omitempty"`
	Token  string `json:"token,omitempty"`
}

type Config struct {
	APIURL string
	Token  string
	// TokenSource says where the token came from: "env", the config file
	// path, or "" when there is none.
	TokenSource string
	// Warning is set when the config file is readable by other users.
	Warning string
}

// Dir returns the config directory: $PUSHWARD_CONFIG_DIR, then
// $XDG_CONFIG_HOME/pushward, then the OS default.
func Dir() (string, error) {
	if d := os.Getenv(EnvConfig); d != "" {
		return d, nil
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "pushward"), nil
	}
	if runtime.GOOS == "windows" {
		d, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(d, "pushward"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "pushward"), nil
}

func Path() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, fileName), nil
}

// Read loads the config file. A missing file is an empty config, not an error.
func Read() (File, string, error) {
	var f File
	p, err := Path()
	if err != nil {
		return f, "", err
	}
	data, err := os.ReadFile(p) // #nosec G304 -- the path is the CLI's own config file
	if errors.Is(err, fs.ErrNotExist) {
		return f, p, nil
	}
	if err != nil {
		return f, p, err
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, p, fmt.Errorf("%s: %w", p, err)
	}
	return f, p, nil
}

// Write replaces the config file atomically with 0600 permissions.
func Write(f File) (string, error) {
	p, err := Path()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".config-*.json")
	if err != nil {
		return "", err
	}
	// Cleans up after a failed write; after the rename it finds nothing.
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return p, os.Rename(tmp.Name(), p)
}

// Load resolves the effective config. apiURL is the --api-url flag and wins
// over everything when set.
func Load(apiURL string) (Config, error) {
	f, p, err := Read()
	if err != nil {
		return Config{}, err
	}
	c := Config{APIURL: api.DefaultBaseURL}
	switch {
	case apiURL != "":
		c.APIURL = apiURL
	case os.Getenv(EnvURL) != "":
		c.APIURL = os.Getenv(EnvURL)
	case f.APIURL != "":
		c.APIURL = f.APIURL
	}
	switch {
	case os.Getenv(EnvToken) != "":
		c.Token, c.TokenSource = strings.TrimSpace(os.Getenv(EnvToken)), "env"
	case f.Token != "":
		c.Token, c.TokenSource = f.Token, p
		if info, err := os.Stat(p); err == nil && runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			c.Warning = fmt.Sprintf("%s is readable by other users; run chmod 600 on it", p)
		}
	}
	u, err := ValidateURL(c.APIURL)
	if err != nil {
		return Config{}, err
	}
	c.APIURL = u
	return c, nil
}

// ValidateURL requires https, except for loopback hosts during local
// development, so a key never crosses the network in the clear.
func ValidateURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid API URL %q", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		host := u.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", fmt.Errorf("API URL %q must use https (http is allowed only for localhost)", raw)
		}
	default:
		return "", fmt.Errorf("API URL %q must use https", raw)
	}
	return u.String(), nil
}
