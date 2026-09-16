"""Explicit offline staging; no download, Docker invocation, build or run."""
import json
from pathlib import Path
import sys

from recoverykit.cli.parser import SafeArgumentParser

from imagekit.context import stage_context, verify_context


def main(argv=None, stdout=None, stderr=None):
    stdout = sys.stdout if stdout is None else stdout
    stderr = sys.stderr if stderr is None else stderr
    try:
        parser = SafeArgumentParser(description=__doc__, allow_abbrev=False)
        commands = parser.add_subparsers(dest="operation", required=True)
        stage = commands.add_parser("stage", allow_abbrev=False)
        stage.add_argument("--lock", required=True)
        stage.add_argument("--source-root", required=True)
        stage.add_argument("--destination", required=True)
        verify = commands.add_parser("verify", allow_abbrev=False)
        verify.add_argument("--directory", required=True)
        args = parser.parse_args(argv)
        if args.operation == "stage":
            result = stage_context(Path(args.lock), Path(args.source_root), Path(args.destination))
        else:
            result = verify_context(Path(args.directory))
        # Context functions have a fixed schema. Do not echo arbitrary fields
        # should a future implementation attach paths or artifact metadata.
        summary = {key: result[key] for key in (
            "status", "image_built", "execution_permitted", "artifact_count", "total_bytes")}
        if (summary["status"] != "base_context_verified" or summary["image_built"] is not False
                or summary["execution_permitted"] is not False
                or type(summary["artifact_count"]) is not int or type(summary["total_bytes"]) is not int):
            raise ValueError("unexpected_stage_summary")
        stdout.write(json.dumps({"ok": True, **summary}, sort_keys=True) + "\n")
        return 0
    except Exception as error:
        stderr.write(json.dumps({"ok": False, "error_type": type(error).__name__}) + "\n")
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
