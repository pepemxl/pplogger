# pplogger-processor (Go)

`processor/` is a stdlib-only Go program that tails a pplogger JSON log file
and ships each record to a time-series database that accepts InfluxDB line
protocol over HTTP — InfluxDB 1.x/2.x, VictoriaMetrics, QuestDB, etc.

## Build

```bash
cd processor
go build -o pplogger-processor
```

## Run

```bash
./pplogger-processor \
    --file /tmp/service_api.module_pepe_logs.2024_07_13.log \
    --endpoint 'http://localhost:8086/api/v2/write?org=acme&bucket=logs&precision=ns' \
    --token 'Token my-influx-token'
```

All three core inputs also accept environment variables, so the binary fits
cleanly into systemd / Docker:

| Flag             | Env var               | Default                | Purpose                                            |
|------------------|-----------------------|------------------------|----------------------------------------------------|
| `--file`         | `PPLOGGER_FILE`       | —                      | JSON log file to tail. Required.                   |
| `--endpoint`     | `PPLOGGER_TSDB_URL`   | —                      | Write endpoint accepting line protocol. Required.  |
| `--token`        | `PPLOGGER_TSDB_TOKEN` | —                      | Value for the `Authorization` HTTP header.         |
| `--measurement`  | —                     | `logs`                 | InfluxDB measurement name.                         |
| `--batch-size`   | —                     | `200`                  | Max records per HTTP write.                        |
| `--flush-interval` | —                   | `2s`                   | Force-flush a partial batch after this duration.   |
| `--poll-interval`| —                     | `250ms`                | How often to poll for new data at EOF.             |
| `--from-start`   | —                     | `false`                | Read from the beginning instead of seeking to EOF. |

Send `SIGINT` or `SIGTERM` for graceful shutdown — the in-flight batch is
flushed before exit.

## Mapping JSON → line protocol

Each JSON record becomes one line. Tags are kept low-cardinality so the
resulting series count stays bounded.

**Tags** (indexed): `service`, `module`, `level`, `hostname`.

**Fields**: `message`, `logger`, `function`, `line`, `pid`,
`exception_type`, `exception_message`, plus any scalar `extra={…}` field
attached by the producer.

**Timestamp**: parsed from the `timestamp` field (RFC 3339, nanosecond
precision). If missing or unparsable the record is dropped (with a log line
to stderr).

Example input:

```json
{"timestamp":"2026-05-01T10:00:00.123Z","level":"INFO","logger":"app",
 "message":"hello","service":"svc","module":"mod","hostname":"h1",
 "pid":42,"function":"main","line":12,"request_id":"abc-123"}
```

Example output (single line, wrapped here for readability):

```
logs,service=svc,module=mod,level=INFO,hostname=h1
  message="hello",logger="app",function="main",line=12i,pid=42i,request_id="abc-123"
  1746093600123000000
```

## File rotation

The shipper detects rotation in two ways:

1. The watched file's inode changes (typical `logrotate` create-then-rename).
2. The file shrinks below the last seen size (truncation).

In either case it re-opens from the start and resumes shipping.

## systemd unit (example)

```ini
[Unit]
Description=pplogger TSDB shipper
After=network-online.target

[Service]
Environment=PPLOGGER_FILE=/var/log/pplogger/service_api.module_pepe_logs.current.log
Environment=PPLOGGER_TSDB_URL=http://influx.internal:8086/api/v2/write?org=acme&bucket=logs&precision=ns
Environment=PPLOGGER_TSDB_TOKEN=Token redacted
ExecStart=/usr/local/bin/pplogger-processor
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

## Failure behavior

- Bad JSON lines are skipped with a stderr log line; the tail continues.
- HTTP failures (timeouts, non-2xx responses) are logged and the affected
  batch is dropped — the shipper does not currently buffer to disk. Pair
  with a remote write that has its own retry/queue if at-least-once delivery
  is required.
