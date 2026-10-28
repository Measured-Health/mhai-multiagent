"""Environment settings for the boundary.

Only the environment rule lives here for now; the worker's settings arrive
with the worker (Task 9). Like core's CORE_ENV, an unset APP_ENV counts as
production: the development relaxations (test-only operations) are on only
when development is named.
"""

from __future__ import annotations

import os
from collections.abc import Mapping


def is_development(environ: Mapping[str, str] | None = None) -> bool:
    """True only when APP_ENV names development (case and spaces ignored)."""
    env = os.environ if environ is None else environ
    return env.get("APP_ENV", "production").strip().lower() == "development"
