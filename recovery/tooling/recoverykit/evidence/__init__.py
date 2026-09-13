"""Public evidence-preservation API; all functions return JSON-safe metadata."""

from .errors import EvidenceError
from .manifest import load_manifest, verify_manifest
from .preservation import preserve_manifest, verify_preserved

__all__ = ["EvidenceError", "load_manifest", "verify_manifest", "preserve_manifest",
           "verify_preserved"]
