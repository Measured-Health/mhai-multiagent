"""Retry decorator with exponential backoff."""
import functools
import time
import logging

log = logging.getLogger(__name__)

def retry(max_attempts=3, backoff=2.0, exceptions=(Exception,)):
    def decorator(func):
        @functools.wraps(func)
        def wrapper(*args, **kwargs):
            for attempt in range(max_attempts):
                try:
                    return func(*args, **kwargs)
                except exceptions as e:
                    if attempt == max_attempts - 1:
                        raise
                    wait = backoff ** attempt
                    log.warning(f"{func.__name__} attempt {attempt+1} failed: {e}. Retrying in {wait}s")
                    time.sleep(wait)
        return wrapper
    return decorator
