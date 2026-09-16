"""Explicit paths and operations; no implicit server, upload or restore."""
import argparse


class CLIUsageError(ValueError):
    """Invalid arguments; details may contain credentials and are not echoed."""


class SafeArgumentParser(argparse.ArgumentParser):
    def error(self, message):
        raise CLIUsageError("Invalid arguments; use --help for command syntax")


def build_parser():
    parser = SafeArgumentParser(
        prog="recoverykit",
        description="Offline recovery evidence and contract tools (no target execution).",
        allow_abbrev=False,
    )
    commands = parser.add_subparsers(dest="command", required=True)

    contracts = commands.add_parser("contracts", help="Validate discovery catalog structure")
    contracts.add_argument("--root", required=True, help="Contract catalog directory")

    lab = commands.add_parser("lab", help="Read-only explicit local Docker endpoint checks; never execution approval", allow_abbrev=False)
    lab_ops = lab.add_subparsers(dest="operation", required=True)
    inspect = lab_ops.add_parser("inspect", allow_abbrev=False)
    inspect.add_argument("--socket", required=True, help="Absolute socket in a user-private dedicated lab directory")
    inspect.add_argument("--expected-engine-id", required=True, help="Previously confirmed ID; not auto-discovered")
    inspect.add_argument("--expected-engine-name", required=True)
    inspect.add_argument("--expected-architecture", required=True, choices=("amd64", "arm64"))

    wire = commands.add_parser("wire", help="Anchor reviewed static observations to source bytes", allow_abbrev=False)
    wire_ops = wire.add_subparsers(dest="operation", required=True)
    wire_verify = wire_ops.add_parser("verify", allow_abbrev=False)
    wire_verify.add_argument("--observations", required=True, help="Reviewed observations JSON")
    wire_verify.add_argument("--manifest", required=True, help="Allowlisted artifact manifest")
    wire_verify.add_argument("--source-root", required=True, help="Private reviewed source directory")
    wire_verify.add_argument("--catalog-root", required=True, help="Existing discovery catalog")

    evidence = commands.add_parser("evidence", help="Verify or preserve allowlisted evidence")
    evidence_ops = evidence.add_subparsers(dest="operation", required=True)
    for operation in ("verify", "preserve"):
        command = evidence_ops.add_parser(operation)
        command.add_argument("--manifest", required=True)
        command.add_argument("--source-root", required=True)
        if operation == "preserve":
            command.add_argument("--destination", required=True, help="Absolute new private directory")
    preserved = evidence_ops.add_parser("verify-preserved")
    preserved.add_argument("--directory", required=True)

    workspace = commands.add_parser("workspace", help="Snapshot or verify WIP without restoring it")
    workspace_ops = workspace.add_subparsers(dest="operation", required=True)
    snapshot = workspace_ops.add_parser("snapshot")
    snapshot.add_argument("--repo", required=True)
    snapshot.add_argument("--destination", required=True, help="Absolute new directory outside the repo")
    verify = workspace_ops.add_parser("verify")
    verify.add_argument("--directory", required=True)
    return parser
