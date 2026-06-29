"""pplogger - common logger for observability of distributed systems."""

from importlib.metadata import PackageNotFoundError, version

from pplogger.context import bind_context, clear_context, context, get_context
from pplogger.logger import build_log_path, initializer_logger

try:
    # Single source of truth: the version declared in pyproject.toml, read from
    # the installed distribution metadata. The distribution is named
    # "pepe-logger" on PyPI even though the import package is "pplogger".
    __version__ = version("pepe-logger")
except PackageNotFoundError:  # running from a source tree that isn't installed
    __version__ = "0.0.0+unknown"

__all__ = [
    "build_log_path",
    "initializer_logger",
    "bind_context",
    "clear_context",
    "context",
    "get_context",
    "__version__",
]
