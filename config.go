package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// defaultConfigPath is where init writes and the other commands look.
const defaultConfigPath = "/etc/lastseen.conf"

// Duration keeps the config file readable: "15m" rather than a nanosecond
// count nobody can eyeball.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(raw []byte) error {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("%q is not a duration: %w", text, err)
	}
	*d = Duration(parsed)
	return nil
}

type Config struct {
	Listen string `json:"listen"`
	// Advertise is the base url clients are told to use. The daemon cannot
	// work this out at run time: it knows what it binds, not what is in front
	// of it, and a host can reach the world by more than one path.
	Advertise     string   `json:"advertise"`
	Secret        string   `json:"secret"`
	Grace         Duration `json:"grace"`
	CheckInterval Duration `json:"check_interval"`
	State         string   `json:"state"`
	Monitors      []string `json:"monitors"`
}

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.Secret == "" {
		return nil, fmt.Errorf("config %s has no secret", path)
	}
	if _, err := parseMonitors(strings.Join(cfg.Monitors, ",")); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &cfg, nil
}

// save writes the config readable only by its owner, because it holds the
// secret that lets anyone silence every alert.
func (c *Config) save(path string) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create config directory: %w", err)
		}
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

// urls renders one copy-and-paste line per monitor plus the status page.
func (c *Config) urls() []string {
	base := strings.TrimSuffix(c.Advertise, "/")
	lines := make([]string, 0, len(c.Monitors)+1)
	for _, name := range c.Monitors {
		lines = append(lines, fmt.Sprintf("%-24s %s/ping/%s/%s", name, base, c.Secret, name))
	}
	return append(lines, fmt.Sprintf("%-24s %s/status/%s", "(status page)", base, c.Secret))
}

// newSecret returns a url-safe secret with 192 bits of entropy, which is far
// past anything worth guessing over a network.
func newSecret() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// guessAdvertise builds the base url from the address this host would use to
// reach the internet. Connecting a UDP socket sends no packet; it only asks
// the kernel which route and source address it would pick.
//
// Behind NAT this yields a private address, which is correct for a watchdog
// watching its own network and wrong for anything outside it. The caller is
// told which one it got rather than left to find out from silence.
func guessAdvertise(listen string) (string, bool, error) {
	conn, err := net.Dial("udp", "1.1.1.1:53")
	if err != nil {
		return "", false, fmt.Errorf("cannot work out this host's address: %w", err)
	}
	defer conn.Close()

	host := conn.LocalAddr().(*net.UDPAddr).IP
	port := "9000"
	if _, listenPort, err := net.SplitHostPort(listen); err == nil && listenPort != "" {
		port = listenPort
	}
	return "http://" + net.JoinHostPort(host.String(), port), host.IsPrivate(), nil
}

// validateAdvertise refuses a base url that would render into a link nobody
// can follow, which would only be discovered as a monitor that never reports.
func validateAdvertise(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is not a url: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%q must start with http:// or https://", raw)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%q has no host", raw)
	}
	return nil
}
