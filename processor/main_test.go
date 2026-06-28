package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// parseLine splits a line-protocol string into its measurement+tags prefix,
// the comma-separated fields, and the trailing timestamp. It assumes no
// unescaped spaces inside tag/field values (true for the test inputs below).
func parseLine(t *testing.T, line string) (prefix string, fields map[string]string, ts string) {
	t.Helper()
	parts := strings.Split(line, " ")
	if len(parts) != 3 {
		t.Fatalf("expected 3 space-separated sections, got %d: %q", len(parts), line)
	}
	fields = map[string]string{}
	for _, kv := range strings.Split(parts[1], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("malformed field %q", kv)
		}
		fields[k] = v
	}
	return parts[0], fields, parts[2]
}

func TestToLineProtocolBasic(t *testing.T) {
	raw := []byte(`{"timestamp":"2026-05-01T10:00:00.123Z","level":"INFO","logger":"app",` +
		`"message":"hello","service":"svc","module":"mod","hostname":"h1",` +
		`"pid":42,"function":"main","line":12,"request_id":"abc-123"}`)

	line, err := toLineProtocol(raw, "logs")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	prefix, fields, ts := parseLine(t, line)

	if want := "logs,service=svc,module=mod,level=INFO,hostname=h1"; prefix != want {
		t.Errorf("prefix = %q, want %q", prefix, want)
	}

	wantFields := map[string]string{
		"message":    `"hello"`,
		"logger":     `"app"`,
		"function":   `"main"`,
		"line":       "12i",
		"pid":        "42i",
		"request_id": `"abc-123"`,
	}
	for k, want := range wantFields {
		if got := fields[k]; got != want {
			t.Errorf("field %s = %q, want %q", k, got, want)
		}
	}

	parsed, _ := time.Parse(time.RFC3339Nano, "2026-05-01T10:00:00.123Z")
	if want := strconv.FormatInt(parsed.UnixNano(), 10); ts != want {
		t.Errorf("timestamp = %q, want %q", ts, want)
	}
}

func TestToLineProtocolException(t *testing.T) {
	raw := []byte(`{"timestamp":"2026-05-01T10:00:00.000Z","level":"ERROR","message":"boom",` +
		`"service":"svc","exception":{"type":"ValueError","message":"bad"}}`)

	line, err := toLineProtocol(raw, "logs")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, fields, _ := parseLine(t, line)
	if got := fields["exception_type"]; got != `"ValueError"` {
		t.Errorf("exception_type = %q, want %q", got, `"ValueError"`)
	}
	if got := fields["exception_message"]; got != `"bad"` {
		t.Errorf("exception_message = %q, want %q", got, `"bad"`)
	}
}

func TestToLineProtocolMissingTimestampUsesNow(t *testing.T) {
	raw := []byte(`{"level":"INFO","message":"hi","service":"svc"}`)
	line, err := toLineProtocol(raw, "logs")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, _, ts := parseLine(t, line)
	if _, err := strconv.ParseInt(ts, 10, 64); err != nil {
		t.Errorf("timestamp not numeric: %q", ts)
	}
}

func TestToLineProtocolUnparsableTimestamp(t *testing.T) {
	raw := []byte(`{"timestamp":"not-a-date","message":"hi"}`)
	if _, err := toLineProtocol(raw, "logs"); err == nil {
		t.Fatal("expected error for unparsable timestamp, got nil")
	}
}

func TestToLineProtocolNoFields(t *testing.T) {
	raw := []byte(`{"timestamp":"2026-05-01T10:00:00.000Z","service":"svc"}`)
	if _, err := toLineProtocol(raw, "logs"); err == nil {
		t.Fatal("expected 'no fields' error, got nil")
	}
}

func TestToLineProtocolInvalidJSON(t *testing.T) {
	if _, err := toLineProtocol([]byte(`{not json`), "logs"); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestParseTimestamp(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		wantErr bool
	}{
		{"RFC3339Nano", "2026-05-01T10:00:00.123456789Z", false},
		{"RFC3339", "2026-05-01T10:00:00Z", false},
		{"millis", "2026-05-01T10:00:00.123Z", false},
		{"empty uses now", "", false},
		{"non-string uses now", 12345, false},
		{"garbage", "yesterday", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseTimestamp(c.in)
			if (err != nil) != c.wantErr {
				t.Errorf("parseTimestamp(%v) err = %v, wantErr = %v", c.in, err, c.wantErr)
			}
		})
	}
}

func TestFormatField(t *testing.T) {
	cases := []struct {
		name   string
		in     any
		want   string
		wantOk bool
	}{
		{"string", "hi", `"hi"`, true},
		{"bool true", true, "true", true},
		{"bool false", false, "false", true},
		{"json int", json.Number("42"), "42i", true},
		{"json float", json.Number("3.14"), "3.14", true},
		{"float64", 2.5, "2.5", true},
		{"int", 7, "7i", true},
		{"nil", nil, "", false},
		{"nested", map[string]any{"k": "v"}, `"{\"k\":\"v\"}"`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := formatField(c.in)
			if ok != c.wantOk || got != c.want {
				t.Errorf("formatField(%v) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.wantOk)
			}
		})
	}
}

func TestQuoteString(t *testing.T) {
	in := `say "hi"\bye`
	want := `"say \"hi\"\\bye"`
	if got := quoteString(in); got != want {
		t.Errorf("quoteString = %q, want %q", got, want)
	}
}

func TestEscapeTag(t *testing.T) {
	if got, want := escapeTag(`a,b=c d`), `a\,b\=c\ d`; got != want {
		t.Errorf("escapeTag = %q, want %q", got, want)
	}
}

func TestEscapeMeasurement(t *testing.T) {
	// commas and spaces are escaped, '=' is not.
	if got, want := escapeMeasurement(`a,b c=d`), `a\,b\ c=d`; got != want {
		t.Errorf("escapeMeasurement = %q, want %q", got, want)
	}
}

func TestRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"network error", errors.New("dial tcp: timeout"), true},
		{"400 bad request", &httpStatusError{status: 400}, false},
		{"404 not found", &httpStatusError{status: 404}, false},
		{"429 too many requests", &httpStatusError{status: 429}, true},
		{"500 server error", &httpStatusError{status: 500}, true},
		{"503 unavailable", &httpStatusError{status: 503}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := retryable(c.err); got != c.want {
				t.Errorf("retryable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestShipBatchRetriesThenSucceeds(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cfg := config{endpoint: srv.URL, maxRetries: 5, retryBackoff: time.Millisecond}
	if err := shipBatch(context.Background(), srv.Client(), cfg, "logs x=1i 1"); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("server hit %d times, want 3", got)
	}
}

func TestShipBatchNoRetryOn4xx(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	cfg := config{endpoint: srv.URL, maxRetries: 5, retryBackoff: time.Millisecond}
	if err := shipBatch(context.Background(), srv.Client(), cfg, "logs x=1i 1"); err == nil {
		t.Fatal("expected error for 4xx, got nil")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("server hit %d times, want 1 (no retry on 4xx)", got)
	}
}

func TestShipBatchExhaustsRetries(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := config{endpoint: srv.URL, maxRetries: 2, retryBackoff: time.Millisecond}
	if err := shipBatch(context.Background(), srv.Client(), cfg, "logs x=1i 1"); err == nil {
		t.Fatal("expected error after exhausting retries, got nil")
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("server hit %d times, want 3 (1 try + 2 retries)", got)
	}
}
