"""Errors intentionally contain metadata, never evidence contents."""


class EvidenceError(ValueError):
    """The requested evidence operation could not be completed safely."""
