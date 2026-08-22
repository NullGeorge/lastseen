package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// notifier is the outbound half: everything this program says, it says here.
type notifier func(ctx context.Context, text string) error

// newMux is the entire inbound surface. One path per declared monitor plus a
// status page, all behind the same secret segment. Registering the names
// explicitly means an unknown one is a 404 from the router itself, with no
// code to get wrong.
func newMux(ctx context.Context, token string, watches *monitor, notify notifier) *http.ServeMux {
	mux := http.NewServeMux()

	for _, name := range watches.names() {
		mux.HandleFunc("/ping/"+token+"/"+name, pingHandler(ctx, name, watches, notify))
	}
	mux.HandleFunc("/status/"+token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, statusPage(watches, time.Now().UTC()))
	})
	return mux
}

func pingHandler(ctx context.Context, name string, watches *monitor, notify notifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recovered, silentFor := watches.ping(name, time.Now().UTC())
		watches.save()
		w.Write([]byte("ok\n"))

		if !recovered {
			return
		}
		text := fmt.Sprintf("%s is back\nHeartbeat resumed after %s of silence",
			name, silentFor.Round(time.Second))
		if err := notify(ctx, text); err != nil {
			watches.log.Printf("send recovery for %s: %v", name, err)
		}
	}
}

// statusPage answers "which of them is alive right now" in one glance, which
// is the question you have at 3am and cannot answer from the alert history.
func statusPage(watches *monitor, now time.Time) string {
	var page strings.Builder
	for _, line := range watches.report(now) {
		state := "ok"
		if line.Silent {
			state = "SILENT"
		}
		fmt.Fprintf(&page, "%-24s %-7s last seen %s ago (%s)\n",
			line.Name, state, line.SinceSeen.Round(time.Second),
			line.LastSeen.Format(time.RFC3339))
	}
	return page.String()
}

// sweep reports every monitor that has just gone quiet, one message each.
func sweep(ctx context.Context, watches *monitor, notify notifier, now time.Time) {
	due := watches.check(now)
	if len(due) == 0 {
		return
	}
	watches.save()

	for _, gone := range due {
		text := fmt.Sprintf("%s is silent\nNo heartbeat for %s, tolerated %s",
			gone.Name, gone.SilentFor.Round(time.Second), watches.grace)
		if err := notify(ctx, text); err != nil {
			watches.log.Printf("send alert for %s: %v", gone.Name, err)
		}
	}
}

func watchLoop(ctx context.Context, watches *monitor, notify notifier, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		sweep(ctx, watches, notify, time.Now().UTC())
	}
}
