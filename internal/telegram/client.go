// Package telegram is the smallest useful Bot API client: one outbound call.
// This program only ever tells you things, it never takes instructions, so
// getUpdates and the whole command surface are deliberately absent.
package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the public Bot API endpoint. Tests point this elsewhere.
const DefaultBaseURL = "https://api.telegram.org"

// messageLimit is the Bot API ceiling for a single text message.
const messageLimit = 4096

type Client struct {
	token   string
	baseURL string
	http    *http.Client
}

func New(token, baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		token:   token,
		baseURL: strings.TrimSuffix(baseURL, "/"),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

type envelope struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	ErrorCode   int    `json:"error_code"`
}

// SendMessage delivers one alert. Over-long text is truncated rather than
// rejected, because a clipped alert beats a dropped one.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string) error {
	// The Bot API counts characters, not bytes, and a byte slice would both
	// overshoot the limit and risk cutting a rune in half.
	if runes := []rune(text); len(runes) > messageLimit {
		text = string(runes[:messageLimit-1]) + "\u2026"
	}

	form := url.Values{
		"chat_id":                  {strconv.FormatInt(chatID, 10)},
		"text":                     {text},
		"disable_web_page_preview": {"true"},
	}
	endpoint := c.baseURL + "/bot" + c.token + "/sendMessage"

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return c.redact(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := c.http.Do(request)
	if err != nil {
		return c.redact(err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return c.redact(err)
	}

	var parsed envelope
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("sendMessage: http %d: unreadable response", response.StatusCode)
	}
	if !parsed.OK {
		return fmt.Errorf("sendMessage: telegram error %d: %s", parsed.ErrorCode, parsed.Description)
	}
	return nil
}

// redact keeps the bot token out of logs. Transport errors quote the request
// URL, and the token is part of that path.
func (c *Client) redact(err error) error {
	if c.token == "" {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), c.token, "REDACTED"))
}
