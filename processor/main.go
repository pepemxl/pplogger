// Command pplogger-processor tails a pplogger JSON log file and ships each
// record to a time-series database that accepts InfluxDB line protocol over
// HTTP (InfluxDB 1.x/2.x, VictoriaMetrics, QuestDB, ...).
//
// Usage:
//
//	pplogger-processor \
//	    --file /tmp/service_api.module_pepe_logs.2024_07_13.log \
//	    --endpoint http://localhost:8086/api/v2/write?org=acme&bucket=logs \
//	    --token "Token my-influx-token"
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// maxRetryBackoff caps the exponential backoff between write retries.
const maxRetryBackoff = 30 * time.Second

type config struct {
	file          string
	endpoint      string
	authHeader    string
	measurement   string
	batchSize     int
	flushInterval time.Duration
	pollInterval  time.Duration
	fromStart       bool
	maxRetries      int
	retryBackoff    time.Duration
	metricsInterval   time.Duration
	spoolDir          string
	maxTagCardinality int
}

// metrics holds running counters for the shipper, logged periodically when
// --metrics-interval > 0.
type metrics struct {
	shipped   int
	dropped   int
	malformed int
	batches   int
	spooled   int
}

func (m metrics) String() string {
	return fmt.Sprintf("shipped=%d dropped=%d malformed=%d batches=%d spooled=%d",
		m.shipped, m.dropped, m.malformed, m.batches, m.spooled)
}

func parseFlags() config {
	c := config{}
	flag.StringVar(&c.file, "file", os.Getenv("PPLOGGER_FILE"), "path to the JSON log file to tail")
	flag.StringVar(&c.endpoint, "endpoint", os.Getenv("PPLOGGER_TSDB_URL"), "TSDB write endpoint accepting InfluxDB line protocol")
	flag.StringVar(&c.authHeader, "token", os.Getenv("PPLOGGER_TSDB_TOKEN"), "value for the Authorization header (e.g. 'Token my-token')")
	flag.StringVar(&c.measurement, "measurement", "logs", "InfluxDB measurement name")
	flag.IntVar(&c.batchSize, "batch-size", 200, "max records per HTTP write")
	flag.DurationVar(&c.flushInterval, "flush-interval", 2*time.Second, "max time before a partial batch is flushed")
	flag.DurationVar(&c.pollInterval, "poll-interval", 250*time.Millisecond, "how often to poll the file for new data")
	flag.BoolVar(&c.fromStart, "from-start", false, "read the file from the beginning instead of seeking to EOF")
	flag.IntVar(&c.maxRetries, "max-retries", 5, "max retries for a failed batch on retryable errors (5xx/429/network)")
	flag.DurationVar(&c.retryBackoff, "retry-backoff", 500*time.Millisecond, "initial backoff between retries (doubles each attempt, capped at 30s)")
	flag.DurationVar(&c.metricsInterval, "metrics-interval", 0, "if > 0, periodically log internal counters (shipped/dropped/malformed/batches)")
	flag.StringVar(&c.spoolDir, "spool-dir", os.Getenv("PPLOGGER_SPOOL_DIR"), "if set, persist batches that exhaust retries to this directory and replay them later (durable at-least-once)")
	flag.IntVar(&c.maxTagCardinality, "max-tag-cardinality", 0, "if > 0, demote a tag to a field once it exceeds this many distinct values (protects the TSDB)")
	flag.Parse()
	return c
}

func main() {
	cfg := parseFlags()
	if cfg.file == "" || cfg.endpoint == "" {
		fmt.Fprintln(os.Stderr, "--file and --endpoint are required")
		flag.Usage()
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	lines := make(chan []byte, 1024)
	go func() {
		defer close(lines)
		if err := tail(ctx, cfg, lines); err != nil && ctx.Err() == nil {
			log.Printf("tail stopped: %v", err)
		}
	}()

	if cfg.spoolDir != "" {
		go replaySpool(ctx, cfg)
	}

	if err := ship(ctx, cfg, lines); err != nil {
		log.Fatalf("shipper exited: %v", err)
	}
}

// tail reads the file and emits one byte slice per complete line. It re-opens
// the file when it has been truncated or rotated. Partial trailing data
// (without a newline) is buffered until the rest of the line arrives.
func tail(ctx context.Context, cfg config, out chan<- []byte) error {
	var (
		f         *os.File
		reader    *bufio.Reader
		pending   []byte
		lastInode uint64
		lastSize  int64
	)

	open := func() error {
		if f != nil {
			f.Close()
		}
		nf, err := os.Open(cfg.file)
		if err != nil {
			return err
		}
		if !cfg.fromStart {
			if _, err := nf.Seek(0, io.SeekEnd); err != nil {
				nf.Close()
				return err
			}
		}
		stat, err := nf.Stat()
		if err != nil {
			nf.Close()
			return err
		}
		f = nf
		reader = bufio.NewReader(f)
		pending = pending[:0]
		lastInode = inodeOf(stat)
		off, _ := f.Seek(0, io.SeekCurrent)
		lastSize = off
		// Only honor fromStart on first open; rotations always start at 0.
		cfg.fromStart = true
		return nil
	}

	if err := open(); err != nil {
		return fmt.Errorf("open %s: %w", cfg.file, err)
	}
	defer func() {
		if f != nil {
			f.Close()
		}
	}()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			pending = append(pending, line...)
		}
		if err == nil {
			// Complete line: emit the whole pending buffer.
			trimmed := bytes.TrimRight(pending, "\r\n")
			if len(trimmed) > 0 {
				cp := make([]byte, len(trimmed))
				copy(cp, trimmed)
				select {
				case out <- cp:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			pending = pending[:0]
			continue
		}
		if err != io.EOF {
			return err
		}

		// EOF reached mid-line: keep `pending` as-is and wait for more data.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cfg.pollInterval):
		}

		stat, statErr := os.Stat(cfg.file)
		if statErr != nil {
			// File disappeared (rotation in progress); retry next tick.
			continue
		}
		if inodeOf(stat) != lastInode || stat.Size() < lastSize {
			if err := open(); err != nil {
				log.Printf("reopen failed: %v", err)
				continue
			}
		} else {
			lastSize = stat.Size()
		}
	}
}

// ship batches incoming JSON records and POSTs InfluxDB line protocol.
func ship(ctx context.Context, cfg config, in <-chan []byte) error {
	client := &http.Client{Timeout: 10 * time.Second}
	batch := make([]string, 0, cfg.batchSize)
	guard := newCardinalityGuard(cfg.maxTagCardinality)
	var m metrics
	flush := func() {
		if len(batch) == 0 {
			return
		}
		body := strings.Join(batch, "\n")
		records := len(batch)
		batch = batch[:0]
		if err := shipBatch(ctx, client, cfg, body); err != nil {
			if ctx.Err() != nil {
				return
			}
			// Retryable-but-exhausted failures can be spooled for later replay;
			// permanent errors (4xx) never will, so they are dropped outright.
			if cfg.spoolDir != "" && retryable(err) {
				if serr := spoolBatch(cfg.spoolDir, body); serr != nil {
					m.dropped += records
					log.Printf("dropping batch of %d records (spool failed: %v); original: %v", records, serr, err)
				} else {
					m.spooled += records
					log.Printf("spooled batch of %d records after retries: %v", records, err)
				}
				return
			}
			m.dropped += records
			log.Printf("dropping batch of %d records after retries: %v", records, err)
			return
		}
		m.shipped += records
		m.batches++
	}

	ticker := time.NewTicker(cfg.flushInterval)
	defer ticker.Stop()

	var metricsC <-chan time.Time
	if cfg.metricsInterval > 0 {
		mt := time.NewTicker(cfg.metricsInterval)
		defer mt.Stop()
		metricsC = mt.C
	}
	logMetrics := func(prefix string) {
		if cfg.metricsInterval > 0 {
			log.Printf("%s: %s", prefix, m)
		}
	}

	for {
		select {
		case <-ctx.Done():
			flush()
			logMetrics("final metrics")
			return nil
		case <-ticker.C:
			flush()
		case <-metricsC:
			log.Printf("metrics: %s", m)
		case raw, ok := <-in:
			if !ok {
				flush()
				logMetrics("final metrics")
				return nil
			}
			line, err := toLineProtocol(raw, cfg.measurement, guard)
			if err != nil {
				m.malformed++
				log.Printf("skip malformed record: %v", err)
				continue
			}
			batch = append(batch, line)
			if len(batch) >= cfg.batchSize {
				flush()
			}
		}
	}
}

// httpStatusError carries a non-2xx response so callers can decide whether the
// failure is worth retrying (5xx/429) or permanent (other 4xx).
type httpStatusError struct {
	status  int
	preview string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("status %d: %s", e.status, e.preview)
}

// retryable reports whether a failed write should be retried. Network/transport
// errors (timeouts, connection refused) and transient server responses
// (HTTP 429 and 5xx) are retryable; permanent client errors (other 4xx) are not.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	var se *httpStatusError
	if errors.As(err, &se) {
		return se.status == http.StatusTooManyRequests || se.status >= 500
	}
	return true
}

// shipBatch writes a batch, retrying retryable failures with exponential
// backoff up to cfg.maxRetries. It stops early if the context is cancelled.
func shipBatch(ctx context.Context, client *http.Client, cfg config, body string) error {
	backoff := cfg.retryBackoff
	for attempt := 0; ; attempt++ {
		err := writeBatch(ctx, client, cfg, body)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt >= cfg.maxRetries || !retryable(err) {
			return err
		}
		log.Printf("write attempt %d/%d failed: %v; retrying in %s", attempt+1, cfg.maxRetries+1, err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxRetryBackoff {
			backoff = maxRetryBackoff
		}
	}
}

// spoolExt marks batch files written to the spool directory.
const spoolExt = ".batch"

// spoolBatch durably persists a batch body to the spool directory. It writes to
// a temp file and renames it into place so a replayer never sees a partial file.
func spoolBatch(dir, body string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "tmp-*"+spoolExt)
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	// Final name encodes time for FIFO-ish ordering; keep the .batch suffix.
	final := filepath.Join(dir, fmt.Sprintf("%d-%s", time.Now().UnixNano(), filepath.Base(name)))
	return os.Rename(name, final)
}

// replaySpoolOnce attempts to re-ship every spooled batch once, oldest first.
// A successfully shipped file is removed; on the first retryable failure it
// stops (leaving the rest for a later pass). Returns the number replayed.
func replaySpoolOnce(ctx context.Context, client *http.Client, cfg config) (int, error) {
	entries, err := os.ReadDir(cfg.spoolDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), spoolExt) && !strings.HasPrefix(e.Name(), "tmp-") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	replayed := 0
	for _, name := range names {
		if ctx.Err() != nil {
			return replayed, ctx.Err()
		}
		path := filepath.Join(cfg.spoolDir, name)
		body, err := os.ReadFile(path)
		if err != nil {
			log.Printf("spool: cannot read %s: %v", name, err)
			continue
		}
		if err := shipBatch(ctx, client, cfg, string(body)); err != nil {
			if retryable(err) {
				return replayed, err // still failing; try again next pass
			}
			// Permanent error: this batch will never succeed, so discard it.
			log.Printf("spool: discarding %s (permanent error): %v", name, err)
		}
		if err := os.Remove(path); err != nil {
			log.Printf("spool: cannot remove %s: %v", name, err)
		}
		replayed++
	}
	return replayed, nil
}

// replaySpool periodically drains the spool directory until the context ends.
func replaySpool(ctx context.Context, cfg config) {
	client := &http.Client{Timeout: 10 * time.Second}
	ticker := time.NewTicker(cfg.flushInterval)
	defer ticker.Stop()
	for {
		if n, err := replaySpoolOnce(ctx, client, cfg); err != nil && ctx.Err() == nil {
			log.Printf("spool replay paused: %v", err)
		} else if n > 0 {
			log.Printf("spool: replayed %d batch file(s)", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func writeBatch(ctx context.Context, client *http.Client, cfg config, body string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.endpoint, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	if cfg.authHeader != "" {
		req.Header.Set("Authorization", cfg.authHeader)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &httpStatusError{status: resp.StatusCode, preview: string(bytes.TrimSpace(preview))}
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// toLineProtocol converts a pplogger JSON record to one InfluxDB line.
// Tags (low-cardinality): service, module, level, hostname.
// Fields: message, logger, function, line, pid, exception_type, plus any
// scalar `extra` fields the producer attached.
// cardinalityGuard caps the number of distinct values a tag may take. Once a
// tag exceeds the limit it is "tripped": further occurrences are demoted to
// fields by toLineProtocol (the value is preserved but no longer creates new
// time-series). A nil guard disables the check.
type cardinalityGuard struct {
	max     int
	seen    map[string]map[string]struct{}
	tripped map[string]bool
}

func newCardinalityGuard(max int) *cardinalityGuard {
	if max <= 0 {
		return nil
	}
	return &cardinalityGuard{
		max:     max,
		seen:    map[string]map[string]struct{}{},
		tripped: map[string]bool{},
	}
}

// allow reports whether key=value may be emitted as a tag.
func (g *cardinalityGuard) allow(key, value string) bool {
	if g == nil {
		return true
	}
	if g.tripped[key] {
		return false
	}
	vals := g.seen[key]
	if vals == nil {
		vals = map[string]struct{}{}
		g.seen[key] = vals
	}
	if _, ok := vals[value]; ok {
		return true // already counted, no new series
	}
	if len(vals) >= g.max {
		g.tripped[key] = true
		log.Printf("cardinality: tag %q exceeded %d distinct values; demoting it to a field to protect the TSDB", key, g.max)
		return false
	}
	vals[value] = struct{}{}
	return true
}

func toLineProtocol(raw []byte, measurement string, guard *cardinalityGuard) (string, error) {
	var rec map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&rec); err != nil {
		return "", err
	}

	ts, err := parseTimestamp(rec["timestamp"])
	if err != nil {
		return "", fmt.Errorf("timestamp: %w", err)
	}

	var b strings.Builder
	b.WriteString(escapeMeasurement(measurement))
	for _, key := range []string{"service", "module", "level", "hostname"} {
		v, ok := rec[key].(string)
		if !ok || v == "" {
			continue
		}
		if !guard.allow(key, v) {
			// Leave the value in rec so it is promoted to a field below.
			continue
		}
		b.WriteByte(',')
		b.WriteString(escapeTag(key))
		b.WriteByte('=')
		b.WriteString(escapeTag(v))
		delete(rec, key)
	}

	fields := make([]string, 0, 8)
	addField := func(key string, value any) {
		if value == nil {
			return
		}
		f, ok := formatField(value)
		if !ok {
			return
		}
		fields = append(fields, escapeTag(key)+"="+f)
	}

	addField("message", rec["message"])
	addField("logger", rec["logger"])
	addField("function", rec["function"])
	addField("line", rec["line"])
	addField("pid", rec["pid"])
	if exc, ok := rec["exception"].(map[string]any); ok {
		addField("exception_type", exc["type"])
		addField("exception_message", exc["message"])
	}
	// Promote any remaining scalar extras as fields (skip already-consumed keys
	// and the verbose source-position metadata we don't index).
	skip := map[string]bool{
		"timestamp": true, "message": true, "logger": true, "function": true,
		"line": true, "pid": true, "exception": true, "thread": true,
		"source_module": true,
	}
	for k, v := range rec {
		if skip[k] {
			continue
		}
		addField(k, v)
	}

	if len(fields) == 0 {
		return "", fmt.Errorf("no fields")
	}
	b.WriteByte(' ')
	b.WriteString(strings.Join(fields, ","))
	b.WriteByte(' ')
	b.WriteString(strconv.FormatInt(ts.UnixNano(), 10))
	return b.String(), nil
}

func parseTimestamp(v any) (time.Time, error) {
	s, ok := v.(string)
	if !ok || s == "" {
		return time.Now().UTC(), nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp %q", s)
}

func formatField(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return quoteString(x), true
	case bool:
		if x {
			return "true", true
		}
		return "false", true
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return strconv.FormatInt(i, 10) + "i", true
		}
		if f, err := x.Float64(); err == nil {
			return strconv.FormatFloat(f, 'f', -1, 64), true
		}
		return quoteString(x.String()), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case int:
		return strconv.FormatInt(int64(x), 10) + "i", true
	case int64:
		return strconv.FormatInt(x, 10) + "i", true
	case nil:
		return "", false
	default:
		// Fallback: stringify nested objects/arrays as JSON.
		raw, err := json.Marshal(x)
		if err != nil {
			return "", false
		}
		return quoteString(string(raw)), true
	}
}

func quoteString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

// escapeTag escapes a tag key/value or measurement-suffix per line protocol.
func escapeTag(s string) string {
	r := strings.NewReplacer(",", `\,`, "=", `\=`, " ", `\ `)
	return r.Replace(s)
}

func escapeMeasurement(s string) string {
	r := strings.NewReplacer(",", `\,`, " ", `\ `)
	return r.Replace(s)
}
