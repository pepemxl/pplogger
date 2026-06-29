"""Logger initialization for pplogger."""

from __future__ import annotations

import datetime as dt
import logging
import os
import sys
from collections.abc import Callable
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


class DailyDatedFileHandler(logging.FileHandler):
    """A ``FileHandler`` that rolls over to a new date-stamped file at midnight.

    Unlike ``TimedRotatingFileHandler`` (which renames the active file and keeps
    a fixed base name), this preserves pplogger's convention of putting the day
    in the filename: each calendar day gets its own
    ``<service>.<module>_logs.<YYYY_MM_DD>.log``. When the local date changes,
    the current stream is closed and a new dated file is opened.

    ``clock`` is injectable for testing; it defaults to ``datetime.date.today``.
    """

    def __init__(
        self,
        service: str,
        module: str,
        log_dir: str | os.PathLike[str],
        encoding: str | None = None,
        clock: Callable[[], dt.date] | None = None,
    ) -> None:
        self._service = service
        self._module = module
        self._log_dir = log_dir
        self._clock = clock or dt.date.today
        self._current_day = self._clock()
        path = build_log_path(service, module, log_dir, self._current_day)
        super().__init__(path, encoding=encoding)

    def emit(self, record: logging.LogRecord) -> None:
        today = self._clock()
        if today != self._current_day:
            self._roll_to(today)
        super().emit(record)

    def _roll_to(self, day: dt.date) -> None:
        self.acquire()
        try:
            if self.stream:
                self.stream.close()
                self.stream = None
            self._current_day = day
            new_path = build_log_path(self._service, self._module, self._log_dir, day)
            self.baseFilename = os.path.abspath(str(new_path))
            self.stream = self._open()
        finally:
            self.release()


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
    rotate_daily: bool = False,
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
    :param rotate_daily: when True (and ``max_bytes`` is 0), roll over to a new
        date-stamped file at midnight via :class:`DailyDatedFileHandler`.
        Ignored when ``max_bytes`` > 0 (size-based rotation takes precedence).
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
    elif rotate_daily:
        file_handler = DailyDatedFileHandler(service, module, log_dir, encoding="utf-8")
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
