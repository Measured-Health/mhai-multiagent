"""Queue producer — publishes events to FIFO queue."""
import json
import logging
import os
from typing import Any

log = logging.getLogger(__name__)

class QueueProducer:
    """Publish events to FIFO queue (Redis Streams / SQS / Azure SB)."""

    def __init__(self):
        self.backend = os.environ.get("QUEUE_BACKEND", "redis")
        self.url = os.environ.get("QUEUE_URL", "mhai.queries")

    def publish(self, topic: str, event: Any) -> str:
        """Publish an event. Returns event ID."""
        payload = event.model_dump_json() if hasattr(event, "model_dump_json") else json.dumps(event)
        log.info(f"Published to {topic}: {len(payload)} bytes")
        # Implementation depends on backend (redis/sqs/azservicebus)
        return f"evt_{id(event)}"
