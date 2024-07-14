""""
This module specifies common log for observability and monitoring of distribuited systems.

Each module should log:

- Things working fine
- Errors
- Exceptions

Also enable debug mode.

This module converts log records into JSON messages.

An example of a log file:
- /tmp/service_api.module_pepe_logs.2024_07_13.log

Example of usage:

    import logging
    from pplogger import initializer_logger

    initializer_logger()
    log = logging.getLogger(__name__)

    log.info("This log info message")
    log.debug("This log info message")
    log.error("This log error message")
    log.exception("This log exception message")

"""

