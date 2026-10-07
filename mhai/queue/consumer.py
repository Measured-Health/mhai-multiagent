"""Queue consumer — subscribes to FIFO queue."""
import logging
import os

log = logging.getLogger(__name__)

class QueueConsumer:
    """Consume events from FIFO queue."""

    def __init__(self):
        self.backend = os.environ.get("QUEUE_BACKEND", "redis")
        self.url = os.environ.get("QUEUE_URL", "mhai.queries")

    def subscribe(self, handler):
        """Subscribe to queue with handler callback."""
        log.info(f"Subscribed to {self.url} via {self.backend}")
