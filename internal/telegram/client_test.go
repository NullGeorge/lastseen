package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const testToken = "123456:AAHfake-token-value"

func TestSendMessage(t *testing.T) {
	var gotPath string
	var gotForm url.Values

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		gotForm = r.PostForm
		w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	defer server.Close()

	if err := New(testToken, server.URL).SendMessage(context.Background(), -100123, "atsos is silent"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if want := "/bot" + testToken + "/sendMessage"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if got := gotForm.Get("chat_id"); got != "-100123" {
		t.Errorf("chat_id = %q", got)
	}
	if got := gotForm.Get("text"); got != "atsos is silent" {
		t.Errorf("text = %q", got)
	}
}

// The Bot API counts characters, not bytes, so a byte slice would overshoot
// the limit and could split a rune.
func TestSendMessageTruncatesOnRuneBoundary(t *testing.T) {
	var gotText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotText = r.PostForm.Get("text")
		w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer server.Close()

	if err := New(testToken, server.URL).SendMessage(context.Background(), 1, strings.Repeat("я", 5000)); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	runes := []rune(gotText)
	if len(runes) != messageLimit {
		t.Errorf("sent %d runes, want %d", len(runes), messageLimit)
	}
	if runes[len(runes)-1] != '\u2026' {
		t.Errorf("last rune = %q, want the ellipsis", runes[len(runes)-1])
	}
	if strings.ContainsRune(gotText, '\uFFFD') {
		t.Error("truncation split a rune")
	}
}

func TestSendMessageReportsApiErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
	}))
	defer server.Close()

	err := New(testToken, server.URL).SendMessage(context.Background(), 1, "hi")
	if err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("error = %v, want the api description", err)
	}
}

// A proxy or captive portal answers with HTML. That must be reported as
// unreadable, never mistaken for a delivered alert.
func TestSendMessageRejectsANonJsonResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("<html>502</html>"))
	}))
	defer server.Close()

	err := New(testToken, server.URL).SendMessage(context.Background(), 1, "hi")
	if err == nil {
		t.Fatal("an HTML body was accepted")
	}
	for _, want := range []string{"502", "unreadable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}

// The token sits in the request path, so transport errors quote it. Nothing
// that reaches a log may contain it.
func TestErrorsNeverLeakTheToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()

	err := New(testToken, server.URL).SendMessage(context.Background(), 1, "hi")
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token leaked into %q", err)
	}
	if !strings.Contains(err.Error(), "REDACTED") {
		t.Errorf("error = %v, want the token replaced", err)
	}
}

func TestRedactLeavesTokenlessErrorsAlone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()

	err := New("", server.URL).SendMessage(context.Background(), 1, "hi")
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), "REDACTED") {
		t.Errorf("error = %v, want it untouched when there is no token", err)
	}
}

func TestNewDefaultsAndTrimsTheBaseURL(t *testing.T) {
	if got := New("t", "").baseURL; got != DefaultBaseURL {
		t.Errorf("empty base url = %q, want %q", got, DefaultBaseURL)
	}
	if got := New("t", "http://stub:8080/").baseURL; got != "http://stub:8080" {
		t.Errorf("base url = %q, want the trailing slash removed", got)
	}
}

func TestSendMessageHonoursTheContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := New(testToken, server.URL).SendMessage(ctx, 1, "hi"); err == nil {
		t.Fatal("succeeded with a cancelled context")
	}
}
