"""The services a worker can run: each maps its operation names (the `op`
of operations.json) to handlers. A handler gets only what passed every
check of spec §7.3: the Verified message, never the raw body."""
