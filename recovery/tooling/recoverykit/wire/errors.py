"""Fixed, non-content-bearing errors for static wire observations."""

from __future__ import annotations


ERROR_CODES = frozenset(
    (
        "invalid_document",
        "invalid_manifest",
        "source_integrity",
        "invalid_anchor",
        "catalog_reference",
        "unsafe_input",
        "input_limit",
    )
)


class WireError(ValueError):
    """An offline wire-evidence validation failure with a stable category.

    The exception deliberately carries no source path, source bytes, statement,
    or caller supplied text.  The command layer can therefore report only the
    category without turning a failed evidence review into a disclosure path.
    """

    def __init__(self, code: str) -> None:
        if code not in ERROR_CODES:
            raise ValueError("unknown wire error category")
        self.code = code
        super().__init__(code)
