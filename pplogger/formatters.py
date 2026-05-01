"""JSON formatter for pplogger.

Each log record is serialized to a single-line JSON document so downstream
processors (e.g. the Go shipper in ./processor) can consume it line by line.
"""

from __future__ import annotations

import datetime as dt
import json
import logging
import os
import socket
import traceback
from typing import Any

# logging.LogRecord built-in attributes — anything not in this set is treated
# as user-supplied `extra={...}` and copied into the JSON payload.
_RESERVED_RECORD_ATTRS = frozenset(
    {
        "name", "msg", "args", "levelname", "levelno", "pathname", "filename",
        "module", "exc_info", "exc_text", "stack_info", "lineno", "funcName",
        "created", "msecs", "relativeCreated", "thread", "threadName",
        "processName", "process", "message", "asctime", "taskName",
    }
)


class JSONFormatter(logging.Formatter):
    """Serialize log records as JSON documents."""

    def __init__(self, service: str, module: str, hostname: str | None = None) -> None:
        super().__init__()
        self.service = service
        self.module = module
        self.hostname = hostname or socket.gethostname()
        self.pid = os.getpid()

    def format(self, record: logging.LogRecord) -> str:
        timestamp = dt.datetime.fromtimestamp(record.created, tz=dt.timezone.utc)
        payload: dict[str, Any] = {
            "timestamp": timestamp.isoformat(timespec="milliseconds").replace("+00:00", "Z"),
            "level": record.levelname,
            "logger": record.name,
            "message": record.getMessage(),
            "service": self.service,
            "module": self.module,
            "hostname": self.hostname,
            "pid": self.pid,
            "source_module": record.module,
            "function": record.funcName,
            "line": record.lineno,
            "thread": record.threadName,
        }

        if record.exc_info:
            exc_type, exc_value, exc_tb = record.exc_info
            payload["exception"] = {
                "type": exc_type.__name__ if exc_type else None,
                "message": str(exc_value) if exc_value else None,
                "traceback": "".join(traceback.format_exception(exc_type, exc_value, exc_tb)),
            }

        for key, value in record.__dict__.items():
            if key in _RESERVED_RECORD_ATTRS or key.startswith("_") or key in payload:
                continue
            payload[key] = _safe(value)

        return json.dumps(payload, ensure_ascii=False, default=str)


def _safe(value: Any) -> Any:
    """Best-effort conversion of `extra` values to JSON-friendly types."""
    try:
        json.dumps(value)
        return value
    except (TypeError, ValueError):
        return repr(value)
