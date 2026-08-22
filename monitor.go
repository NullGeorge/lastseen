package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// watch is one thing being watched: when it last checked in, and whether its
// silence has already been reported.
type watch struct {
	LastSeen time.Time `json:"last_seen"`
	Alerted  bool      `json:"alerted"`
}

// monitor is the dead-man switch for every declared name. The decisions live
// here, apart from HTTP and Telegram, so they can be tested without a clock,
// a socket or a bot token.
type monitor struct {
	mu        sync.Mutex
	grace     time.Duration
	statePath string
	watches   map[string]*watch
	log       *log.Logger
}

// newMonitor arms every declared name at start. A name that has never checked
// in still gets reported after one grace period: a client that died before its
// first ping is exactly the case a watchdog must not miss.
func newMonitor(names []string, grace time.Duration, statePath string, now time.Time, logger *log.Logger) (*monitor, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("no monitors declared")
	}
	if logger == nil {
		logger = log.Default()
	}

	m := &monitor{
		grace:     grace,
		statePath: statePath,
		watches:   make(map[string]*watch, len(names)),
		log:       logger,
	}
	stored, err := m.load()
	if err != nil {
		return nil, err
	}

	for _, name := range names {
		if previous, ok := stored[name]; ok {
			m.watches[name] = previous
			continue
		}
		m.watches[name] = &watch{LastSeen: now}
	}
	return m, nil
}

// names returns the declared monitors in a stable order, which keeps the
// status page and the tests from depending on map iteration.
func (m *monitor) names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	ordered := make([]string, 0, len(m.watches))
	for name := range m.watches {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	return ordered
}

// ping records a check-in. It reports whether this ended an outstanding alert
// and how long the silence lasted, so the caller can say so out loud.
func (m *monitor) ping(name string, now time.Time) (recovered bool, silentFor time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, ok := m.watches[name]
	if !ok {
		return false, 0
	}
	if state.Alerted {
		recovered, silentFor = true, now.Sub(state.LastSeen)
		state.Alerted = false
	}
	state.LastSeen = now
	return recovered, silentFor
}

// overdue is one monitor whose silence has just crossed the grace period.
type overdue struct {
	Name      string
	SilentFor time.Duration
}

// check reports the monitors that have just gone quiet. Each outage is
// reported exactly once: an alert that repeats every tick is one you learn to
// ignore, and then it is worth nothing when it matters.
func (m *monitor) check(now time.Time) []overdue {
	m.mu.Lock()
	defer m.mu.Unlock()

	var due []overdue
	for _, name := range m.sortedLocked() {
		state := m.watches[name]
		silentFor := now.Sub(state.LastSeen)
		if state.Alerted || silentFor <= m.grace {
			continue
		}
		state.Alerted = true
		due = append(due, overdue{Name: name, SilentFor: silentFor})
	}
	return due
}

// report is one line per monitor for the status page.
type report struct {
	Name      string
	Silent    bool
	LastSeen  time.Time
	SinceSeen time.Duration
}

func (m *monitor) report(now time.Time) []report {
	m.mu.Lock()
	defer m.mu.Unlock()

	lines := make([]report, 0, len(m.watches))
	for _, name := range m.sortedLocked() {
		state := m.watches[name]
		lines = append(lines, report{
			Name:      name,
			Silent:    now.Sub(state.LastSeen) > m.grace,
			LastSeen:  state.LastSeen,
			SinceSeen: now.Sub(state.LastSeen),
		})
	}
	return lines
}

func (m *monitor) sortedLocked() []string {
	ordered := make([]string, 0, len(m.watches))
	for name := range m.watches {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	return ordered
}

// load reads the persisted map. A missing file is a first start, not a
// failure; anything else is refused rather than silently treated as empty,
// because starting from scratch would re-alert about outages already sent.
func (m *monitor) load() (map[string]*watch, error) {
	raw, err := os.ReadFile(m.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]*watch{}, nil
		}
		return nil, fmt.Errorf("read state: %w", err)
	}

	stored := map[string]*watch{}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", m.statePath, err)
	}
	return stored, nil
}

func (m *monitor) save() {
	m.mu.Lock()
	raw, err := json.Marshal(m.watches)
	m.mu.Unlock()
	if err != nil {
		m.log.Printf("encode state: %v", err)
		return
	}

	if dir := filepath.Dir(m.statePath); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			m.log.Printf("create state directory: %v", err)
			return
		}
	}
	if err := os.WriteFile(m.statePath, raw, 0o600); err != nil {
		m.log.Printf("write state: %v", err)
	}
}
