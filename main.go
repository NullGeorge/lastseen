// Command lastseen is a dead-man switch. Things check in over HTTP on a
// schedule; when one stops, it says so in Telegram once, and says so again
// when it comes back.
//
// It exists because a machine cannot report its own death. Anything that
// watches itself is blind to the failure that matters most, and silence from
// a healthy system looks exactly like silence from a burnt one.
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
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "run":
		err = run(os.Args[2:])
	case "init":
		err = initConfig(os.Args[2:], os.Stdout)
	case "urls":
		err = showURLs(os.Args[2:], os.Stdout)
	case "add":
		err = addMonitor(os.Args[2:], os.Stdout)
	case "version":
		fmt.Println("lastseen " + version.String())
	default:
		usage()
		os.Exit(1)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Println("usage: lastseen <run|init|urls|add|version>")
	fmt.Println("  init  -monitors a,b [-advertise url] [-listen :9000]  generate a config and print the urls")
	fmt.Println("  urls                                                  print them again")
	fmt.Println("  add   <name>                                          watch one more thing")
	fmt.Println("  run                                                   the daemon itself")
	fmt.Println("all commands take -config, default " + defaultConfigPath)
}

// initConfig writes the config once and refuses to touch an existing one:
// regenerating the secret would silently orphan every client already using it.
func initConfig(args []string, out *os.File) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "where to write the config")
	monitors := fs.String("monitors", "", "comma separated names to watch")
	listen := fs.String("listen", ":9000", "address to listen on")
	advertise := fs.String("advertise", "", "base url clients should use; detected when empty")
	grace := fs.Duration("grace", 15*time.Minute, "silence tolerated before alerting")
	every := fs.Duration("check-interval", 30*time.Second, "how often silence is evaluated")
	state := fs.String("state", "/var/lib/lastseen/state.json", "where last-seen is persisted")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if _, err := os.Stat(*configPath); err == nil {
		return fmt.Errorf("%s already exists: delete it first if you really mean to replace the secret", *configPath)
	}
	names, err := parseMonitors(*monitors)
	if err != nil {
		return err
	}

	base := *advertise
	private := false
	if base == "" {
		if base, private, err = guessAdvertise(*listen); err != nil {
			return err
		}
	}
	if err := validateAdvertise(base); err != nil {
		return err
	}

	secret, err := newSecret()
	if err != nil {
		return err
	}

	cfg := &Config{
		Listen: *listen, Advertise: base, Secret: secret,
		Grace: Duration(*grace), CheckInterval: Duration(*every),
		State: *state, Monitors: names,
	}
	if err := cfg.save(*configPath); err != nil {
		return err
	}

	fmt.Fprintf(out, "wrote %s\n\n", *configPath)
	printURLs(out, cfg)
	if private && *advertise == "" {
		fmt.Fprintf(out, "\nthat address is private, so these urls only work from the same network.\n")
		fmt.Fprintf(out, "pass -advertise to publish something reachable from outside.\n")
	}
	return nil
}

func showURLs(args []string, out *os.File) error {
	fs := flag.NewFlagSet("urls", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config to read")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	printURLs(out, cfg)
	return nil
}

func addMonitor(args []string, out *os.File) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config to update")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: lastseen add <name>")
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	name := fs.Arg(0)
	// Reuse the same rules the daemon applies, so a name accepted here cannot
	// turn out to be unroutable later.
	if _, err := parseMonitors(strings.Join(append(cfg.Monitors, name), ",")); err != nil {
		return err
	}

	cfg.Monitors = append(cfg.Monitors, name)
	if err := cfg.save(*configPath); err != nil {
		return err
	}
	fmt.Fprintf(out, "added %s, restart lastseen to start watching it\n\n", name)
	printURLs(out, cfg)
	return nil
}

func printURLs(out *os.File, cfg *Config) {
	for _, line := range cfg.urls() {
		fmt.Fprintln(out, line)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config to read")
	listen := fs.String("listen", "", "override the configured listen address")
	statePath := fs.String("state", "", "override the configured state path")
	apiURL := fs.String("telegram-api", telegram.DefaultBaseURL, "Bot API base url; override only for testing")
	chatID := fs.Int64("telegram-chat-id", 0, "chat that receives alerts; CHAT_ID otherwise")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *statePath != "" {
		cfg.State = *statePath
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
	watches, err := newMonitor(cfg.Monitors, time.Duration(cfg.Grace), cfg.State, time.Now().UTC(), logger)
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
		Addr:              cfg.Listen,
		Handler:           newMux(ctx, cfg.Secret, watches, notify),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	go watchLoop(ctx, watches, notify, time.Duration(cfg.CheckInterval))

	log.Printf("lastseen %s", version.String())
	log.Printf("listening on %s, alerting after %s of silence from: %s",
		cfg.Listen, time.Duration(cfg.Grace), strings.Join(cfg.Monitors, ", "))
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
