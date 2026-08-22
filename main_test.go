package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// Every refusal here is a misconfiguration that would otherwise produce a
// watchdog that looks healthy and guards nothing.
func TestRunRefusesToStartMisconfigured(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	tests := []struct {
		name  string
		args  []string
		token string
		chat  string
		want  string
	}{
		{
			name: "no secret", args: []string{"-monitors", "atsos", "-state", state},
			token: "bot-token", chat: "42", want: "-token is required",
		},
		{
			name: "no monitors", args: []string{"-token", "s", "-state", state},
			token: "bot-token", chat: "42", want: "-monitors is required",
		},
		{
			name: "no bot token", args: []string{"-token", "s", "-monitors", "atsos", "-state", state},
			token: "", chat: "42", want: "BOT_TOKEN is not set",
		},
		{
			name: "no chat", args: []string{"-token", "s", "-monitors", "atsos", "-state", state},
			token: "bot-token", chat: "", want: "is not a chat id",
		},
		{
			name: "unknown flag", args: []string{"-nonsense"},
			token: "bot-token", chat: "42", want: "flag provided but not defined",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BOT_TOKEN", tt.token)
			t.Setenv("CHAT_ID", tt.chat)
			// flag prints its own usage dump on a bad flag; keep it out of the
			// test output.
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

func TestRunRefusesACorruptStateFile(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(state, []byte("{broken"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Setenv("BOT_TOKEN", "bot-token")
	t.Setenv("CHAT_ID", "42")

	err := run([]string{"-token", "s", "-monitors", "atsos", "-state", state})
	if err == nil || !strings.Contains(err.Error(), "parse state") {
		t.Fatalf("error = %v, want a refusal naming the state file", err)
	}
}
