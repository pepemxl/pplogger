package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

	line, err := toLineProtocol(raw, "logs", nil)
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

	line, err := toLineProtocol(raw, "logs", nil)
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
	line, err := toLineProtocol(raw, "logs", nil)
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
	if _, err := toLineProtocol(raw, "logs", nil); err == nil {
		t.Fatal("expected error for unparsable timestamp, got nil")
	}
}

func TestToLineProtocolNoFields(t *testing.T) {
	raw := []byte(`{"timestamp":"2026-05-01T10:00:00.000Z","service":"svc"}`)
	if _, err := toLineProtocol(raw, "logs", nil); err == nil {
		t.Fatal("expected 'no fields' error, got nil")
	}
}

func TestToLineProtocolInvalidJSON(t *testing.T) {
	if _, err := toLineProtocol([]byte(`{not json`), "logs", nil); err == nil {
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

func TestCardinalityGuardDemotesTagToField(t *testing.T) {
	guard := newCardinalityGuard(1)
	rec := func(host string) []byte {
		return []byte(`{"timestamp":"2026-05-01T10:00:00.000Z","level":"INFO","message":"m",` +
			`"service":"svc","hostname":"` + host + `"}`)
	}

	// First distinct hostname value is allowed as a tag.
	l1, err := toLineProtocol(rec("h1"), "logs", guard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	prefix1, fields1, _ := parseLine(t, l1)
	if !strings.Contains(prefix1, "hostname=h1") {
		t.Errorf("expected hostname tag in prefix %q", prefix1)
	}
	if _, ok := fields1["hostname"]; ok {
		t.Errorf("did not expect hostname as a field yet: %v", fields1)
	}

	// Second distinct value trips the limit and is demoted to a field.
	l2, err := toLineProtocol(rec("h2"), "logs", guard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	prefix2, fields2, _ := parseLine(t, l2)
	if strings.Contains(prefix2, "hostname=") {
		t.Errorf("expected hostname demoted out of tags, prefix %q", prefix2)
	}
	if got := fields2["hostname"]; got != `"h2"` {
		t.Errorf("expected hostname field=%q, got %q", `"h2"`, got)
	}
	// service stays a tag throughout (low cardinality).
	if !strings.Contains(prefix2, "service=svc") {
		t.Errorf("expected service tag retained, prefix %q", prefix2)
	}
}

func TestCardinalityGuardNilAllowsAll(t *testing.T) {
	var guard *cardinalityGuard // disabled
	if !guard.allow("hostname", "anything") {
		t.Error("nil guard should allow all tags")
	}
}

func newTestMetrics(shipped, dropped, malformed, batches, spooled int64) *metrics {
	m := &metrics{}
	m.shipped.Add(shipped)
	m.dropped.Add(dropped)
	m.malformed.Add(malformed)
	m.batches.Add(batches)
	m.spooled.Add(spooled)
	return m
}

func nextLine(t *testing.T, ch <-chan []byte) string {
	t.Helper()
	select {
	case b := <-ch:
		return string(b)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for a tailed line")
		return ""
	}
}

func appendLine(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatalf("append: %v", err)
	}
}

func TestTailFollowsAppendsAndRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("line1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan []byte, 16)
	cfg := config{file: path, fromStart: true, pollInterval: 10 * time.Millisecond}
	errc := make(chan error, 1)
	go func() { errc <- tail(ctx, cfg, out) }()

	if got := nextLine(t, out); got != "line1" {
		t.Errorf("first line = %q, want line1", got)
	}

	// Plain append on the same inode.
	appendLine(t, path, "line2\n")
	if got := nextLine(t, out); got != "line2" {
		t.Errorf("append = %q, want line2", got)
	}

	// Partial write must be buffered until the newline arrives.
	appendLine(t, path, "par")
	appendLine(t, path, "tial\n")
	if got := nextLine(t, out); got != "partial" {
		t.Errorf("partial = %q, want partial", got)
	}

	// Rotation by inode change (logrotate create-then-rename style).
	newpath := filepath.Join(dir, "app.log.new")
	if err := os.WriteFile(newpath, []byte("rotated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(newpath, path); err != nil {
		t.Fatal(err)
	}
	if got := nextLine(t, out); got != "rotated" {
		t.Errorf("after rotation = %q, want rotated", got)
	}

	cancel()
	if err := <-errc; err != nil && err != context.Canceled {
		t.Errorf("tail returned %v", err)
	}
}

func TestTailHandlesTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan []byte, 16)
	cfg := config{file: path, fromStart: true, pollInterval: 10 * time.Millisecond}
	errc := make(chan error, 1)
	go func() { errc <- tail(ctx, cfg, out) }()

	if got := nextLine(t, out); got != "first" {
		t.Errorf("first line = %q, want first", got)
	}

	// Let the poller record the current size (6) before we shrink it, so the
	// truncation is actually observed as size < lastSize.
	time.Sleep(60 * time.Millisecond)

	// Truncate (size shrinks below last seen), let the poller observe the
	// shrink and reopen at offset 0, then write fresh content. This models
	// logrotate's copytruncate: truncate now, app writes gradually after.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond) // > pollInterval, so the reopen happens first
	appendLine(t, path, "after-trunc\n")
	if got := nextLine(t, out); got != "after-trunc" {
		t.Errorf("after truncation = %q, want after-trunc", got)
	}

	cancel()
	if err := <-errc; err != nil && err != context.Canceled {
		t.Errorf("tail returned %v", err)
	}
}

func TestMetricsString(t *testing.T) {
	m := newTestMetrics(5, 1, 2, 3, 4)
	want := "shipped=5 dropped=1 malformed=2 batches=3 spooled=4"
	if got := m.String(); got != want {
		t.Errorf("metrics.String() = %q, want %q", got, want)
	}
}

func TestMetricsPrometheus(t *testing.T) {
	out := newTestMetrics(5, 1, 2, 3, 4).prometheus()
	for _, want := range []string{
		"# TYPE pplogger_shipped_total counter",
		"pplogger_shipped_total 5",
		"pplogger_dropped_total 1",
		"pplogger_malformed_total 2",
		"pplogger_batches_total 3",
		"pplogger_spooled_total 4",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prometheus() missing %q in:\n%s", want, out)
		}
	}
}

func TestServeMetricsEndpoint(t *testing.T) {
	m := newTestMetrics(7, 0, 0, 2, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = io.WriteString(w, m.prometheus())
	}))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "pplogger_shipped_total 7") {
		t.Errorf("unexpected /metrics body:\n%s", body)
	}
}

func TestSpoolBatchAndReplay(t *testing.T) {
	dir := t.TempDir()
	if err := spoolBatch(dir, "logs x=1i 1"); err != nil {
		t.Fatalf("spoolBatch: %v", err)
	}
	if err := spoolBatch(dir, "logs x=2i 2"); err != nil {
		t.Fatalf("spoolBatch: %v", err)
	}

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cfg := config{endpoint: srv.URL, spoolDir: dir, maxRetries: 1, retryBackoff: time.Millisecond}
	n, err := replaySpoolOnce(context.Background(), srv.Client(), cfg)
	if err != nil {
		t.Fatalf("replaySpoolOnce: %v", err)
	}
	if n != 2 {
		t.Errorf("replayed %d, want 2", n)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("server hit %d times, want 2", got)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*"+spoolExt))
	if len(left) != 0 {
		t.Errorf("expected spool dir drained, %d file(s) left", len(left))
	}
}

func TestReplaySpoolStopsOnRetryableFailure(t *testing.T) {
	dir := t.TempDir()
	if err := spoolBatch(dir, "logs x=1i 1"); err != nil {
		t.Fatalf("spoolBatch: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := config{endpoint: srv.URL, spoolDir: dir, maxRetries: 0, retryBackoff: time.Millisecond}
	n, err := replaySpoolOnce(context.Background(), srv.Client(), cfg)
	if err == nil {
		t.Fatal("expected error on persistent 503, got nil")
	}
	if n != 0 {
		t.Errorf("replayed %d, want 0", n)
	}
	// The batch must survive for a later pass.
	left, _ := filepath.Glob(filepath.Join(dir, "*"+spoolExt))
	if len(left) != 1 {
		t.Errorf("expected 1 spooled file retained, got %d", len(left))
	}
}

func TestReplaySpoolDiscardsPermanentError(t *testing.T) {
	dir := t.TempDir()
	if err := spoolBatch(dir, "logs x=1i 1"); err != nil {
		t.Fatalf("spoolBatch: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	cfg := config{endpoint: srv.URL, spoolDir: dir, maxRetries: 3, retryBackoff: time.Millisecond}
	n, err := replaySpoolOnce(context.Background(), srv.Client(), cfg)
	if err != nil {
		t.Fatalf("replaySpoolOnce: %v", err)
	}
	if n != 1 {
		t.Errorf("replayed %d, want 1 (discarded)", n)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*"+spoolExt))
	if len(left) != 0 {
		t.Errorf("expected permanent-error batch discarded, %d left", len(left))
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
