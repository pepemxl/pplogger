# PPLogger

A logger package for scaled distributed systems.

The goal is to standarize logs in distribuited systems and permit scale logging process.

This package converts log records into JSON messages.

## Install

```bash
pip install pepe-logger
```

The PyPI distribution is named **`pepe-logger`**; the importable package is
**`pplogger`** (`from pplogger import initializer_logger`).

An example of a log file:
- `/tmp/service_api.module_pepe_logs.2024_07_13.log`

Example of usage:

```python
    import logging
    from pplogger import initializer_logger

    initializer_logger()
    log = logging.getLogger(__name__)

    log.info("This log info message")
    log.debug("This log info message")
    log.error("This log error message")
    log.exception("This log exception message")
```