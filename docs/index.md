# pplogger — Features overview

`pplogger` is a structured-logging toolkit for distributed systems. It
standardizes logs across services so they can be produced uniformly in Python
and shipped, at scale, to a time-series database for querying and alerting.

The project has two cooperating components:

```
┌─────────────────┐   JSON lines    ┌──────────────────────┐   line protocol   ┌──────────────┐
│ Python library  │ ──────────────► │  Go processor/shipper │ ────────────────► │     TSDB     │
│   (pplogger/)   │   daily file    │     (processor/)      │     over HTTP      │ Influx/VM/…  │
└─────────────────┘                 └──────────────────────┘                    └──────────────┘
```

- The **Python library** configures the standard `logging` module to emit one
  JSON document per line into a daily log file (and optionally stdout).
- The **Go processor** tails that file and ships each record to any database
  that accepts InfluxDB line protocol over HTTP.

For task-focused guides see [Python library](intro.md) and
[Go processor](processor.md). This page is the complete catalog of features.

---

## 1. Python logging library (`pplogger/`)

### Public API

| Symbol | Kind | Purpose |
|--------|------|---------|
| `initializer_logger(...)` | function | Configure the root logger to emit JSON to a daily file (and stdout). Returns the active log file `Path`. |
| `build_log_path(...)` | function | Compute the daily log file path without configuring anything. |
| `JSONFormatter` | class | `logging.Formatter` that serializes each record to a single-line JSON document. |
| `__version__` | str | Package version. |

`initializer_logger` and `build_log_path` are re-exported from the package root
(`from pplogger import initializer_logger, build_log_path`).

### `initializer_logger(service, module, log_dir, debug, console) -> Path`

Configures the **root** logger and returns the path of the active log file
(useful to hand to the Go shipper or to tests).

Features:

- **Daily file handler** — writes JSON records to
  `<log_dir>/<service>.<module>_logs.<YYYY_MM_DD>.log`, creating the directory
  if needed (`parents=True, exist_ok=True`). With `rotate_daily=True` a
  long-running process rolls over to the next day's dated file at midnight;
  with `max_bytes` it rotates by size instead.
- **Optional console handler** — when `console=True` (default) the same JSON is
  also streamed to `stdout`.
- **Level control** — `debug=True` (or the `PPLOGGER_DEBUG` env var) sets the
  root level to `DEBUG`; otherwise `INFO`.
- **Idempotency** — every handler it installs is tagged with a private
  `_pplogger` marker; calling the function again removes and replaces only those
  handlers, so it is safe to invoke from each entry point without stacking
  duplicate outputs.
- **UTF-8 output** — the file handler is opened with `encoding="utf-8"` and JSON
  is serialized with `ensure_ascii=False`, preserving non-ASCII text.

### `build_log_path(service, module, log_dir, when) -> Path`

Pure helper that returns the daily log path. `when` defaults to today's date;
pass an explicit `datetime.date` for deterministic paths in tests. The naming
scheme is:

```
<log_dir>/<service>.<module>_logs.<YYYY_MM_DD>.log
# e.g. /tmp/service_api.module_pepe_logs.2024_07_13.log
```

### `JSONFormatter` — record serialization

Each log record becomes one JSON object on its own line. Standard fields:

| Field | Source |
|-------|--------|
| `timestamp` | UTC ISO-8601 with millisecond precision, `Z`-suffixed. |
| `level` | Record level name (`DEBUG`, `INFO`, `ERROR`, …). |
| `logger` | Name from `logging.getLogger(name)`. |
| `message` | Rendered message (`record.getMessage()`). |
| `service` / `module` | Identifiers passed to `initializer_logger`. |
| `hostname` | `socket.gethostname()` of the emitter. |
| `pid` | Process id (cached, but refreshed after `os.fork()` so children report their own). |
| `source_module` | Python module that issued the log. |
| `function` | Function name that issued the log. |
| `line` | Source line number. |
| `thread` | Thread name. |

Additional behavior:

- **Exception capture** — when a record carries `exc_info` (e.g.
  `log.exception(...)`), the payload gains an `exception` object with `type`,
  `message`, and the full formatted `traceback`.
- **Structured extras** — anything passed via `extra={...}` that is not a
  reserved `LogRecord` attribute is merged into the JSON payload as a top-level
  field. Values that are not natively JSON-serializable are coerced to their
  `repr()` via a best-effort `_safe()` helper, so logging never raises on an
  exotic value.
- **Low-config** — `service`, `module`, `hostname`, and `pid` are captured once
  at formatter construction, not per record.

### Configuration & environment variables

Every argument has an environment-variable fallback so the same code deploys
unchanged across services:

| Argument | Env var | Default | Purpose |
|----------|---------|---------|---------|
| `service` | `PPLOGGER_SERVICE` | `service_api` | `service` tag in every record. |
| `module` | `PPLOGGER_MODULE` | `module_pepe` | `module` tag in every record. |
| `log_dir` | `PPLOGGER_DIR` | `/tmp` | Directory for the daily log file. |
| `debug` | `PPLOGGER_DEBUG` | `False` | Truthy values (`1/true/yes/on`) raise level to `DEBUG`. |
| `console` | — | `True` | Also stream JSON to stdout. |
| `level` | — | `None` | Explicit level (`int` or name like `"WARNING"`); overrides `debug`. |
| `max_bytes` | — | `0` | When > 0, rotate the file at this size (`RotatingFileHandler`). |
| `backup_count` | — | `0` | Number of rotated backups to keep (with `max_bytes`). |
| `rotate_daily` | — | `False` | Roll to a new date-stamped file at midnight (ignored if `max_bytes` > 0). |
| `hostname` | `PPLOGGER_HOSTNAME` | host name | Override the `hostname` field. |

Per-logger filtering still works after initialization, e.g.
`logging.getLogger("noisy.lib").setLevel(logging.WARNING)`.

### Context fields for correlation

Bind fields once and they are merged into every subsequent record from the same
thread / asyncio task — no need to repeat them on each call. Explicit
`extra={...}` fields override bound context on key collisions.

```python
from pplogger import bind_context, context

bind_context(request_id="abc-123")     # sticky until cleared/overwritten
log.info("handling")                    # -> includes request_id

with context(trace_id="t-1"):           # scoped to the block
    log.info("inner")                   # -> includes request_id + trace_id
```

API: `bind_context(**fields)`, `context(**fields)` (context manager),
`clear_context()`, `get_context()`.

---

## 2. Go processor / shipper (`processor/`)

A dependency-free (stdlib-only) Go program that tails a pplogger JSON log file
and writes each record to a TSDB speaking InfluxDB line protocol over HTTP
(InfluxDB 1.x/2.x, VictoriaMetrics, QuestDB, …).

### Tailing & file rotation

- **Live tail** — reads the file line by line, polling at `--poll-interval`
  while at EOF. By default it seeks to the end on startup; `--from-start` reads
  the whole file first.
- **Partial-line buffering** — incomplete trailing data (no newline yet) is
  buffered until the rest of the line arrives, so half-written records are never
  shipped.
- **Rotation detection** — re-opens the file when its inode changes
  (logrotate create-then-rename) or when it shrinks below the last seen size
  (truncation), then resumes from the start of the new file.

### Batching

- Records are buffered and flushed as a single HTTP write when the batch reaches
  `--batch-size` **or** after `--flush-interval`, whichever comes first.

### JSON → line protocol mapping

- **Tags** (low cardinality, indexed): `service`, `module`, `level`,
  `hostname`. Empty values are skipped to keep series count bounded; with
  `--max-tag-cardinality` a tag that exceeds N distinct values is demoted to a
  field to protect the TSDB.
- **Fields**: `message`, `logger`, `function`, `line`, `pid`,
  `exception_type`, `exception_message`, plus any scalar `extra` field promoted
  from the record. Numbers are emitted as integers (`i` suffix) or floats;
  strings are quoted and escaped; nested objects are JSON-stringified.
- **Timestamp** — parsed from the record's `timestamp` (RFC 3339 / nanosecond
  precision). A missing timestamp falls back to "now"; an unparsable one drops
  the record.
- **Escaping** — measurement, tag, and field encoding follow line-protocol
  rules (commas, spaces, equals, quotes, backslashes).

### Reliable delivery

- **Retry classification** — network/timeout errors and transient server
  responses (HTTP **429** and **5xx**) are retried; permanent client errors
  (other **4xx**, e.g. malformed line protocol) are dropped immediately.
- **Exponential backoff** — retries back off starting at `--retry-backoff`,
  doubling each attempt up to a 30s cap, for at most `--max-retries` attempts.
- **Durable spool (optional)** — with `--spool-dir`, batches that exhaust their
  retries are persisted to disk and replayed by a background loop until they
  succeed, surviving process restarts (at-least-once). Permanent (4xx) batches
  are discarded instead of spooled.
- **Context-aware** — backoff and in-flight writes abort promptly on shutdown.
- **Resilient parsing** — a single malformed JSON line is logged and skipped;
  the tail keeps going.

### Observability

- Running counters (`shipped`, `dropped`, `malformed`, `batches`, `spooled`)
  are exposed two ways: a periodic summary log (`--metrics-interval`) and a
  Prometheus `/metrics` endpoint (`--metrics-addr`). Counters are atomic, so
  scraping never races the ship loop.

### Graceful shutdown

- `SIGINT` / `SIGTERM` cancel the context; the in-flight batch is flushed before
  the process exits.

### Configuration (flags + env)

| Flag | Env var | Default | Purpose |
|------|---------|---------|---------|
| `--file` | `PPLOGGER_FILE` | — | JSON log file to tail (**required**). |
| `--endpoint` | `PPLOGGER_TSDB_URL` | — | Line-protocol write endpoint (**required**). |
| `--token` | `PPLOGGER_TSDB_TOKEN` | — | `Authorization` header value. |
| `--measurement` | — | `logs` | InfluxDB measurement name. |
| `--batch-size` | — | `200` | Max records per HTTP write. |
| `--flush-interval` | — | `2s` | Force-flush a partial batch after this duration. |
| `--poll-interval` | — | `250ms` | Poll cadence for new data at EOF. |
| `--from-start` | — | `false` | Read from the beginning instead of seeking to EOF. |
| `--max-retries` | — | `5` | Retries for a failed batch on retryable errors. |
| `--retry-backoff` | — | `500ms` | Initial backoff; doubles per attempt, capped at 30s. |
| `--metrics-interval` | — | `0` | When > 0, periodically log internal counters. |
| `--metrics-addr` | `PPLOGGER_METRICS_ADDR` | — | Serve Prometheus counters at `/metrics`. |
| `--spool-dir` | `PPLOGGER_SPOOL_DIR` | — | Persist exhausted batches to disk and replay them. |
| `--max-tag-cardinality` | — | `0` | Demote a tag to a field past N distinct values. |

The required inputs accept environment variables, so the binary drops cleanly
into systemd or Docker. See [processor.md](processor.md) for build and
deployment examples.

---

## 3. End-to-end example

```python
import logging
from pplogger import initializer_logger

log_path = initializer_logger(service="orders", module="api")
log = logging.getLogger(__name__)

log.info("request handled", extra={"request_id": "abc-123", "duration_ms": 42})
try:
    1 / 0
except ZeroDivisionError:
    log.exception("payment failed")
```

```bash
# Ship whatever the Python side writes to the TSDB.
processor/pplogger-processor \
    --file "$log_path" \
    --endpoint 'http://localhost:8086/api/v2/write?org=acme&bucket=logs&precision=ns' \
    --token 'Token my-influx-token'
```

---

## 4. Feature summary

| Capability | Python library | Go processor |
|------------|:--------------:|:------------:|
| Structured JSON output (one doc per line) | ✅ | — |
| Daily log file with deterministic naming | ✅ | — |
| Console + file fan-out | ✅ | — |
| Idempotent re-initialization | ✅ | — |
| Exception + traceback capture | ✅ | — |
| Arbitrary `extra={…}` fields | ✅ | ✅ (promoted to fields) |
| Context fields (request_id/trace_id) | ✅ | ✅ (promoted to fields) |
| Env-var configuration | ✅ | ✅ |
| Debug-level toggle / explicit level | ✅ | — |
| File rotation (size + daily/midnight) | ✅ | — |
| Live tail with rotation handling | — | ✅ |
| Batched HTTP writes | — | ✅ |
| Line-protocol mapping & escaping | — | ✅ |
| Retry with backoff (429/5xx/network) | — | ✅ |
| Durable disk spool / replay (at-least-once) | — | ✅ |
| Internal metrics counters (log + Prometheus) | — | ✅ |
| Configurable hostname | ✅ | — |
| Graceful shutdown (SIGINT/SIGTERM) | — | ✅ |
