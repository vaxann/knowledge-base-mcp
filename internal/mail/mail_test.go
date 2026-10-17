package mail

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResendRequest(t *testing.T) {
	var got *http.Request
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"49a3999c"}`))
	}))
	defer srv.Close()
	r := &Resend{APIKey: "re_test", From: "KB <login@mail.example.com>", UserAgent: "knowledge-base-mcp/test", BaseURL: srv.URL}
	m := CodeMessage("ann@example.com", "123456", 10*time.Minute, "req-1/1")
	if err := r.Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPost || got.URL.Path != "/emails" {
		t.Errorf("request = %s %s", got.Method, got.URL.Path)
	}
	for h, want := range map[string]string{
		"Authorization":   "Bearer re_test",
		"Content-Type":    "application/json",
		"User-Agent":      "knowledge-base-mcp/test",
		"Idempotency-Key": "req-1/1",
	} {
		if got.Header.Get(h) != want {
			t.Errorf("%s = %q, want %q", h, got.Header.Get(h), want)
		}
	}
	if body["from"] != "KB <login@mail.example.com>" || body["subject"] != m.Subject {
		t.Errorf("body = %v", body)
	}
	if to, _ := body["to"].([]any); len(to) != 1 || to[0] != "ann@example.com" {
		t.Errorf("to = %v", body["to"])
	}
	for _, field := range []string{"text", "html"} {
		if s, _ := body[field].(string); !strings.Contains(s, "123456") {
			t.Errorf("%s must contain the code", field)
		}
	}
}

func TestResendDefaultUserAgent(t *testing.T) {
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ua = r.Header.Get("User-Agent") }))
	defer srv.Close()
	r := &Resend{APIKey: "k", From: "a@example.com", BaseURL: srv.URL}
	if err := r.Send(context.Background(), Message{To: "b@example.com"}); err != nil {
		t.Fatal(err)
	}
	if ua == "" || strings.HasPrefix(ua, "Go-http-client") {
		t.Errorf("a User-Agent of our own is mandatory, got %q", ua)
	}
}

func TestResendErrors(t *testing.T) {
	cases := []struct {
		status    int
		body      string
		temporary bool
		want      string
	}{
		{401, `{"statusCode":401,"name":"missing_api_key","message":"Missing API key"}`, false, "missing_api_key"},
		{403, `{"statusCode":403,"name":"validation_error","message":"You can only send testing emails to your own email address"}`, false, "testing emails"},
		{429, `{"statusCode":429,"name":"rate_limit_exceeded","message":"Too many requests"}`, true, "rate_limit_exceeded"},
		{500, `<html>oops</html>`, true, "500"},
		{503, ``, true, "503"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		r := &Resend{APIKey: "re_test", From: "a@example.com", BaseURL: srv.URL}
		err := r.Send(context.Background(), CodeMessage("b@example.com", "654321", 10*time.Minute, ""))
		srv.Close()
		var me *Error
		if !errors.As(err, &me) || me.Status != c.status {
			t.Fatalf("status %d: err = %v", c.status, err)
		}
		if me.Temporary() != c.temporary {
			t.Errorf("status %d: Temporary() = %v", c.status, me.Temporary())
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("status %d: %q does not mention %q", c.status, err, c.want)
		}
		if strings.Contains(err.Error(), "654321") || strings.Contains(err.Error(), "re_test") {
			t.Errorf("status %d: error leaks the code or the key: %q", c.status, err)
		}
	}
}

func TestResendTransportErrorHidesCode(t *testing.T) {
	r := &Resend{APIKey: "re_test", From: "a@example.com", BaseURL: "http://127.0.0.1:1", Client: &http.Client{Timeout: time.Second}}
	err := r.Send(context.Background(), CodeMessage("b@example.com", "654321", 10*time.Minute, ""))
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), "654321") || strings.Contains(err.Error(), "re_test") {
		t.Errorf("error leaks the code or the key: %q", err)
	}
}

func TestResendNeedsConfiguration(t *testing.T) {
	if err := (&Resend{}).Send(context.Background(), Message{To: "a@example.com"}); err == nil {
		t.Error("a sender without key and address must fail")
	}
}

func TestCodeMessageHasNoLink(t *testing.T) {
	m := CodeMessage("ann@example.com", "123456", 10*time.Minute, "k")
	for _, s := range []string{m.Subject, m.Text, m.HTML} {
		l := strings.ToLower(s)
		if strings.Contains(l, "http://") || strings.Contains(l, "https://") || strings.Contains(l, "<a ") || strings.Contains(l, "href") {
			t.Errorf("the sign-in email must not contain a link: %q", s)
		}
	}
	if strings.Contains(m.Subject, "123456") {
		t.Error("the code does not belong in the subject")
	}
	if !strings.Contains(m.Text, "10 minutes") {
		t.Error("the email must state the lifetime of the code")
	}
}
