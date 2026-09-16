"""Compose module APIs and emit small, non-secret JSON summaries."""
import json
from pathlib import Path
import sys

from ..contracts import validate_catalog
from ..evidence import load_manifest, preserve_manifest, verify_manifest, verify_preserved
from ..evidence.policy import check_content
from ..lab import inspect_lab
from ..lab.config import LabConfig
from ..workspace import snapshot_workspace, verify_snapshot
from ..wire import validate_observations
from .parser import build_parser


def dispatch(args):
    if args.command == "lab":
        return inspect_lab(LabConfig(Path(args.socket), args.expected_engine_id,
                                    args.expected_engine_name, args.expected_architecture))
    if args.command == "contracts":
        return validate_catalog(Path(args.root))
    if args.command == "wire":
        return validate_observations(Path(args.observations), Path(args.manifest),
                                     Path(args.source_root), Path(args.catalog_root))
    if args.command == "evidence":
        if args.operation == "verify-preserved":
            return verify_preserved(Path(args.directory))
        manifest = load_manifest(Path(args.manifest))
        if args.operation == "verify":
            return verify_manifest(manifest, Path(args.source_root))
        return preserve_manifest(manifest, Path(args.source_root), Path(args.destination))
    if args.operation == "snapshot":
        return snapshot_workspace(Path(args.repo), Path(args.destination))
    return verify_snapshot(Path(args.directory))


def summarize(result):
    # Do not echo entry paths, source descriptions, patches or exception text.
    allowed = {
        "status", "valid", "manifest_id", "file_count", "total_bytes", "head", "branch",
        "counts", "manifest_count", "entry_count", "owners", "statuses",
        "business_verification", "tracked_count", "untracked_count",
        "isolation_verified", "execution_permitted", "created_resources",
        "container_count", "volume_count", "network_count",
    }
    return {key: value for key, value in result.items() if key in allowed}


def main(argv=None, stdout=None, stderr=None):
    stdout = sys.stdout if stdout is None else stdout
    stderr = sys.stderr if stderr is None else stderr
    try:
        args = build_parser().parse_args(argv)
        result = dispatch(args)
        encoded = json.dumps({"ok": True, **summarize(result)}, sort_keys=True)
        # Branch names and manifest IDs are still untrusted text. Screen even
        # the allowlisted metadata before any bytes reach stdout.
        check_content(encoded.encode("utf-8"), text_only=False)
    except Exception as error:
        # A malicious artifact/path/branch could include credentials. Report the
        # error category only; module tests exercise detailed diagnostics offline.
        json.dump({"ok": False, "error_type": type(error).__name__}, stderr)
        stderr.write("\n")
        return 2
    stdout.write(encoded + "\n")
    return 0
