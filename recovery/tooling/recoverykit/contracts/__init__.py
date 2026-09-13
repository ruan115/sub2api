"""Machine-checkable recovery contract catalog.

The validator proves only that local catalog records are structurally
self-consistent.  It does not prove that an online API, binary, schema, or
business behavior has been recovered.
"""

from .catalog import validate_catalog
from .loader import ContractError, load_catalog
from .report import format_report, render_report

__all__ = (
    "ContractError",
    "format_report",
    "load_catalog",
    "render_report",
    "validate_catalog",
)
