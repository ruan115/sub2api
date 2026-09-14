#!/usr/bin/env python3
"""Preserve a reviewed, fixed-scope frontend privately; never execute or publish it.

Use PYTHONPATH=recovery/tooling. Inventory transmits metadata only. Capture
requires that exact reviewed inventory and screens every byte before storage.
Quarantine is deliberately NOT evidence-policy acceptance or execution approval.
"""

from __future__ import annotations

import argparse
import base64
import binascii
from datetime import datetime, timezone
import hashlib
import os
from pathlib import Path
import re
import shlex
import sys

from recoverykit.evidence.errors import EvidenceError
from recoverykit.evidence.filesystem import (
    absolute_path, create_private_destination, json_bytes, load_json_bytes,
    private_inventory, read_regular, write_exclusive,
)
from recoverykit.evidence.policy import check_content
from recoverykit.workspace.process import ProcessError, run_bounded

SOURCE = {"host": "216.106.185.119", "user": "root", "root": "/opt/gateway/public"}
EXTRA_PATHS = (
    "favicon.ico", "logo.svg", "images/customer-service-qrcode.jpg",
    "images/customer-service-qrcode2.jpg", "images/logo-black.png",
    "images/logo.png", "images/logo-white.png",
    "textures/earth/earth_diffuse_2k.avif", "textures/earth/earth_normal_2k.avif",
    "textures/earth/earth_specular_2k.avif",
)
MAX_FILE = 4 * 1024 * 1024
MAX_TOTAL = 16 * 1024 * 1024
MAX_ENTRIES = 512
MAX_METADATA = 1024 * 1024
MAX_OUTPUT = 32 * 1024 * 1024
ASSET = re.compile(r"assets/[A-Za-z0-9_][A-Za-z0-9_.-]{0,179}\.(?:js|css)\Z")
HEX = re.compile(r"[0-9a-f]{64}\Z")

# This source is sent in one SSH command, not written on the server. All paths,
# limits and allowed extensions are constants, not CLI-supplied remote paths.
REMOTE_SOURCE = r'''
import base64, hashlib, json, os, re, stat, sys
ROOT = "/opt/gateway/public"
EXTRA = __EXTRA_PATHS__
MAX_FILE = 4194304
MAX_TOTAL = 16777216
MAX_ENTRIES = 512
ASSET = re.compile(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,179}\.(?:js|css)\Z")

def fingerprint(info):
    return (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns)

def directory(root):
    fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for part in root.strip("/").split("/"):
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = child
        return fd
    except BaseException:
        os.close(fd)
        raise

def read_file(root_fd, name):
    fd = os.dup(root_fd)
    file_fd = None
    try:
        for part in name.split("/")[:-1]:
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = child
        before_dir = fingerprint(os.fstat(fd))
        file_fd = os.open(name.split("/")[-1], os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=fd)
        before = os.fstat(file_fd)
        if not stat.S_ISREG(before.st_mode) or not 0 <= before.st_size <= MAX_FILE:
            raise ValueError("source_file_rejected")
        chunks, total = [], 0
        while True:
            chunk = os.read(file_fd, min(65536, MAX_FILE + 1 - total))
            if not chunk:
                break
            chunks.append(chunk)
            total += len(chunk)
            if total > MAX_FILE:
                raise ValueError("source_size_rejected")
        if (fingerprint(before) != fingerprint(os.fstat(file_fd))
                or before_dir != fingerprint(os.fstat(fd))):
            raise ValueError("source_changed")
        return b"".join(chunks)
    finally:
        if file_fd is not None:
            os.close(file_fd)
        os.close(fd)

def collect(mode, reviewed):
    if mode not in ("inventory", "capture"):
        raise ValueError("invalid_mode")
    root_fd = directory(ROOT)
    assets_fd = None
    try:
        before_root = fingerprint(os.fstat(root_fd))
        assets_fd = os.open("assets", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=root_fd)
        before_assets = fingerprint(os.fstat(assets_fd))
        paths = ["index.html"]
        with os.scandir(assets_fd) as iterator:
            for number, entry in enumerate(iterator):
                if number >= 2048:
                    raise ValueError("directory_budget_rejected")
                if entry.name.endswith((".js", ".css")):
                    if not ASSET.fullmatch(entry.name):
                        raise ValueError("source_name_rejected")
                    paths.append("assets/" + entry.name)
                    if len(paths) > MAX_ENTRIES:
                        raise ValueError("source_count_rejected")
        # Only these individually reviewed paths; no root/images tree walk.
        for name in EXTRA:
            try:
                read_file(root_fd, name)
            except FileNotFoundError:
                continue
            paths.append(name)
        if len(paths) > MAX_ENTRIES:
            raise ValueError("source_count_rejected")
        entries, contents, total = [], {}, 0
        for name in sorted(paths):
            raw = read_file(root_fd, name)
            total += len(raw)
            if total > MAX_TOTAL:
                raise ValueError("source_total_rejected")
            entries.append({"path": name, "bytes": len(raw), "sha256": hashlib.sha256(raw).hexdigest()})
            if mode == "capture":
                contents[name] = base64.b64encode(raw).decode("ascii")
        if (before_root != fingerprint(os.fstat(root_fd))
                or before_assets != fingerprint(os.fstat(assets_fd))):
            raise ValueError("source_changed")
        inventory = {"schema_version": 1, "kind": "portunex.frontend.inventory",
                     "source": {"host": "216.106.185.119", "user": "root", "root": ROOT},
                     "entries": entries}
        if mode == "capture" and inventory != reviewed:
            raise ValueError("reviewed_inventory_mismatch")
        return {"inventory": inventory, "contents": contents}
    finally:
        if assets_fd is not None:
            os.close(assets_fd)
        os.close(root_fd)

try:
    result = collect(sys.argv[1], json.loads(sys.argv[2]))
    sys.stdout.write(json.dumps(result, ensure_ascii=True, separators=(",", ":")))
except BaseException:
    # No source paths, content, command arguments, or exception data in errors.
    sys.stderr.write("frontend_source_rejected\n")
    sys.exit(1)
'''.replace("__EXTRA_PATHS__", repr(EXTRA_PATHS))


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def validate_inventory(value):
    if (type(value) is not dict or set(value) != {"schema_version", "kind", "source", "entries"}
            or type(value["schema_version"]) is not int or value["schema_version"] != 1
            or value["kind"] != "portunex.frontend.inventory" or value["source"] != SOURCE):
        raise EvidenceError("invalid frontend inventory")
    entries = value["entries"]
    if type(entries) is not list or not 1 <= len(entries) <= MAX_ENTRIES:
        raise EvidenceError("invalid frontend inventory entries")
    paths, total = [], 0
    for entry in entries:
        if type(entry) is not dict or set(entry) != {"path", "bytes", "sha256"}:
            raise EvidenceError("invalid frontend entry")
        name = entry["path"]
        if (type(name) is not str or not (name == "index.html" or name in EXTRA_PATHS or ASSET.fullmatch(name))
                or type(entry["bytes"]) is not int or not 0 <= entry["bytes"] <= MAX_FILE
                or type(entry["sha256"]) is not str or not HEX.fullmatch(entry["sha256"])):
            raise EvidenceError("invalid frontend entry")
        total += entry["bytes"]
        paths.append(name)
    if paths != sorted(set(paths)) or "index.html" not in paths or total > MAX_TOTAL:
        raise EvidenceError("invalid frontend inventory bounds")
    return value


def classify(name, raw):
    suffix = Path(name).suffix
    binary = suffix in {".ico", ".jpg", ".png", ".avif"}
    try:
        check_content(raw, text_only=not binary)
    except EvidenceError as error:
        reason = ("recognisable_secret_material" if str(error) == "recognisable secret material is forbidden"
                  else "invalid_text")
        return "quarantine", reason
    if binary:
        magic = {".ico": b"\x00\x00\x01\x00", ".jpg": b"\xff\xd8\xff", ".png": b"\x89PNG\r\n\x1a\n"}
        if suffix == ".avif":
            box_size = int.from_bytes(raw[:4], "big")
            matched = (16 <= box_size <= min(len(raw), 4096) and raw[4:8] == b"ftyp"
                       and (raw[8:12] in {b"avif", b"avis"}
                            or any(raw[i:i + 4] in {b"avif", b"avis"} for i in range(16, box_size - 3, 4))))
        else:
            matched = raw.startswith(magic[suffix])
        if not matched:
            return "quarantine", "image_signature_mismatch"
        return "reference", "binary_reference_not_text_screened"
    return "files", "text_policy_passed_not_execution_approval"


def ssh_command(mode, identity_file, interface, reviewed):
    identity = absolute_path(identity_file, required=True)
    if type(interface) is not str or not re.fullmatch(r"[A-Za-z][A-Za-z0-9_.:-]{0,31}", interface):
        raise EvidenceError("invalid bind interface")
    command = ["/usr/bin/ssh", "-T", "-F", "/dev/null", "-i", str(identity),
               "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes",
               "-o", "ConnectTimeout=8", "-o", "ClearAllForwardings=yes", "-o", "ForwardAgent=no",
               "-o", "PermitLocalCommand=no", "-o", "ProxyCommand=none", "-o", "ProxyJump=none",
               "-o", "BindInterface=" + interface, "root@216.106.185.119"]
    arguments = ["/usr/bin/python3", "-I", "-c", REMOTE_SOURCE, mode, json_bytes(reviewed).decode()]
    return command + [" ".join(shlex.quote(part) for part in arguments)]


def request_remote(mode, identity_file, interface, reviewed):
    command = ssh_command(mode, identity_file, interface, reviewed)
    result = run_bounded(command, environment={"PATH": "/usr/bin:/bin", "LC_ALL": "C"},
                         stdout_limit=MAX_OUTPUT, stderr_limit=65536, timeout_seconds=60)
    if result.returncode != 0:
        raise EvidenceError("frontend remote collection failed")
    value = load_json_bytes(result.stdout)
    if type(value) is not dict or set(value) != {"inventory", "contents"}:
        raise EvidenceError("invalid remote envelope")
    validate_inventory(value["inventory"])
    if mode == "capture" and value["inventory"] != reviewed:
        raise EvidenceError("reviewed inventory changed")
    return value


def preserve(destination, mode, envelope):
    if mode not in {"inventory", "capture"}:
        raise EvidenceError("invalid preservation mode")
    if type(envelope) is not dict or set(envelope) != {"inventory", "contents"}:
        raise EvidenceError("invalid remote envelope")
    inventory = validate_inventory(envelope["inventory"])
    contents = envelope["contents"]
    expected = {entry["path"] for entry in inventory["entries"]} if mode == "capture" else set()
    if type(contents) is not dict or set(contents) != expected:
        raise EvidenceError("capture file set mismatch")
    decoded, records = {}, []
    for entry in inventory["entries"] if mode == "capture" else []:
        name, encoded = entry["path"], contents[entry["path"]]
        if type(encoded) is not str or len(encoded) > ((MAX_FILE + 2) // 3) * 4:
            raise EvidenceError("invalid captured bytes")
        try:
            raw = base64.b64decode(encoded, validate=True)
        except (ValueError, binascii.Error) as error:
            raise EvidenceError("invalid captured bytes") from error
        if len(raw) != entry["bytes"] or digest(raw) != entry["sha256"]:
            raise EvidenceError("capture fingerprint mismatch")
        category, reason = classify(name, raw)
        stored_path = category + "/" + name
        decoded[stored_path] = raw
        records.append({"path": name, "stored_path": stored_path, "category": category, "reason": reason})
    inventory_raw = json_bytes(inventory)
    receipt = {
        "schema_version": 1, "kind": "portunex.frontend.private_preservation", "mode": mode,
        "captured_at": datetime.now(timezone.utc).isoformat(),
        "collector_sha256": digest(read_regular(Path(__file__).resolve().parent, Path(__file__).name, MAX_METADATA)),
        "inventory_sha256": digest(inventory_raw), "records": records,
        "source_executed": False, "business_rows_read": False,
        "approved_for_execution": False, "approved_for_publication": False,
        "original_source_project_recovered": False,
    }
    receipt_raw = json_bytes(receipt)
    if max(len(inventory_raw), len(receipt_raw)) > MAX_METADATA:
        raise EvidenceError("frontend metadata exceeds limit")
    destination = absolute_path(destination, required=True)
    create_private_destination(destination, forbidden_roots=(Path(__file__).resolve().parents[3],))
    write_exclusive(destination, "inventory.json", inventory_raw)
    for name, raw in decoded.items():
        write_exclusive(destination, name, raw)
    write_exclusive(destination, "receipt.json", receipt_raw)
    return verify(destination)


def verify(destination):
    destination = absolute_path(destination, required=True)
    inventory_raw = read_regular(destination, "inventory.json", MAX_METADATA)
    inventory = validate_inventory(load_json_bytes(inventory_raw))
    receipt = load_json_bytes(read_regular(destination, "receipt.json", MAX_METADATA))
    fields = {"schema_version", "kind", "mode", "captured_at", "collector_sha256", "inventory_sha256", "records",
              "source_executed", "business_rows_read", "approved_for_execution", "approved_for_publication",
              "original_source_project_recovered"}
    flags = fields - {"schema_version", "kind", "mode", "captured_at", "collector_sha256", "inventory_sha256", "records"}
    if (type(receipt) is not dict or set(receipt) != fields or type(receipt["schema_version"]) is not int
            or receipt["schema_version"] != 1 or receipt["kind"] != "portunex.frontend.private_preservation"
            or type(receipt["mode"]) is not str or receipt["mode"] not in {"inventory", "capture"}
            or any(receipt[field] is not False for field in flags)
            or receipt["inventory_sha256"] != digest(inventory_raw)
            or type(receipt["collector_sha256"]) is not str or not HEX.fullmatch(receipt["collector_sha256"])
            or type(receipt["captured_at"]) is not str or len(receipt["captured_at"]) > 64
            or type(receipt["records"]) is not list):
        raise EvidenceError("invalid frontend receipt")
    entries = inventory["entries"] if receipt["mode"] == "capture" else []
    if len(receipt["records"]) != len(entries):
        raise EvidenceError("receipt file set mismatch")
    expected_files, counts = {"inventory.json", "receipt.json"}, {"files": 0, "quarantine": 0, "reference": 0}
    for entry, record in zip(entries, receipt["records"]):
        if (type(record) is not dict or set(record) != {"path", "stored_path", "category", "reason"}
                or record["path"] != entry["path"] or type(record["category"]) is not str or record["category"] not in counts
                or record["stored_path"] != record["category"] + "/" + entry["path"]):
            raise EvidenceError("invalid frontend receipt record")
        raw = read_regular(destination, record["stored_path"], MAX_FILE)
        if (len(raw) != entry["bytes"] or digest(raw) != entry["sha256"]
                or classify(entry["path"], raw) != (record["category"], record["reason"])):
            raise EvidenceError("preserved content mismatch")
        counts[record["category"]] += 1
        expected_files.add(record["stored_path"])
    if private_inventory(destination) != expected_files:
        raise EvidenceError("private preservation file set mismatch")
    return {"status": "verified", "mode": receipt["mode"], "entry_count": len(inventory["entries"]),
            "total_bytes": sum(entry["bytes"] for entry in inventory["entries"]), "stored_counts": counts,
            "approved_for_execution": False, "approved_for_publication": False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="mode", required=True)
    for mode in ("inventory", "capture", "verify"):
        command = commands.add_parser(mode)
        command.add_argument("--destination", required=True)
        if mode != "verify":
            command.add_argument("--identity-file", required=True)
            command.add_argument("--interface", required=True)
        if mode == "capture":
            command.add_argument("--reviewed-inventory", required=True)
    args = parser.parse_args()
    try:
        if args.mode == "verify":
            report = verify(args.destination)
        else:
            reviewed = None
            if args.mode == "capture":
                path = absolute_path(args.reviewed_inventory, required=True)
                reviewed = validate_inventory(load_json_bytes(read_regular(path.parent, path.name, MAX_METADATA)))
            envelope = request_remote(args.mode, args.identity_file, args.interface, reviewed)
            report = preserve(args.destination, args.mode, envelope)
        print(json_bytes(report).decode(), end="")
        return 0
    except (EvidenceError, ProcessError, OSError, ValueError, TypeError):
        print("frontend preservation failed; no source content is printed", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
