package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func quiet() *log.Logger { return log.New(io.Discard, "", 0) }

var start = time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)

func newTestMonitor(t *testing.T, names ...string) *monitor {
	t.Helper()
	m, err := newMonitor(names, 15*time.Minute, filepath.Join(t.TempDir(), "state.json"), start, quiet())
	if err != nil {
		t.Fatalf("newMonitor: %v", err)
	}
	return m
}

// The point of a dead-man switch is that it fires once. Repeating every tick
// trains you to ignore it, and then it is worth nothing when it matters.
func TestSilenceAlertsOncePerOutage(t *testing.T) {
	m := newTestMonitor(t, "atsos")

	if due := m.check(start.Add(15 * time.Minute)); len(due) != 0 {
		t.Fatalf("alerted at the grace boundary, which is not yet overdue: %+v", due)
	}

	due := m.check(start.Add(16 * time.Minute))
	if len(due) != 1 || due[0].Name != "atsos" || due[0].SilentFor != 16*time.Minute {
		t.Fatalf("first alert = %+v, want one for atsos after 16m", due)
	}

	for _, minutes := range []int{17, 30, 240} {
		if repeated := m.check(start.Add(time.Duration(minutes) * time.Minute)); len(repeated) != 0 {
			t.Fatalf("alerted again at %dm: one outage must produce one alert", minutes)
		}
	}
}

// Each monitor keeps its own clock. One noisy neighbour must not mask another
// going quiet, which is the whole reason for having several in one process.
func TestMonitorsAreIndependent(t *testing.T) {
	m := newTestMonitor(t, "atsos", "nas", "backup")

	// atsos keeps checking in, nas does not, backup never has.
	for minutes := 5; minutes <= 30; minutes += 5 {
		m.ping("atsos", start.Add(time.Duration(minutes)*time.Minute))
	}
	m.ping("nas", start.Add(2*time.Minute))

	due := m.check(start.Add(30 * time.Minute))
	if len(due) != 2 {
		t.Fatalf("alerts = %+v, want nas and backup", due)
	}
	if due[0].Name != "backup" || due[1].Name != "nas" {
		t.Errorf("alerts = %+v, want them sorted: backup then nas", due)
	}
	if due[1].SilentFor != 28*time.Minute {
		t.Errorf("nas silent for %s, want 28m measured from its own last ping", due[1].SilentFor)
	}
}

// A client that dies before it ever checks in is exactly the case a watchdog
// must not miss, so a declared name is armed from start-up.
func TestDeclaredButNeverSeenIsReported(t *testing.T) {
	m := newTestMonitor(t, "backup")

	if due := m.check(start.Add(16 * time.Minute)); len(due) != 1 || due[0].Name != "backup" {
		t.Fatalf("alerts = %+v, want backup reported despite never checking in", due)
	}
}

func TestPingClearsTheAlertAndReportsRecovery(t *testing.T) {
	m := newTestMonitor(t, "atsos")

	if due := m.check(start.Add(20 * time.Minute)); len(due) != 1 {
		t.Fatalf("expected an alert, got %+v", due)
	}

	recovered, silentFor := m.ping("atsos", start.Add(25*time.Minute))
	if !recovered {
		t.Fatal("a ping after an alert must report recovery")
	}
	if silentFor != 25*time.Minute {
		t.Errorf("silence reported as %s, want 25m", silentFor)
	}

	if due := m.check(start.Add(30 * time.Minute)); len(due) != 0 {
		t.Fatalf("alerted right after recovery: %+v", due)
	}
	if due := m.check(start.Add(41 * time.Minute)); len(due) != 1 {
		t.Fatal("a second outage must alert again")
	}
}

// An unknown name must change nothing. The router answers 404 for it, and
// even if it did not, the state must not grow a monitor nobody declared.
func TestPingForAnUndeclaredNameIsIgnored(t *testing.T) {
	m := newTestMonitor(t, "atsos")

	if recovered, _ := m.ping("intruder", start.Add(time.Minute)); recovered {
		t.Error("an undeclared name reported a recovery")
	}
	if got := m.names(); len(got) != 1 || got[0] != "atsos" {
		t.Errorf("monitors = %v, want only the declared one", got)
	}
}

// Restarting the watchdog must neither forget an outstanding alert nor repeat
// it, and must not resurrect a monitor that is no longer declared.
func TestStateSurvivesRestart(t *testing.T) {
	m := newTestMonitor(t, "atsos", "nas")
	m.ping("nas", start.Add(time.Minute))
	if due := m.check(start.Add(20 * time.Minute)); len(due) != 2 {
		t.Fatalf("expected both to alert, got %+v", due)
	}
	m.save()

	restarted, err := newMonitor([]string{"atsos"}, 15*time.Minute, m.statePath, start.Add(21*time.Minute), quiet())
	if err != nil {
		t.Fatalf("newMonitor after restart: %v", err)
	}
	if got := restarted.names(); len(got) != 1 || got[0] != "atsos" {
		t.Fatalf("monitors after restart = %v, want only atsos: nas was dropped from the flags", got)
	}
	if due := restarted.check(start.Add(25 * time.Minute)); len(due) != 0 {
		t.Errorf("a restart re-alerted about an outage already sent: %+v", due)
	}

	recovered, _ := restarted.ping("atsos", start.Add(26*time.Minute))
	if !recovered {
		t.Error("the outstanding alert was lost across the restart")
	}
}

// A newly declared monitor joins an existing state file without disturbing
// the others, which is what adding a machine looks like in practice.
func TestNewNameJoinsAnExistingStateFile(t *testing.T) {
	m := newTestMonitor(t, "atsos")
	m.ping("atsos", start.Add(time.Minute))
	m.save()

	later := start.Add(10 * time.Minute)
	grown, err := newMonitor([]string{"atsos", "nas"}, 15*time.Minute, m.statePath, later, quiet())
	if err != nil {
		t.Fatalf("newMonitor: %v", err)
	}

	lines := grown.report(later)
	if len(lines) != 2 {
		t.Fatalf("report has %d lines, want 2", len(lines))
	}
	if !lines[0].LastSeen.Equal(start.Add(time.Minute)) {
		t.Errorf("atsos last seen = %s, want the stored value", lines[0].LastSeen)
	}
	if !lines[1].LastSeen.Equal(later) {
		t.Errorf("nas last seen = %s, want it armed at start-up", lines[1].LastSeen)
	}
}

func TestLoadRejectsACorruptStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// Starting fresh from an unreadable file would re-alert about outages
	// already reported, so it has to be refused loudly.
	if _, err := newMonitor([]string{"atsos"}, time.Minute, path, start, quiet()); err == nil {
		t.Fatal("newMonitor accepted a corrupt state file")
	}
}

func TestNewMonitorRequiresAtLeastOneName(t *testing.T) {
	if _, err := newMonitor(nil, time.Minute, filepath.Join(t.TempDir(), "s.json"), start, quiet()); err == nil {
		t.Fatal("newMonitor accepted an empty list")
	}
}

func TestSaveRoundTrip(t *testing.T) {
	m := newTestMonitor(t, "atsos")
	m.ping("atsos", start.Add(3*time.Minute))
	m.save()

	reloaded, err := newMonitor([]string{"atsos"}, 15*time.Minute, m.statePath, start.Add(time.Hour), quiet())
	if err != nil {
		t.Fatalf("newMonitor: %v", err)
	}
	if got := reloaded.report(start.Add(3 * time.Minute))[0]; !got.LastSeen.Equal(start.Add(3 * time.Minute)) {
		t.Errorf("last seen = %s, want the persisted value", got.LastSeen)
	}
}
