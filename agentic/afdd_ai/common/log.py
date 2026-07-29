"""Logging setup shared by the Python services (key=value lines, like the Go services)."""

from __future__ import annotations

import logging
import sys

from .config import get_settings

_configured = False


def setup_logging(service: str) -> logging.Logger:
    """Configure root logging once; returns the service logger."""
    global _configured
    if not _configured:
        handler = logging.StreamHandler(sys.stdout)
        handler.setFormatter(logging.Formatter(f"time=%(asctime)s level=%(levelname)s service={service} logger=%(name)s msg=%(message)s"))
        root = logging.getLogger()
        root.handlers[:] = [handler]
        root.setLevel(get_settings().log_level)
        # Third-party chatter.
        for noisy in ("httpx", "httpcore", "urllib3", "google_genai", "google.genai", "asyncio", "mcp", "sse_starlette"):
            logging.getLogger(noisy).setLevel(logging.WARNING)
        _configured = True
    return logging.getLogger(service)
