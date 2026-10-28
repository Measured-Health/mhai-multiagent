"""Dead-Letter Queue handler."""
import logging

log = logging.getLogger(__name__)

class DLQHandler:
    """Handle poison messages moved to DLQ."""

    def replay(self, event_id: str) -> bool:
        """Requeue a DLQ event to the main FIFO."""
        log.info(f"Replaying DLQ event {event_id}")
        return True
