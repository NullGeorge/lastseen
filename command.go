package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/NullGeorge/lastseen/internal/telegram"
)

// pollWindow is how long one getUpdates call may hang open.
const pollWindow = 25 * time.Second

// pollRetryDelay is the pause after a failed poll, a variable so tests do not
// have to sit through it.
var pollRetryDelay = 5 * time.Second

// commandAPI is the slice of the Bot API the command loop needs.
type commandAPI interface {
	GetUpdates(ctx context.Context, offset int64, wait time.Duration) ([]telegram.Update, error)
	SendMessage(ctx context.Context, chatID int64, text string) error
}

// moscow is the zone the answer is written in. Fixed rather than the host's
// local time: the watchdog may be moved to another continent, and the reading
// must not change when it is.
var moscow = time.FixedZone("MSK", 3*60*60)

// commandLoop answers the question a watchdog gets asked out loud: when did
// each thing last check in. Silence means everything is fine, which is exactly
// why it is impossible to tell working from dead without asking.
//
// Only the alert chat is served, and only reads are on offer: there is no
// command here that changes anything. An unknown chat gets no reply at all,
// so probing the bot teaches nothing about who owns it.
func commandLoop(ctx context.Context, api commandAPI, watches *monitor, chatID int64, logger *log.Logger) {
	if logger == nil {
		logger = log.Default()
	}
	var offset int64

	for ctx.Err() == nil {
		updates, err := api.GetUpdates(ctx, offset, pollWindow)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Printf("commands: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollRetryDelay):
			}
			continue
		}

		for _, update := range updates {
			offset = update.UpdateID + 1
			if update.Message == nil || update.Message.Chat.ID != chatID {
				continue
			}
			reply := answer(update.Message.Text, watches, time.Now().UTC())
			if reply == "" {
				continue
			}
			if err := api.SendMessage(ctx, chatID, reply); err != nil {
				logger.Printf("commands: reply: %v", err)
			}
		}
	}
}

const commandHelp = "/status — когда каждый последний раз отзывался"

// answer maps one message to a reply. An empty string means keep quiet.
func answer(text string, watches *monitor, now time.Time) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	command := strings.ToLower(fields[0])
	// Telegram appends @botname when several bots share a group.
	if at := strings.IndexByte(command, '@'); at >= 0 {
		command = command[:at]
	}

	switch command {
	case "/status":
		return statusText(watches.report(now), now)
	case "/help", "/start":
		return commandHelp
	default:
		return "неизвестная команда. " + commandHelp
	}
}

// statusText renders one line per monitor: how long ago it was last heard from
// and at what time, both as they would be said aloud.
func statusText(lines []report, now time.Time) string {
	var text strings.Builder
	for i, line := range lines {
		if i > 0 {
			text.WriteString("\n")
		}
		fmt.Fprintf(&text, "%s: %s (%s)", line.Name, ago(line.SinceSeen), clock(line.LastSeen, now))
		// Past the grace period this is not a report but an outstanding
		// alarm, and the reader must not have to compare numbers to see it.
		if line.Silent {
			text.WriteString(" — тишина")
		}
	}
	return text.String()
}

// clock prints the moment itself, to the second. The date is added only when
// it is not today, because "12:34:56" is unambiguous for today and misleading
// for last Tuesday.
//
// Seconds are not decoration: two things on the same schedule land in the same
// minute, and a reply that stops at the minute reads as one check-in when it
// was really two.
func clock(at, now time.Time) string {
	local := at.In(moscow)
	if today := now.In(moscow); local.YearDay() == today.YearDay() && local.Year() == today.Year() {
		return local.Format("15:04:05")
	}
	return local.Format("02.01 15:04:05")
}

// ago renders a gap in the largest unit that still says something useful.
// Minutes stop being informative after a day, and seconds after a minute.
func ago(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		n := int(d.Seconds())
		return fmt.Sprintf("%d %s назад", n, plural(n, "секунду", "секунды", "секунд"))
	case d < time.Hour:
		n := int(d.Minutes())
		return fmt.Sprintf("%d %s назад", n, plural(n, "минуту", "минуты", "минут"))
	case d < 24*time.Hour:
		n := int(d.Hours())
		return fmt.Sprintf("%d %s назад", n, plural(n, "час", "часа", "часов"))
	default:
		n := int(d.Hours() / 24)
		return fmt.Sprintf("%d %s назад", n, plural(n, "день", "дня", "дней"))
	}
}

// plural picks the Russian form for n: 1 минуту, 2 минуты, 5 минут. The teens
// are the exception that catches naive implementations out — 11 takes the same
// form as 5, not the one 1 takes. n is a count, never negative: ago clamps a
// clock that went backwards before it gets here.
func plural(n int, one, few, many string) string {
	if mod100 := n % 100; mod100 >= 11 && mod100 <= 14 {
		return many
	}
	switch n % 10 {
	case 1:
		return one
	case 2, 3, 4:
		return few
	default:
		return many
	}
}
