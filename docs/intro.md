# pplogger

Common logger for observability and monitoring of distributed systems.

Every module emits structured JSON for:

- Things working fine (`info`, `debug`)
- Errors (`error`)
- Exceptions (`exception`, including traceback)

Debug mode is opt-in. All records are serialized as one JSON document per line
so downstream tooling (the Go processor in `./processor`) can ship them to a
time-series database.

## Install

```bash
poetry install
```

Or from source in any environment:

```bash
pip install -e .
```

## Quick start

```python
import logging
from pplogger import initializer_logger

initializer_logger()
log = logging.getLogger(__name__)

log.info("This log info message")
log.debug("This log debug message")     # only captured when debug=True
log.error("This log error message")
try:
    1 / 0
except ZeroDivisionError:
    log.exception("This log exception message")
```

## Configuration

`initializer_logger` accepts the following keyword arguments. Each one has an
environment-variable fallback so the same code can be deployed unchanged
across services.

| Argument   | Env var             | Default        | Purpose                                              |
|------------|---------------------|----------------|------------------------------------------------------|
| `service`  | `PPLOGGER_SERVICE`  | `service_api`  | Service identifier — becomes the `service` JSON tag. |
| `module`   | `PPLOGGER_MODULE`   | `module_pepe`  | Module identifier — becomes the `module` JSON tag.   |
| `log_dir`  | `PPLOGGER_DIR`      | `/tmp`         | Directory where the daily log file is written.       |
| `debug`    | `PPLOGGER_DEBUG`    | `False`        | When truthy, sets the root level to `DEBUG`.         |
| `console`  | —                   | `True`         | Also stream JSON to stdout.                          |

The function is idempotent: calling it again replaces only the handlers it
previously installed, so it is safe to invoke from each entry point.

## Log file naming

The active log file is named:

```
<log_dir>/<service>.<module>_logs.<YYYY_MM_DD>.log
```

For example:

```
/tmp/service_api.module_pepe_logs.2024_07_13.log
```

You can derive the same path programmatically:

```python
from pplogger import build_log_path
build_log_path(service="orders", module="api")
# PosixPath('/tmp/orders.api_logs.2026_05_01.log')
```

## Record schema

Each line is a single JSON object. Standard fields:

| Field            | Description                                              |
|------------------|----------------------------------------------------------|
| `timestamp`      | UTC ISO-8601 with millisecond precision (`...Z`).        |
| `level`          | `DEBUG`, `INFO`, `ERROR`, …                              |
| `logger`         | `logging.getLogger(name)` of the caller.                 |
| `message`        | Rendered log message.                                    |
| `service`        | From `initializer_logger(service=…)`.                    |
| `module`         | From `initializer_logger(module=…)`.                     |
| `hostname`       | `socket.gethostname()` of the emitter.                   |
| `pid`            | Process id.                                              |
| `source_module`  | Python module that issued the log.                       |
| `function`       | Function name that issued the log.                       |
| `line`           | Source line number.                                      |
| `thread`         | Thread name.                                             |
| `exception`      | `{type, message, traceback}` when `log.exception(...)`.  |

Anything passed via `extra={…}` is merged into the record:

```python
log.info("request handled", extra={"request_id": "abc-123", "duration_ms": 42})
```

```json
{"timestamp": "...", "level": "INFO", "message": "request handled",
 "request_id": "abc-123", "duration_ms": 42, ...}
```

## Debug mode

```python
initializer_logger(debug=True)
# or
PPLOGGER_DEBUG=1 python app.py
```

Debug mode lifts the root level to `DEBUG`. Per-logger filtering still works:

```python
logging.getLogger("noisy.library").setLevel(logging.WARNING)
```

## Shipping records to a time-series DB

See [`processor.md`](processor.md) for the Go shipper that tails the JSON log
file and writes records to an InfluxDB-compatible endpoint.
