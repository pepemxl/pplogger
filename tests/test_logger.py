"""Tests for pplogger."""

from __future__ import annotations

import datetime as dt
import json
import logging
from pathlib import Path

import pytest

from pplogger import build_log_path, initializer_logger


@pytest.fixture(autouse=True)
def _reset_root_logger():
    yield
    root = logging.getLogger()
    for handler in list(root.handlers):
        root.removeHandler(handler)
        handler.close()


def _read_records(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def test_build_log_path_format():
    path = build_log_path(
        service="service_api",
        module="module_pepe",
        log_dir="/tmp",
        when=dt.date(2024, 7, 13),
    )
    assert str(path) == "/tmp/service_api.module_pepe_logs.2024_07_13.log"


def test_initializer_logger_emits_json(tmp_path):
    log_path = initializer_logger(
        service="svc", module="mod", log_dir=tmp_path, console=False
    )
    log = logging.getLogger("test.json")
    log.info("hello")

    records = _read_records(log_path)
    assert len(records) == 1
    assert records[0]["level"] == "INFO"
    assert records[0]["message"] == "hello"
    assert records[0]["service"] == "svc"
    assert records[0]["module"] == "mod"
    assert "timestamp" in records[0]


def test_debug_level_toggle(tmp_path):
    log_path = initializer_logger(
        service="svc", module="mod", log_dir=tmp_path, console=False, debug=False
    )
    logging.getLogger("test.debug").debug("ignored")
    assert _read_records(log_path) == []

    log_path = initializer_logger(
        service="svc", module="mod", log_dir=tmp_path, console=False, debug=True
    )
    logging.getLogger("test.debug").debug("kept")
    records = _read_records(log_path)
    assert any(r["message"] == "kept" and r["level"] == "DEBUG" for r in records)


def test_exception_serialized(tmp_path):
    log_path = initializer_logger(
        service="svc", module="mod", log_dir=tmp_path, console=False
    )
    log = logging.getLogger("test.exc")
    try:
        raise ValueError("boom")
    except ValueError:
        log.exception("failure")

    record = _read_records(log_path)[0]
    assert record["level"] == "ERROR"
    assert record["exception"]["type"] == "ValueError"
    assert record["exception"]["message"] == "boom"
    assert "Traceback" in record["exception"]["traceback"]


def test_extra_fields_preserved(tmp_path):
    log_path = initializer_logger(
        service="svc", module="mod", log_dir=tmp_path, console=False
    )
    logging.getLogger("test.extra").info("with extras", extra={"request_id": "abc-123"})

    record = _read_records(log_path)[0]
    assert record["request_id"] == "abc-123"
