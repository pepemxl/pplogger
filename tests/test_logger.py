"""Tests for pplogger."""

from __future__ import annotations

import datetime as dt
import json
import logging
from pathlib import Path

import pytest
from pplogger import (
    bind_context,
    build_log_path,
    clear_context,
    context,
    initializer_logger,
)


@pytest.fixture(autouse=True)
def _reset_root_logger():
    yield
    root = logging.getLogger()
    for handler in list(root.handlers):
        root.removeHandler(handler)
        handler.close()
    clear_context()


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
    log_path = initializer_logger(service="svc", module="mod", log_dir=tmp_path, console=False)
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
    log_path = initializer_logger(service="svc", module="mod", log_dir=tmp_path, console=False)
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
    log_path = initializer_logger(service="svc", module="mod", log_dir=tmp_path, console=False)
    logging.getLogger("test.extra").info("with extras", extra={"request_id": "abc-123"})

    record = _read_records(log_path)[0]
    assert record["request_id"] == "abc-123"


def test_initializer_logger_is_idempotent(tmp_path):
    initializer_logger(service="svc", module="mod", log_dir=tmp_path, console=False)
    log_path = initializer_logger(service="svc", module="mod", log_dir=tmp_path, console=False)

    root = logging.getLogger()
    pplogger_handlers = [h for h in root.handlers if getattr(h, "_pplogger", False)]
    assert len(pplogger_handlers) == 1

    logging.getLogger("test.idem").info("once")
    assert len(_read_records(log_path)) == 1


def test_console_handler_emits_json(tmp_path, capsys):
    initializer_logger(service="svc", module="mod", log_dir=tmp_path, console=True)
    logging.getLogger("test.console").info("on stdout")

    out = capsys.readouterr().out
    record = json.loads(out.strip().splitlines()[-1])
    assert record["message"] == "on stdout"


def test_explicit_level_overrides_debug(tmp_path):
    log_path = initializer_logger(
        service="svc", module="mod", log_dir=tmp_path, console=False, level="WARNING"
    )
    log = logging.getLogger("test.level")
    log.info("dropped")
    log.warning("kept")

    records = _read_records(log_path)
    assert [r["message"] for r in records] == ["kept"]


def test_unknown_level_raises(tmp_path):
    with pytest.raises(ValueError):
        initializer_logger(
            service="svc", module="mod", log_dir=tmp_path, console=False, level="NOPE"
        )


def test_non_serializable_extra_is_stringified(tmp_path):
    log_path = initializer_logger(service="svc", module="mod", log_dir=tmp_path, console=False)
    sentinel = object()
    logging.getLogger("test.safe").info("weird", extra={"obj": sentinel})

    record = _read_records(log_path)[0]
    assert isinstance(record["obj"], str)
    assert "object" in record["obj"]


def test_context_fields_merged_and_overridable(tmp_path):
    log_path = initializer_logger(service="svc", module="mod", log_dir=tmp_path, console=False)
    log = logging.getLogger("test.ctx")

    bind_context(request_id="r-1")
    log.info("a")
    with context(trace_id="t-1"):
        log.info("b")
    log.info("c", extra={"request_id": "override"})

    recs = _read_records(log_path)
    assert recs[0]["request_id"] == "r-1" and "trace_id" not in recs[0]
    assert recs[1]["request_id"] == "r-1" and recs[1]["trace_id"] == "t-1"
    assert "trace_id" not in recs[2]  # context manager restored on exit
    assert recs[2]["request_id"] == "override"  # explicit extra wins over context


def test_hostname_override(tmp_path):
    log_path = initializer_logger(
        service="svc", module="mod", log_dir=tmp_path, console=False, hostname="edge-1"
    )
    logging.getLogger("test.host").info("hi")
    assert _read_records(log_path)[0]["hostname"] == "edge-1"


def test_size_based_rotation(tmp_path):
    log_path = initializer_logger(
        service="svc",
        module="mod",
        log_dir=tmp_path,
        console=False,
        max_bytes=300,
        backup_count=2,
    )
    log = logging.getLogger("test.rot")
    for i in range(50):
        log.info("padded message number %d %s", i, "x" * 20)

    backups = list(tmp_path.glob(log_path.name + ".*"))
    assert backups, "expected at least one rotated backup file"
