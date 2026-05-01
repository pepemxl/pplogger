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
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type config struct {
	file          string
	endpoint      string
	authHeader    string
	measurement   string
	batchSize     int
	flushInterval time.Duration
	pollInterval  time.Duration
	fromStart     bool
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
	flush := func() {
		if len(batch) == 0 {
			return
		}
		body := strings.Join(batch, "\n")
		batch = batch[:0]
		if err := writeBatch(ctx, client, cfg, body); err != nil {
			log.Printf("write batch failed (%d bytes): %v", len(body), err)
		}
	}

	ticker := time.NewTicker(cfg.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			flush()
			return nil
		case <-ticker.C:
			flush()
		case raw, ok := <-in:
			if !ok {
				flush()
				return nil
			}
			line, err := toLineProtocol(raw, cfg.measurement)
			if err != nil {
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
		return fmt.Errorf("status %d: %s", resp.StatusCode, bytes.TrimSpace(preview))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// toLineProtocol converts a pplogger JSON record to one InfluxDB line.
// Tags (low-cardinality): service, module, level, hostname.
// Fields: message, logger, function, line, pid, exception_type, plus any
// scalar `extra` fields the producer attached.
func toLineProtocol(raw []byte, measurement string) (string, error) {
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
