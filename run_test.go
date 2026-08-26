package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// freePort reserves a port and lets it go, which is the usual way to get one
// nobody else is on without hardcoding a number.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// The daemon is the thing that actually ships, so it is worth starting for
// real: a config on disk, a listening socket, a Bot API to talk to, and a
// signal to stop it the way systemd would.
func TestRunServesAndShutsDownOnSignal(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	var polls int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		// A poll here would mean the daemon took the bot's command channel
		// without being asked to, which costs another process its own.
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			mu.Lock()
			polls++
			mu.Unlock()
			select {
			case <-time.After(100 * time.Millisecond):
			case <-r.Context().Done():
			}
			w.Write([]byte(`{"ok":true,"result":[]}`))
			return
		}
		mu.Lock()
		sent = append(sent, r.PostForm.Get("text"))
		mu.Unlock()
		w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer api.Close()

	dir := t.TempDir()
	config := filepath.Join(dir, "lastseen.conf")
	port := freePort(t)
	if _, err := capture(t, func(out *os.File) error {
		return initConfig([]string{
			"-config", config, "-monitors", "atsos,nas",
			"-advertise", "http://127.0.0.1:" + fmt.Sprint(port),
			"-listen", "127.0.0.1:" + fmt.Sprint(port),
			"-grace", "1s", "-check-interval", "100ms",
			"-state", filepath.Join(dir, "state.json"),
		}, out)
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	cfg, err := loadConfig(config)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	t.Setenv("BOT_TOKEN", "111:FAKE")
	t.Setenv("CHAT_ID", "42")

	done := make(chan error, 1)
	go func() {
		done <- run([]string{"-config", config, "-telegram-api", api.URL})
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitFor(t, func() bool {
		response, err := http.Get(base + "/status/" + cfg.Secret)
		if err != nil {
			return false
		}
		response.Body.Close()
		return response.StatusCode == http.StatusOK
	}, "the server never came up")

	// Both monitors were armed at start-up and neither has checked in, so
	// with a one second grace both must be reported, once each.
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(sent) == 2
	}, "the silent monitors were never reported")

	mu.Lock()
	first := strings.Join(sent, "\n")
	mu.Unlock()
	for _, want := range []string{"atsos is silent", "nas is silent"} {
		if !strings.Contains(first, want) {
			t.Errorf("messages = %q, want one containing %q", first, want)
		}
	}

	// A check-in must be accepted on the real socket and must announce the
	// recovery, which proves the http layer and the notifier are wired up.
	response, err := http.Get(base + "/ping/" + cfg.Secret + "/atsos")
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	response.Body.Close()
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(sent) == 3 && strings.Contains(sent[2], "atsos is back")
	}, "the recovery was never announced")

	// systemd stops a service with a signal; the exit has to be clean.
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("signal: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v, want a clean shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run ignored SIGINT")
	}

	// What it learned has to survive, otherwise a restart re-alerts.
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Errorf("no state was written: %v", err)
	}

	// The bot's command channel belongs to whoever was told to take it. Only
	// one getUpdates caller per token is allowed, and a second one costs both
	// of them their commands, so this must stay silent unless asked.
	mu.Lock()
	polled := polls
	mu.Unlock()
	if polled != 0 {
		t.Errorf("polled getUpdates %d times without -commands", polled)
	}
}

// And with the flag it answers for real, through the same wiring that ships.
func TestRunAnswersCommandsWhenAsked(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	served := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			mu.Lock()
			first := !served
			served = true
			mu.Unlock()
			if first {
				w.Write([]byte(`{"ok":true,"result":[{"update_id":1,` +
					`"message":{"chat":{"id":42},"text":"/status"}}]}`))
				return
			}
			select {
			case <-time.After(100 * time.Millisecond):
			case <-r.Context().Done():
			}
			w.Write([]byte(`{"ok":true,"result":[]}`))
			return
		}
		mu.Lock()
		sent = append(sent, r.PostForm.Get("text"))
		mu.Unlock()
		w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer api.Close()

	dir := t.TempDir()
	config := filepath.Join(dir, "lastseen.conf")
	port := freePort(t)
	if _, err := capture(t, func(out *os.File) error {
		return initConfig([]string{
			"-config", config, "-monitors", "atsos",
			"-advertise", "http://127.0.0.1:" + fmt.Sprint(port),
			"-listen", "127.0.0.1:" + fmt.Sprint(port),
			"-grace", "1h", "-check-interval", "1h",
			"-state", filepath.Join(dir, "state.json"),
		}, out)
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	t.Setenv("BOT_TOKEN", "111:FAKE")
	t.Setenv("CHAT_ID", "42")

	done := make(chan error, 1)
	go func() {
		done <- run([]string{"-config", config, "-telegram-api", api.URL, "-commands"})
	}()

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(sent) == 1
	}, "the command was never answered")

	mu.Lock()
	reply := sent[0]
	mu.Unlock()
	// A grace of an hour means it is not silent, so this is the plain form.
	if !strings.HasPrefix(reply, "atsos: ") || strings.Contains(reply, "тишина") {
		t.Errorf("reply = %q, want a plain status line for atsos", reply)
	}

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("signal: %v", err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("run ignored SIGINT")
	}
}

func waitFor(t *testing.T, condition func() bool, complaint string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(complaint)
}

func TestRunRefusesAPortAlreadyTaken(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer busy.Close()

	dir := t.TempDir()
	config := filepath.Join(dir, "lastseen.conf")
	if _, err := capture(t, func(out *os.File) error {
		return initConfig([]string{
			"-config", config, "-monitors", "atsos", "-advertise", "http://h:1",
			"-listen", busy.Addr().String(), "-state", filepath.Join(dir, "state.json"),
		}, out)
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	t.Setenv("BOT_TOKEN", "111:FAKE")
	t.Setenv("CHAT_ID", "42")
	if err := run([]string{"-config", config}); err == nil {
		t.Fatal("run claimed a port that was already taken")
	}
}

// A flag override has to win over the file, otherwise a container cannot be
// pointed somewhere else without rewriting its config.
func TestRunOverridesListenAndState(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "lastseen.conf")
	if _, err := capture(t, func(out *os.File) error {
		return initConfig([]string{
			"-config", config, "-monitors", "atsos", "-advertise", "http://h:1",
			"-listen", "127.0.0.1:1", "-state", "/nonexistent/state.json",
		}, out)
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	t.Setenv("BOT_TOKEN", "111:FAKE")
	t.Setenv("CHAT_ID", "42")

	// The command loop starts polling immediately, so the Bot API has to be a
	// local stub: a test must not reach out to the real one.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(100 * time.Millisecond):
		case <-r.Context().Done():
		}
		w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer api.Close()

	// Port 1 is unbindable for a normal user and the state path is unwritable,
	// so if the overrides are ignored this fails for the wrong reason. Point
	// both somewhere usable and expect it to get as far as listening.
	port := freePort(t)
	done := make(chan error, 1)
	go func() {
		done <- run([]string{
			"-config", config,
			"-listen", fmt.Sprintf("127.0.0.1:%d", port),
			"-state", filepath.Join(dir, "state.json"),
			"-telegram-api", api.URL,
		})
	}()

	waitFor(t, func() bool {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}, "the listen override was ignored")

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("signal: %v", err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("run ignored SIGINT")
	}
}

// usage has to name every subcommand main dispatches, or a command exists
// that nobody can discover.
func TestUsageNamesEverySubcommand(t *testing.T) {
	printed, err := capture(t, func(out *os.File) error {
		stdout := os.Stdout
		os.Stdout = out
		usage()
		os.Stdout = stdout
		return nil
	})
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	for _, command := range []string{"run", "init", "urls", "add", "version"} {
		if !strings.Contains(printed, command) {
			t.Errorf("usage does not mention %q:\n%s", command, printed)
		}
	}
}
