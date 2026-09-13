"""Human-readable rendering for :mod:`recoverykit.contracts` reports."""

from __future__ import annotations

from typing import Any, Mapping


def format_report(report: Mapping[str, Any]) -> str:
    """Render a successful :func:`validate_catalog` report as plain text.

    The caller remains responsible for serializing the original dictionary as
    JSON when machine-readable output is wanted.
    """

    if not report.get("valid"):
        return "Contract catalog: INVALID"
    lines = [
        "Contract catalog: VALID (structure only; business verification: false)",
        "root: %s" % report.get("root", ""),
        "manifests: %s; entries: %s; unknowns: %s"
        % (report.get("manifest_count", 0), report.get("entry_count", 0), report.get("unknown_count", 0)),
    ]
    owners = report.get("owners", {})
    if isinstance(owners, Mapping):
        for owner in sorted(owners):
            details = owners[owner]
            if not isinstance(details, Mapping):
                continue
            lines.append(
                "%s: %s manifests, %s entries, %s unknowns"
                % (
                    owner,
                    details.get("manifest_count", 0),
                    details.get("entry_count", 0),
                    details.get("unknown_count", 0),
                )
            )
    return "\n".join(lines)


# Alias kept intentionally obvious for command-layer callers that prefer the
# verb used in the recovery ADR.
render_report = format_report
