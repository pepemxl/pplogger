"""Logger initialization for pplogger."""

from __future__ import annotations

import datetime as dt
import logging
import os
import sys
from logging.handlers import RotatingFileHandler
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
    level: int | str | None = None,
    max_bytes: int = 0,
    backup_count: int = 0,
    hostname: str | None = None,
) -> Path:
    """Configure the root logger to emit JSON records to a daily file.

    Returns the path of the active log file so callers can hand it to the
    Go shipper or to tests.

    :param level: explicit level (e.g. ``logging.WARNING`` or ``"WARNING"``).
        Takes precedence over ``debug`` when given.
    :param max_bytes: when > 0, rotate the file once it reaches this size using
        ``RotatingFileHandler`` instead of a plain ``FileHandler``.
    :param backup_count: number of rotated backups to keep (only with
        ``max_bytes``).
    :param hostname: override the ``hostname`` field (defaults to
        ``socket.gethostname()``); useful in containers where the host id is
        injected via the environment.
    """
    log_path = build_log_path(service=service, module=module, log_dir=log_dir)
    log_path.parent.mkdir(parents=True, exist_ok=True)

    resolved_level = _resolve_level(level, debug)
    formatter = JSONFormatter(
        service=service,
        module=module,
        hostname=hostname or os.environ.get("PPLOGGER_HOSTNAME"),
    )

    root = logging.getLogger()
    root.setLevel(resolved_level)

    # Replace any handlers we previously installed so repeat calls are idempotent.
    for handler in list(root.handlers):
        if getattr(handler, "_pplogger", False):
            root.removeHandler(handler)
            handler.close()

    file_handler: logging.FileHandler
    if max_bytes > 0:
        file_handler = RotatingFileHandler(
            log_path, maxBytes=max_bytes, backupCount=backup_count, encoding="utf-8"
        )
    else:
        file_handler = logging.FileHandler(log_path, encoding="utf-8")
    file_handler.setLevel(resolved_level)
    file_handler.setFormatter(formatter)
    file_handler._pplogger = True  # type: ignore[attr-defined]
    root.addHandler(file_handler)

    if console:
        stream_handler = logging.StreamHandler(stream=sys.stdout)
        stream_handler.setLevel(resolved_level)
        stream_handler.setFormatter(formatter)
        stream_handler._pplogger = True  # type: ignore[attr-defined]
        root.addHandler(stream_handler)

    return log_path


def _resolve_level(level: int | str | None, debug: bool) -> int:
    """Resolve the effective numeric level from explicit/debug/env inputs."""
    if level is not None:
        if isinstance(level, str):
            num = logging.getLevelName(level.upper())
            if not isinstance(num, int):
                raise ValueError(f"unknown log level: {level!r}")
            return num
        return int(level)
    return logging.DEBUG if debug or _env_truthy("PPLOGGER_DEBUG") else logging.INFO


def _env_truthy(name: str) -> bool:
    return os.environ.get(name, "").strip().lower() in {"1", "true", "yes", "on"}
