package main

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NullGeorge/lastseen/internal/telegram"
)

// The teens are where a naive n%10 implementation gives itself away: 11 takes
// the same form as 5, not the one 1 takes.
func TestPluralFollowsRussianAgreement(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "минут"},
		{1, "минуту"},
		{2, "минуты"},
		{4, "минуты"},
		{5, "минут"},
		{10, "минут"},
		{11, "минут"},
		{12, "минут"},
		{14, "минут"},
		{15, "минут"},
		{20, "минут"},
		{21, "минуту"},
		{22, "минуты"},
		{24, "минуты"},
		{25, "минут"},
		{100, "минут"},
		{101, "минуту"},
		{102, "минуты"},
		{111, "минут"},
		{112, "минут"},
		{121, "минуту"},
	}
	for _, testCase := range cases {
		if got := plural(testCase.n, "минуту", "минуты", "минут"); got != testCase.want {
			t.Errorf("plural(%d) = %q, want %q", testCase.n, got, testCase.want)
		}
	}
}

// Every unit has to agree too, and the gap has to be reported in the largest
// unit that still means something: 4000 minutes tells nobody anything.
func TestAgoPicksUnitAndAgrees(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0 секунд назад"},
		{time.Second, "1 секунду назад"},
		{3 * time.Second, "3 секунды назад"},
		{43 * time.Second, "43 секунды назад"},
		{59*time.Second + 999*time.Millisecond, "59 секунд назад"},
		{time.Minute, "1 минуту назад"},
		{2 * time.Minute, "2 минуты назад"},
		{12*time.Minute + 59*time.Second, "12 минут назад"},
		{21 * time.Minute, "21 минуту назад"},
		{59 * time.Minute, "59 минут назад"},
		{time.Hour, "1 час назад"},
		{2 * time.Hour, "2 часа назад"},
		{5 * time.Hour, "5 часов назад"},
		{21 * time.Hour, "21 час назад"},
		{24 * time.Hour, "1 день назад"},
		{50 * time.Hour, "2 дня назад"},
		{5 * 24 * time.Hour, "5 дней назад"},
		{11 * 24 * time.Hour, "11 дней назад"},
		{21 * 24 * time.Hour, "21 день назад"},
		// A clock that jumped backwards must not print a negative age.
		{-time.Minute, "0 секунд назад"},
	}
	for _, testCase := range cases {
		if got := ago(testCase.in); got != testCase.want {
			t.Errorf("ago(%s) = %q, want %q", testCase.in, got, testCase.want)
		}
	}
}

// The time is read in Moscow whatever the host thinks its zone is, and the
// date appears only when the moment was not today.
func TestClockIsMoscowAndDatesOnlyOlderDays(t *testing.T) {
	now := time.Date(2026, 8, 23, 9, 40, 0, 0, time.UTC) // 12:40 MSK
	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{"same day, utc input", time.Date(2026, 8, 23, 9, 34, 0, 0, time.UTC), "12:34"},
		{"same day, another zone", time.Date(2026, 8, 23, 4, 34, 0, 0, time.FixedZone("EST", -5*3600)), "12:34"},
		{"earlier today, before msk midnight offset", time.Date(2026, 8, 23, 0, 5, 0, 0, time.UTC), "03:05"},
		{"yesterday in moscow", time.Date(2026, 8, 22, 20, 15, 0, 0, time.UTC), "22.08 23:15"},
		{"last month", time.Date(2026, 7, 2, 6, 0, 0, 0, time.UTC), "02.07 09:00"},
		// 21:05 UTC is already the next day in Moscow, so it is not "today".
		{"tonight utc is tomorrow in moscow", time.Date(2026, 8, 23, 21, 5, 0, 0, time.UTC), "24.08 00:05"},
	}
	for _, testCase := range cases {
		if got := clock(testCase.at, now); got != testCase.want {
			t.Errorf("%s: clock = %q, want %q", testCase.name, got, testCase.want)
		}
	}
}

func TestStatusTextRendersEveryMonitor(t *testing.T) {
	now := time.Date(2026, 8, 23, 9, 40, 0, 0, time.UTC)
	lines := []report{
		{Name: "atsos", LastSeen: now.Add(-12 * time.Minute), SinceSeen: 12 * time.Minute},
		{Name: "nas", LastSeen: now.Add(-26 * time.Hour), SinceSeen: 26 * time.Hour, Silent: true},
	}

	want := "atsos: 12 минут назад (12:28)\nnas: 1 день назад (22.08 10:40) — тишина"
	if got := statusText(lines, now); got != want {
		t.Errorf("statusText =\n%q\nwant\n%q", got, want)
	}
}

// fakeCommandAPI scripts the long poll. poll receives the 1-based call number
// and the offset asked for, which is what proves a handled command is never
// answered twice.
type fakeCommandAPI struct {
	poll    func(call int, offset int64) ([]telegram.Update, error)
	sendErr error

	mu      sync.Mutex
	offsets []int64
	sent    []string
}

func (f *fakeCommandAPI) GetUpdates(ctx context.Context, offset int64, _ time.Duration) ([]telegram.Update, error) {
	f.mu.Lock()
	f.offsets = append(f.offsets, offset)
	call := len(f.offsets)
	f.mu.Unlock()
	if f.poll == nil {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.poll(call, offset)
}

func (f *fakeCommandAPI) SendMessage(_ context.Context, _ int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	return f.sendErr
}

func (f *fakeCommandAPI) seen() ([]int64, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.offsets...), append([]string(nil), f.sent...)
}

func testMonitor(t *testing.T, names ...string) *monitor {
	t.Helper()
	watches, err := newMonitor(names, time.Minute,
		filepath.Join(t.TempDir(), "state.json"), time.Now().UTC(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("new monitor: %v", err)
	}
	return watches
}

const ownerChat = 4242

// A stranger must get no reply at all, and must not move the offset for the
// owner: the whole authorisation boundary is that one comparison.
func TestCommandLoopAnswersOnlyTheOwner(t *testing.T) {
	watches := testMonitor(t, "atsos")
	ctx, cancel := context.WithCancel(context.Background())
	api := &fakeCommandAPI{}
	api.poll = func(call int, _ int64) ([]telegram.Update, error) {
		switch call {
		case 1:
			return []telegram.Update{{UpdateID: 7, Message: &telegram.Message{
				Chat: telegram.Chat{ID: 999}, Text: "/status"}}}, nil
		case 2:
			return []telegram.Update{{UpdateID: 8, Message: &telegram.Message{
				Chat: telegram.Chat{ID: ownerChat}, Text: "/status"}}}, nil
		default:
			cancel()
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}

	commandLoop(ctx, api, watches, ownerChat, log.New(io.Discard, "", 0))

	offsets, sent := api.seen()
	if len(sent) != 1 {
		t.Fatalf("replies = %v, want exactly the owner's", sent)
	}
	if !strings.HasPrefix(sent[0], "atsos: ") {
		t.Errorf("reply = %q, want a status line for atsos", sent[0])
	}
	// 0, then past the stranger's update, then past the owner's.
	if want := []int64{0, 8, 9}; len(offsets) < 3 || offsets[0] != want[0] ||
		offsets[1] != want[1] || offsets[2] != want[2] {
		t.Errorf("offsets = %v, want to start %v", offsets, want)
	}
}

// A failed poll is logged and retried rather than ending the loop: the command
// channel going deaf must not be silent about it.
func TestCommandLoopSurvivesAFailedPoll(t *testing.T) {
	pollRetryDelay = time.Millisecond
	t.Cleanup(func() { pollRetryDelay = 5 * time.Second })

	watches := testMonitor(t, "atsos")
	ctx, cancel := context.WithCancel(context.Background())
	var logs strings.Builder
	api := &fakeCommandAPI{}
	api.poll = func(call int, _ int64) ([]telegram.Update, error) {
		switch call {
		case 1:
			return nil, errors.New("telegram: 502 bad gateway")
		case 2:
			return []telegram.Update{{UpdateID: 1, Message: &telegram.Message{
				Chat: telegram.Chat{ID: ownerChat}, Text: "/help"}}}, nil
		default:
			cancel()
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}

	commandLoop(ctx, api, watches, ownerChat, log.New(&logs, "", 0))

	if _, sent := api.seen(); len(sent) != 1 || sent[0] != commandHelp {
		t.Errorf("replies = %v, want the help text after the retry", sent)
	}
	if !strings.Contains(logs.String(), "502 bad gateway") {
		t.Errorf("log = %q, want the failed poll in it", logs.String())
	}
}

func TestAnswerRejectsWhatItDoesNotKnow(t *testing.T) {
	watches := testMonitor(t, "atsos")
	now := time.Now().UTC()

	if reply := answer("", watches, now); reply != "" {
		t.Errorf("empty text produced %q, want silence", reply)
	}
	if reply := answer("/status@lastseen_bot", watches, now); !strings.HasPrefix(reply, "atsos: ") {
		t.Errorf("suffixed command = %q, want it handled", reply)
	}
	if reply := answer("/wat", watches, now); !strings.Contains(reply, "неизвестная команда") {
		t.Errorf("unknown command = %q", reply)
	}
	// Nothing here may change state: there is no write command on offer.
	if reply := answer("/ping atsos", watches, now); !strings.Contains(reply, "неизвестная команда") {
		t.Errorf("/ping = %q, want it refused", reply)
	}
}

// Telegram rate limits and goes down. A reply that could not be delivered is
// logged and the loop carries on: giving up would leave the watchdog unable to
// answer for the rest of its life over one 429.
func TestCommandLoopSurvivesAFailedReply(t *testing.T) {
	watches := testMonitor(t, "atsos")
	ctx, cancel := context.WithCancel(context.Background())
	var logs strings.Builder
	api := &fakeCommandAPI{sendErr: errors.New("telegram: 429 too many requests")}
	api.poll = func(call int, _ int64) ([]telegram.Update, error) {
		switch call {
		case 1, 2:
			return []telegram.Update{{UpdateID: int64(call), Message: &telegram.Message{
				Chat: telegram.Chat{ID: ownerChat}, Text: "/status"}}}, nil
		default:
			cancel()
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}

	commandLoop(ctx, api, watches, ownerChat, log.New(&logs, "", 0))

	if _, sent := api.seen(); len(sent) != 2 {
		t.Errorf("attempted %d replies, want both to have been tried", len(sent))
	}
	if !strings.Contains(logs.String(), "429 too many requests") {
		t.Errorf("log = %q, want the failed reply in it", logs.String())
	}
}
