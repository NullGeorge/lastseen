package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The config is meant to be edited by hand, so a duration has to look like a
// duration and not like a nanosecond count.
func TestDurationIsHumanReadable(t *testing.T) {
	encoded, err := json.Marshal(Duration(90 * time.Second))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != `"1m30s"` {
		t.Errorf("encoded as %s, want \"1m30s\"", encoded)
	}

	var back Duration
	if err := json.Unmarshal([]byte(`"2h"`), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if time.Duration(back) != 2*time.Hour {
		t.Errorf("decoded %s, want 2h", time.Duration(back))
	}

	for _, bad := range []string{`"tomorrow"`, `"15"`, `900`, `null`} {
		var d Duration
		if err := json.Unmarshal([]byte(bad), &d); err == nil {
			t.Errorf("unmarshal accepted %s", bad)
		}
	}
}

func TestUrlsRenderEveryMonitorPlusStatus(t *testing.T) {
	cfg := &Config{
		Advertise: "https://watch.example.com:9000/",
		Secret:    "s3cret",
		Monitors:  []string{"atsos", "nas"},
	}

	lines := cfg.urls()
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want one per monitor plus status:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	// A trailing slash in the configured base must not produce a double slash,
	// which some proxies answer with a redirect and others with a 404.
	for i, want := range []string{
		"https://watch.example.com:9000/ping/s3cret/atsos",
		"https://watch.example.com:9000/ping/s3cret/nas",
		"https://watch.example.com:9000/status/s3cret",
	} {
		if !strings.HasSuffix(lines[i], want) {
			t.Errorf("line %d = %q, want it to end with %q", i, lines[i], want)
		}
	}
}

func TestValidateAdvertise(t *testing.T) {
	good := []string{"http://h:9000", "https://watch.example.com", "http://203.0.113.41:4732"}
	for _, raw := range good {
		if err := validateAdvertise(raw); err != nil {
			t.Errorf("validateAdvertise(%q) = %v, want it accepted", raw, err)
		}
	}

	bad := []string{"", "watch:9000", "ftp://h", "http://", "://nope"}
	for _, raw := range bad {
		if err := validateAdvertise(raw); err == nil {
			t.Errorf("validateAdvertise(%q) accepted it", raw)
		}
	}
}

func TestGuessAdvertiseUsesTheListenPort(t *testing.T) {
	base, _, err := guessAdvertise(":4732")
	if err != nil {
		t.Fatalf("guessAdvertise: %v", err)
	}
	if !strings.HasPrefix(base, "http://") || !strings.HasSuffix(base, ":4732") {
		t.Errorf("base = %q, want http://<address>:4732", base)
	}
	if err := validateAdvertise(base); err != nil {
		t.Errorf("the detected base does not survive validation: %v", err)
	}

	// A listen value with no port at all must still produce something usable
	// rather than a url ending in a colon.
	fallback, _, err := guessAdvertise("")
	if err != nil {
		t.Fatalf("guessAdvertise: %v", err)
	}
	if err := validateAdvertise(fallback); err != nil {
		t.Errorf("fallback base is invalid: %v", err)
	}
}

func TestNewSecretIsLongAndDifferentEveryTime(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		secret, err := newSecret()
		if err != nil {
			t.Fatalf("newSecret: %v", err)
		}
		if len(secret) < 32 {
			t.Fatalf("secret %q is too short", secret)
		}
		if strings.ContainsAny(secret, "/+= ") {
			t.Fatalf("secret %q contains characters that break a url path", secret)
		}
		if seen[secret] {
			t.Fatal("newSecret repeated itself")
		}
		seen[secret] = true
	}
}

func TestSaveReportsAFailure(t *testing.T) {
	cfg := &Config{Secret: "s", Monitors: []string{"atsos"}, Advertise: "http://h:9000"}
	// A path whose parent is a file, not a directory.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	if err := cfg.save(filepath.Join(blocker, "nested", "lastseen.conf")); err == nil {
		t.Fatal("save reported success into an impossible path")
	}
}

// A config that parses but declares nothing to watch would start a daemon
// that guards nothing, so it is refused at load time.
func TestLoadRejectsAConfigWithoutMonitors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lastseen.conf")
	if err := os.WriteFile(path, []byte(`{"secret":"s","monitors":[]}`), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("a config with no monitors was accepted")
	}
}

func TestConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lastseen.conf")
	original := &Config{
		Listen: ":9000", Advertise: "http://h:9000", Secret: "s3cret",
		Grace: Duration(15 * time.Minute), CheckInterval: Duration(30 * time.Second),
		State: "/var/lib/lastseen/state.json", Monitors: []string{"atsos", "nas"},
	}
	if err := original.save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := loadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(loaded, original) {
		t.Errorf("loaded = %+v, want %+v", loaded, original)
	}
}
