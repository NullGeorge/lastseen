package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capture runs a command with stdout redirected to a pipe and returns what it
// printed, so the copy-and-paste output can be asserted verbatim.
func capture(t *testing.T, fn func(out *os.File) error) (string, error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	runErr := fn(writer)
	writer.Close()

	var printed strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		printed.Write(buf[:n])
		if err != nil {
			break
		}
	}
	reader.Close()
	return printed.String(), runErr
}

func TestParseMonitors(t *testing.T) {
	tests := []struct {
		name  string
		list  string
		want  []string
		fails bool
	}{
		{name: "one", list: "atsos", want: []string{"atsos"}},
		{name: "several", list: "atsos,nas,backup", want: []string{"atsos", "nas", "backup"}},
		{name: "spaces are trimmed", list: " atsos , nas ", want: []string{"atsos", "nas"}},
		{name: "trailing comma", list: "atsos,", want: []string{"atsos"}},
		{name: "empty", list: "", fails: true},
		{name: "only commas", list: ",,", fails: true},
		{name: "duplicate", list: "atsos,atsos", fails: true},
		// A name that cannot survive being a url segment would register a
		// route nobody can ever hit, which looks like a monitor that simply
		// never checks in.
		{name: "slash", list: "atsos/nas", fails: true},
		{name: "space inside", list: "my host", fails: true},
		{name: "query marker", list: "atsos?x", fails: true},
		{name: "fragment", list: "atsos#1", fails: true},
		{name: "percent", list: "atsos%2f", fails: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMonitors(tt.list)
			if tt.fails {
				if err == nil {
					t.Fatalf("parseMonitors(%q) = %v, want an error", tt.list, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMonitors(%q): %v", tt.list, err)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("parseMonitors(%q) = %v, want %v", tt.list, got, tt.want)
			}
		})
	}
}

func TestResolveChat(t *testing.T) {
	t.Setenv("CHAT_ID", "222")
	if got, err := resolveChat(111); err != nil || got != 111 {
		t.Errorf("flag should win: got %d, %v", got, err)
	}
	if got, err := resolveChat(0); err != nil || got != 222 {
		t.Errorf("env should be used: got %d, %v", got, err)
	}

	t.Setenv("CHAT_ID", " 333 ")
	if got, err := resolveChat(0); err != nil || got != 333 {
		t.Errorf("env should be trimmed: got %d, %v", got, err)
	}

	t.Setenv("CHAT_ID", "not-a-number")
	if _, err := resolveChat(0); err == nil {
		t.Error("a non numeric CHAT_ID was accepted")
	}

	os.Unsetenv("CHAT_ID")
	if _, err := resolveChat(0); err == nil {
		t.Error("a missing chat was accepted")
	}
}

// init is the whole point of the config: nobody should be typing a secret or
// an address by hand.
func TestInitGeneratesEverythingAndPrintsTheUrls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lastseen.conf")

	printed, err := capture(t, func(out *os.File) error {
		return initConfig([]string{
			"-config", path, "-monitors", "atsos,nas",
			"-advertise", "https://watch.example.com:9000",
		}, out)
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("the config init wrote does not load: %v", err)
	}
	if len(cfg.Secret) < 32 {
		t.Errorf("secret %q is too short to be worth having", cfg.Secret)
	}

	for _, want := range []string{
		"https://watch.example.com:9000/ping/" + cfg.Secret + "/atsos",
		"https://watch.example.com:9000/ping/" + cfg.Secret + "/nas",
		"https://watch.example.com:9000/status/" + cfg.Secret,
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("output is missing %q:\n%s", want, printed)
		}
	}

	// The secret is the one thing that must not be world readable.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("config mode = %o, want 600", mode)
	}
}

// Regenerating the secret would orphan every client already using the old
// one, and they would go silent without anybody noticing why.
func TestInitRefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lastseen.conf")
	args := []string{"-config", path, "-monitors", "atsos", "-advertise", "http://host:9000"}

	if _, err := capture(t, func(out *os.File) error { return initConfig(args, out) }); err != nil {
		t.Fatalf("first init: %v", err)
	}
	first, err := loadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	_, err = capture(t, func(out *os.File) error { return initConfig(args, out) })
	if err == nil {
		t.Fatal("a second init overwrote the config")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v, want it to say the file is already there", err)
	}

	second, err := loadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if second.Secret != first.Secret {
		t.Error("the secret changed despite the refusal")
	}
}

func TestInitRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "no monitors",
			args: []string{"-config", filepath.Join(dir, "a.conf"), "-advertise", "http://h:9000"},
			want: "-monitors is required",
		},
		{
			name: "advertise without a scheme",
			args: []string{"-config", filepath.Join(dir, "b.conf"), "-monitors", "atsos", "-advertise", "watch:9000"},
			want: "must start with http",
		},
		{
			name: "advertise with no host",
			args: []string{"-config", filepath.Join(dir, "c.conf"), "-monitors", "atsos", "-advertise", "http://"},
			want: "has no host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := capture(t, func(out *os.File) error { return initConfig(tt.args, out) })
			if err == nil {
				t.Fatal("init accepted it")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// Detection is what makes init work with no arguments on the common setup:
// a watchdog on the same network as the things it watches.
func TestInitDetectsTheAddressWhenNotTold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lastseen.conf")

	printed, err := capture(t, func(out *os.File) error {
		return initConfig([]string{"-config", path, "-monitors", "atsos", "-listen", ":4732"}, out)
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.HasPrefix(cfg.Advertise, "http://") {
		t.Errorf("advertise = %q, want a detected http url", cfg.Advertise)
	}
	// The port has to come from -listen, not from a default nobody chose.
	if !strings.HasSuffix(cfg.Advertise, ":4732") {
		t.Errorf("advertise = %q, want it to carry the listen port", cfg.Advertise)
	}
	if !strings.Contains(printed, cfg.Advertise+"/ping/"+cfg.Secret+"/atsos") {
		t.Errorf("printed urls do not use the detected address:\n%s", printed)
	}
}

func TestUrlsReprintsWhatWasGenerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lastseen.conf")
	first, err := capture(t, func(out *os.File) error {
		return initConfig([]string{"-config", path, "-monitors", "atsos,nas", "-advertise", "http://h:9000"}, out)
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	again, err := capture(t, func(out *os.File) error { return showURLs([]string{"-config", path}, out) })
	if err != nil {
		t.Fatalf("urls: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(again), "\n") {
		if !strings.Contains(first, strings.TrimSpace(line)) {
			t.Errorf("urls printed %q, which init never showed", line)
		}
	}
}

func TestUrlsNeedsAConfig(t *testing.T) {
	_, err := capture(t, func(out *os.File) error {
		return showURLs([]string{"-config", filepath.Join(t.TempDir(), "absent.conf")}, out)
	})
	if err == nil {
		t.Fatal("urls invented a config out of nothing")
	}
}

func TestAddAppendsAMonitorAndKeepsTheSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lastseen.conf")
	if _, err := capture(t, func(out *os.File) error {
		return initConfig([]string{"-config", path, "-monitors", "atsos", "-advertise", "http://h:9000"}, out)
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	before, _ := loadConfig(path)

	printed, err := capture(t, func(out *os.File) error {
		return addMonitor([]string{"-config", path, "nas"}, out)
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	after, err := loadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if strings.Join(after.Monitors, ",") != "atsos,nas" {
		t.Errorf("monitors = %v, want atsos and nas", after.Monitors)
	}
	if after.Secret != before.Secret {
		t.Error("adding a monitor changed the secret, orphaning every existing client")
	}
	if !strings.Contains(printed, "/ping/"+after.Secret+"/nas") {
		t.Errorf("add did not print the new url:\n%s", printed)
	}
}

func TestAddRejectsDuplicatesAndBadNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lastseen.conf")
	if _, err := capture(t, func(out *os.File) error {
		return initConfig([]string{"-config", path, "-monitors", "atsos", "-advertise", "http://h:9000"}, out)
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	for _, name := range []string{"atsos", "with/slash", "with space"} {
		if _, err := capture(t, func(out *os.File) error {
			return addMonitor([]string{"-config", path, name}, out)
		}); err == nil {
			t.Errorf("add accepted %q", name)
		}
	}

	cfg, _ := loadConfig(path)
	if len(cfg.Monitors) != 1 {
		t.Errorf("monitors = %v, want the refusals to have changed nothing", cfg.Monitors)
	}
}

func TestAddNeedsExactlyOneName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lastseen.conf")
	if _, err := capture(t, func(out *os.File) error {
		return initConfig([]string{"-config", path, "-monitors", "atsos", "-advertise", "http://h:9000"}, out)
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	for _, args := range [][]string{{"-config", path}, {"-config", path, "a", "b"}} {
		if _, err := capture(t, func(out *os.File) error { return addMonitor(args, out) }); err == nil {
			t.Errorf("add(%v) was accepted", args)
		}
	}
}

// Every refusal here is a misconfiguration that would otherwise produce a
// watchdog that looks healthy and guards nothing.
func TestRunRefusesToStartMisconfigured(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.conf")
	if _, err := capture(t, func(out *os.File) error {
		return initConfig([]string{"-config", good, "-monitors", "atsos", "-advertise", "http://h:9000"}, out)
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	broken := filepath.Join(dir, "broken.conf")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	secretless := filepath.Join(dir, "secretless.conf")
	if err := os.WriteFile(secretless, []byte(`{"monitors":["atsos"]}`), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	tests := []struct {
		name  string
		args  []string
		token string
		chat  string
		want  string
	}{
		{name: "missing config", args: []string{"-config", filepath.Join(dir, "nope.conf")},
			token: "bot", chat: "42", want: "read config"},
		{name: "corrupt config", args: []string{"-config", broken},
			token: "bot", chat: "42", want: "parse config"},
		{name: "config without a secret", args: []string{"-config", secretless},
			token: "bot", chat: "42", want: "no secret"},
		{name: "no bot token", args: []string{"-config", good},
			token: "", chat: "42", want: "BOT_TOKEN is not set"},
		{name: "no chat", args: []string{"-config", good},
			token: "bot", chat: "", want: "is not a chat id"},
		{name: "unknown flag", args: []string{"-nonsense"},
			token: "bot", chat: "42", want: "flag provided but not defined"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BOT_TOKEN", tt.token)
			t.Setenv("CHAT_ID", tt.chat)
			stderr := os.Stderr
			os.Stderr, _ = os.Open(os.DevNull)
			err := run(tt.args)
			os.Stderr = stderr

			if err == nil {
				t.Fatalf("run(%v) started anyway", tt.args)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}
