package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// collector stands in for Telegram and records what would have been said.
type collector struct {
	sent []string
	fail error
}

func (c *collector) notify(_ context.Context, text string) error {
	c.sent = append(c.sent, text)
	return c.fail
}

func TestOnlyDeclaredNamesBehindTheSecretAreServed(t *testing.T) {
	watches := newTestMonitor(t, "atsos", "nas")
	said := &collector{}
	server := httptest.NewServer(newMux(context.Background(), "s3cret", watches, said.notify))
	defer server.Close()

	ok := []string{"/ping/s3cret/atsos", "/ping/s3cret/nas", "/status/s3cret"}
	for _, path := range ok {
		response, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, response.StatusCode)
		}
	}

	// A wrong secret, an undeclared name and a missing segment must all be
	// indistinguishable from "there is nothing here".
	refused := []string{
		"/ping/s3cret/intruder", "/ping/wrong/atsos", "/ping/s3cret",
		"/ping", "/status", "/status/wrong", "/", "/ping/s3cret/atsos/extra",
	}
	for _, path := range refused {
		response, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, response.StatusCode)
		}
	}
}

func TestPingRecordsTheCheckInAndPersistsIt(t *testing.T) {
	watches := newTestMonitor(t, "atsos")
	said := &collector{}
	server := httptest.NewServer(newMux(context.Background(), "s3cret", watches, said.notify))
	defer server.Close()

	before := watches.report(start)[0].LastSeen
	response, err := http.Get(server.URL + "/ping/s3cret/atsos")
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	defer response.Body.Close()

	if after := watches.report(start)[0].LastSeen; !after.After(before) {
		t.Error("the check-in was not recorded")
	}
	// A watchdog that forgets across a restart is a watchdog that re-alerts.
	reloaded, err := newMonitor([]string{"atsos"}, time.Minute, watches.statePath, start, quiet())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.report(start)[0].LastSeen.Equal(before) {
		t.Error("the check-in never reached the state file")
	}
	if len(said.sent) != 0 {
		t.Errorf("a routine ping sent %v, want silence", said.sent)
	}
}

func TestRecoveryIsAnnouncedOnceOnTheNextPing(t *testing.T) {
	watches := newTestMonitor(t, "atsos")
	said := &collector{}
	server := httptest.NewServer(newMux(context.Background(), "s3cret", watches, said.notify))
	defer server.Close()

	watches.check(start.Add(20 * time.Minute)) // raise the alert

	for range 2 {
		response, err := http.Get(server.URL + "/ping/s3cret/atsos")
		if err != nil {
			t.Fatalf("ping: %v", err)
		}
		response.Body.Close()
	}

	if len(said.sent) != 1 {
		t.Fatalf("sent %v, want exactly one recovery message", said.sent)
	}
	if !strings.HasPrefix(said.sent[0], "atsos is back") {
		t.Errorf("message = %q", said.sent[0])
	}
}

// A failing Telegram must not cost the check-in: the ping is the fact, the
// message is only the announcement.
func TestAFailedRecoveryStillRecordsThePing(t *testing.T) {
	watches := newTestMonitor(t, "atsos")
	said := &collector{fail: context.DeadlineExceeded}
	server := httptest.NewServer(newMux(context.Background(), "s3cret", watches, said.notify))
	defer server.Close()

	watches.check(start.Add(20 * time.Minute))
	response, err := http.Get(server.URL + "/ping/s3cret/atsos")
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 even though the message failed", response.StatusCode)
	}
	if due := watches.check(start.Add(21 * time.Minute)); len(due) != 0 {
		t.Error("the alert was not cleared by the ping")
	}
}

func TestSweepAnnouncesEachSilenceOnce(t *testing.T) {
	watches := newTestMonitor(t, "atsos", "nas")
	said := &collector{}
	watches.ping("atsos", start.Add(30*time.Minute))

	sweep(context.Background(), watches, said.notify, start.Add(30*time.Minute))
	if len(said.sent) != 1 {
		t.Fatalf("sent %v, want one alert for nas only", said.sent)
	}
	if !strings.HasPrefix(said.sent[0], "nas is silent") || !strings.Contains(said.sent[0], "tolerated 15m0s") {
		t.Errorf("message = %q", said.sent[0])
	}

	sweep(context.Background(), watches, said.notify, start.Add(45*time.Minute))
	if len(said.sent) != 1 {
		t.Errorf("sent %v, want no repeat while the outage continues", said.sent)
	}
}

func TestStatusPageNamesWhoIsQuiet(t *testing.T) {
	watches := newTestMonitor(t, "atsos", "nas")
	watches.ping("atsos", start.Add(29*time.Minute))

	page := statusPage(watches, start.Add(30*time.Minute))
	lines := strings.Split(strings.TrimSpace(page), "\n")
	if len(lines) != 2 {
		t.Fatalf("status has %d lines, want 2:\n%s", len(lines), page)
	}
	if !strings.HasPrefix(lines[0], "atsos") || !strings.Contains(lines[0], "ok") {
		t.Errorf("atsos line = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "nas") || !strings.Contains(lines[1], "SILENT") {
		t.Errorf("nas line = %q", lines[1])
	}
	if !strings.Contains(lines[1], "30m0s ago") {
		t.Errorf("nas line does not say how long: %q", lines[1])
	}
}

func TestWatchLoopStopsWithTheContext(t *testing.T) {
	watches := newTestMonitor(t, "atsos")
	said := &collector{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		watchLoop(ctx, watches, said.notify, time.Millisecond)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("watchLoop ignored the cancelled context")
	}
}
