// Package mail delivers the sign-in emails of a multi-user server. The only
// transport is the Resend HTTP API, called with net/http.
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"
)

// Message is one email.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
	// IdempotencyKey lets the provider drop a duplicate when a request is
	// retried. Optional.
	IdempotencyKey string
}

// Sender delivers a message.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// Error is a rejection by the mail provider. It carries the provider's own
// explanation and never any part of the message.
type Error struct {
	Status  int
	Name    string
	Message string
}

func (e *Error) Error() string {
	if e.Name == "" && e.Message == "" {
		return fmt.Sprintf("mail provider answered %d", e.Status)
	}
	return fmt.Sprintf("mail provider answered %d %s: %s", e.Status, e.Name, e.Message)
}

// Temporary reports whether retrying later may succeed (rate limit or a
// provider-side failure).
func (e *Error) Temporary() bool { return e.Status == http.StatusTooManyRequests || e.Status >= 500 }

// DefaultResendURL is the provider's API base.
const DefaultResendURL = "https://api.resend.com"

// Resend sends through https://resend.com.
type Resend struct {
	APIKey string
	// From is the sender, e.g. "KB <login@mail.example.com>".
	From string
	// UserAgent is mandatory: the API rejects requests without one.
	UserAgent string
	// BaseURL overrides the API base (tests).
	BaseURL string
	Client  *http.Client
}

type resendBody struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	Text    string   `json:"text,omitempty"`
	HTML    string   `json:"html,omitempty"`
}

// Send posts one message.
func (r *Resend) Send(ctx context.Context, m Message) error {
	if r.APIKey == "" || r.From == "" {
		return errors.New("mail: API key and sender are required")
	}
	body, err := json.Marshal(resendBody{From: r.From, To: []string{m.To}, Subject: m.Subject, Text: m.Text, HTML: m.HTML})
	if err != nil {
		return err
	}
	base := r.BaseURL
	if base == "" {
		base = DefaultResendURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/emails", bytes.NewReader(body))
	if err != nil {
		return err
	}
	ua := r.UserAgent
	if ua == "" {
		ua = "knowledge-base-mcp"
	}
	req.Header.Set("Authorization", "Bearer "+r.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	if m.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", m.IdempotencyKey)
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		// A transport error may quote the URL but never the body.
		return fmt.Errorf("mail: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	e := &Error{Status: resp.StatusCode}
	var payload struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &payload) == nil {
		e.Name, e.Message = payload.Name, payload.Message
	}
	return e
}

// CodeMessage builds the sign-in email. It carries the code and its lifetime
// and deliberately no link and no text chosen by the requesting application.
func CodeMessage(to, code string, ttl time.Duration, idempotencyKey string) Message {
	minutes := int(ttl / time.Minute)
	text := fmt.Sprintf("Your sign-in code is %s\n\nIt is valid for %d minutes and can be used once.\n\n"+
		"If you did not try to sign in to the knowledge base, ignore this message: nobody can sign in without this code.\n", code, minutes)
	htmlBody := fmt.Sprintf(`<div style="font-family:system-ui,sans-serif;font-size:16px;color:#222">`+
		`<p>Your sign-in code is</p>`+
		`<p style="font-family:ui-monospace,monospace;font-size:32px;letter-spacing:6px;font-weight:600;margin:16px 0">%s</p>`+
		`<p>It is valid for %d minutes and can be used once.</p>`+
		`<p style="color:#666;font-size:14px">If you did not try to sign in to the knowledge base, ignore this message: nobody can sign in without this code.</p></div>`,
		html.EscapeString(code), minutes)
	return Message{To: to, Subject: "Your knowledge base sign-in code", Text: text, HTML: htmlBody, IdempotencyKey: idempotencyKey}
}
