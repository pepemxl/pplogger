"""Per-context structured fields for log correlation.

Fields bound here (e.g. ``request_id``, ``trace_id``) are merged into every log
record emitted from the same thread / asyncio task, so callers don't have to
thread them through ``extra={...}`` on every log call. Explicit ``extra`` fields
take precedence over bound context fields.
"""

from __future__ import annotations

import contextlib
from collections.abc import Iterator
from contextvars import ContextVar
from typing import Any

_context: ContextVar[dict[str, Any] | None] = ContextVar("pplogger_context", default=None)


def get_context() -> dict[str, Any]:
    """Return a copy of the fields currently bound to this context."""
    return dict(_context.get() or {})


def bind_context(**fields: Any) -> None:
    """Merge ``fields`` into the current context (until overwritten/cleared)."""
    _context.set({**get_context(), **fields})


def clear_context() -> None:
    """Remove all bound context fields."""
    _context.set(None)


@contextlib.contextmanager
def context(**fields: Any) -> Iterator[None]:
    """Bind ``fields`` for the duration of the ``with`` block, then restore."""
    token = _context.set({**get_context(), **fields})
    try:
        yield
    finally:
        _context.reset(token)
