"""Logger initialization for pplogger."""

from __future__ import annotations

import datetime as dt
import logging
import os
import sys
from pathlib import Path

from pplogger.formatters import JSONFormatter

DEFAULT_LOG_DIR = Path(os.environ.get("PPLOGGER_DIR", "/tmp"))
DEFAULT_SERVICE = os.environ.get("PPLOGGER_SERVICE", "service_api")
DEFAULT_MODULE = os.environ.get("PPLOGGER_MODULE", "module_pepe")


def build_log_path(
    service: str = DEFAULT_SERVICE,
    module: str = DEFAULT_MODULE,
    log_dir: str | os.PathLike[str] = DEFAULT_LOG_DIR,
    when: dt.date | None = None,
) -> Path:
    """Return the daily log file path, e.g. /tmp/service_api.module_pepe_logs.2024_07_13.log"""
    day = (when or dt.date.today()).strftime("%Y_%m_%d")
    return Path(log_dir) / f"{service}.{module}_logs.{day}.log"


def initializer_logger(
    service: str = DEFAULT_SERVICE,
    module: str = DEFAULT_MODULE,
    log_dir: str | os.PathLike[str] = DEFAULT_LOG_DIR,
    debug: bool = False,
    console: bool = True,
) -> Path:
    """Configure the root logger to emit JSON records to a daily file.

    Returns the path of the active log file so callers can hand it to the
    Go shipper or to tests.
    """
    log_path = build_log_path(service=service, module=module, log_dir=log_dir)
    log_path.parent.mkdir(parents=True, exist_ok=True)

    level = logging.DEBUG if debug or _env_truthy("PPLOGGER_DEBUG") else logging.INFO
    formatter = JSONFormatter(service=service, module=module)

    root = logging.getLogger()
    root.setLevel(level)

    # Replace any handlers we previously installed so repeat calls are idempotent.
    for handler in list(root.handlers):
        if getattr(handler, "_pplogger", False):
            root.removeHandler(handler)
            handler.close()

    file_handler = logging.FileHandler(log_path, encoding="utf-8")
    file_handler.setLevel(level)
    file_handler.setFormatter(formatter)
    file_handler._pplogger = True  # type: ignore[attr-defined]
    root.addHandler(file_handler)

    if console:
        stream_handler = logging.StreamHandler(stream=sys.stdout)
        stream_handler.setLevel(level)
        stream_handler.setFormatter(formatter)
        stream_handler._pplogger = True  # type: ignore[attr-defined]
        root.addHandler(stream_handler)

    return log_path


def _env_truthy(name: str) -> bool:
    return os.environ.get(name, "").strip().lower() in {"1", "true", "yes", "on"}
