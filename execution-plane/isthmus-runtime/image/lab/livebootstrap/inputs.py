"""Only two hash-pinned public Linux/amd64 executables may enter this probe."""
import hashlib
import json
import os
import re

from lab.identity import _object
from recoverykit.evidence.filesystem import file_descriptor

NAMES = ("worker", "bootstrap-driver")


def verify(lab):
    with file_descriptor(lab.root, "live-binaries.json") as (fd, info):
        if info.st_nlink != 1 or not 0 < info.st_size <= 4096:
            raise ValueError("live_manifest_rejected")
        records = json.loads(os.read(fd, 4097), object_pairs_hook=_object)
    if not isinstance(records, list) or len(records) != len(NAMES):
        raise ValueError("live_manifest_rejected")
    for name, record in zip(NAMES, records):
        if (not isinstance(record, dict) or set(record) != {"name", "size", "sha256"}
                or record["name"] != name or type(record["size"]) is not int
                or not 0 < record["size"] <= 64 * 1024**2
                or not isinstance(record["sha256"], str)
                or not re.fullmatch(r"[a-f0-9]{64}", record["sha256"])):
            raise ValueError("live_binary_record_rejected")
        with file_descriptor(lab.root, "live-bin/" + name) as (fd, info):
            if (info.st_nlink != 1 or info.st_uid != os.geteuid() or info.st_mode & 0o7777 != 0o555
                    or info.st_size != record["size"]):
                raise ValueError("live_binary_metadata_rejected")
            header = os.pread(fd, 20, 0)
            if header[:6] != b"\x7fELF\x02\x01" or header[18:20] != b"\x3e\x00":
                raise ValueError("live_binary_architecture_rejected")
            digest, total = hashlib.sha256(), 0
            while chunk := os.read(fd, min(1024**2, record["size"] + 1 - total)):
                digest.update(chunk)
                total += len(chunk)
                if total > record["size"]:
                    raise ValueError("live_binary_size_changed")
            after = os.fstat(fd)
            if (total != record["size"] or digest.hexdigest() != record["sha256"]
                    or after.st_size != info.st_size or after.st_mtime_ns != info.st_mtime_ns):
                raise ValueError("live_binary_hash_mismatch")
    return records
