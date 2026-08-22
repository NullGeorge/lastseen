// Command lastseen is a dead-man switch. Things check in over HTTP on a
// schedule; when one stops, it says so in Telegram once, and says so again
// when it comes back.
//
// It exists because a machine cannot report its own death. Everything that
// monitors itself is blind to the one failure that matters most, and silence
// from a healthy system looks exactly like silence from a burnt one.
//
// Nothing is ever sent to the things being watched: the only inbound surface
// is one secret path per monitor.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/NullGeorge/lastseen/internal/telegram"
	"github.com/NullGeorge/lastseen/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("lastseen " + version.String())
		return
	}
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("lastseen", flag.ContinueOnError)
	listen := fs.String("listen", ":9000", "address to listen on")
	token := fs.String("token", "", "secret path segment; required")
	monitors := fs.String("monitors", "", "comma separated names to watch, for example atsos,nas,backup")
	grace := fs.Duration("grace", 15*time.Minute, "silence tolerated before alerting")
	every := fs.Duration("check-interval", 30*time.Second, "how often silence is evaluated")
	statePath := fs.String("state", "/var/lib/lastseen/state.json", "where last-seen is persisted")
	chatID := fs.Int64("telegram-chat-id", 0, "chat that receives alerts")
	apiURL := fs.String("telegram-api", telegram.DefaultBaseURL, "Bot API base url; override only for testing")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// An open ping endpoint would let anyone hold the alert down forever,
	// which is precisely the failure this program exists to prevent.
	if *token == "" {
		return fmt.Errorf("-token is required: without it anyone could silence the alert")
	}
	names, err := parseMonitors(*monitors)
	if err != nil {
		return err
	}

	botToken := strings.TrimSpace(os.Getenv("BOT_TOKEN"))
	if botToken == "" {
		return fmt.Errorf("BOT_TOKEN is not set")
	}
	chat, err := resolveChat(*chatID)
	if err != nil {
		return err
	}

	logger := log.Default()
	watches, err := newMonitor(names, *grace, *statePath, time.Now().UTC(), logger)
	if err != nil {
		return err
	}

	api := telegram.New(botToken, *apiURL)
	notify := func(ctx context.Context, text string) error {
		return api.SendMessage(ctx, chat, text)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	server := &http.Server{
		Addr:              *listen,
		Handler:           newMux(ctx, *token, watches, notify),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	go watchLoop(ctx, watches, notify, *every)

	log.Printf("lastseen %s", version.String())
	log.Printf("listening on %s, alerting after %s of silence from: %s",
		*listen, *grace, strings.Join(names, ", "))
	if err := server.ListenAndServe(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// parseMonitors rejects names that would not survive being a URL segment.
// Catching that here beats a monitor that silently can never be pinged.
func parseMonitors(list string) ([]string, error) {
	var names []string
	seen := make(map[string]bool)

	for _, raw := range strings.Split(list, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if strings.ContainsAny(name, "/?#% ") {
			return nil, fmt.Errorf("monitor name %q cannot be part of a url", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("monitor %q is declared twice", name)
		}
		seen[name] = true
		names = append(names, name)
	}

	if len(names) == 0 {
		return nil, fmt.Errorf("-monitors is required, for example -monitors atsos,nas")
	}
	return names, nil
}

func resolveChat(flagValue int64) (int64, error) {
	if flagValue != 0 {
		return flagValue, nil
	}
	raw := strings.TrimSpace(os.Getenv("CHAT_ID"))
	chat, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("CHAT_ID %q is not a chat id: %w", raw, err)
	}
	return chat, nil
}
