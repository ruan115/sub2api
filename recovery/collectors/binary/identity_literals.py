#!/usr/bin/env python3
"""Read bounded static ASCII excerpts from the pinned, authorized Portunex ELF.

Run via `python3 -` over SSH; never executes the ELF or reads process memory.
Output MUST be screened and manually reviewed before printing or retaining it.
ASCII runs may concatenate unrelated Rust literals; they are not call traces.
"""

import hashlib
import json
import os
import re
import stat

SOURCE = "/opt/gateway/bin/portunex-server"
SIZE = 45810904
SHA256 = "5097f64d5042f1e43178df14decfb94ead48e96c16a4fae15bfc1fea14d7014a"
# Fixed, reviewed windows narrowed after the initial marker-guided inspection.
# No user-selectable paths; retain only authentication-relevant excerpts.
WINDOWS = (
    (50992, 8),
    (88487, 87),
    (242671, 89),
    (124002, 5),
    (297200, 345),
    (507602, 1981),
    (1058640, 65),
    (1058771, 31),
)
MARKERS = (
    "argon2", "bcrypt", "scrypt", "password-hash", "snowflake", "sonyflake",
    "ulid", "nanoid", "sess_", "ptx_", "auth_sessions", "password_phc",
    "SESSION_TTL", "SESSION_DURATION", "SESSION_SECRET", "INSERT INTO auth_sessions",
    "UPDATE auth_sessions", "FROM auth_sessions", "gen_random", "nextval",
)


def excerpts(data, windows):
    if len(windows) > 32 or sum(length for _, length in windows) > 65536:
        raise ValueError("window budget exceeded")
    result = []
    for start, length in windows:
        if start < 0 or not 1 <= length <= 8192 or start + length > len(data):
            raise ValueError("invalid window")
        segments = []
        for match in re.finditer(rb"[\x09\x0a\x0d\x20-\x7e]{4,}", data[start:start+length]):
            raw = match.group()
            segments.append({"start": start + match.start(), "end": start + match.end(),
                             "sha256": hashlib.sha256(raw).hexdigest(), "text": raw.decode("ascii")})
        result.append({"start": start, "end": start + length, "segments": segments})
    return result


def marker_scan(data):
    records = []
    for marker in MARKERS:
        needle = marker.encode("ascii")
        count, cursor, offsets = 0, 0, []
        while (hit := data.find(needle, cursor)) >= 0:
            count += 1
            if len(offsets) < 12:
                offsets.append(hit)
            cursor = hit + len(needle)
        records.append({"marker": marker, "count": count, "first_offsets": offsets})
    return records


def main():
    if os.path.realpath(SOURCE) != SOURCE:
        raise ValueError("unexpected source path")
    fd = os.open(SOURCE, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, "rb") as handle:
        before = os.fstat(handle.fileno())
        if not stat.S_ISREG(before.st_mode) or before.st_size != SIZE:
            raise ValueError("unexpected source file")
        data = handle.read(SIZE + 1)
        after = os.fstat(handle.fileno())
    if (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns, before.st_ctime_ns) != (
            after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns, after.st_ctime_ns):
        raise ValueError("source changed while reading")
    if len(data) != SIZE or data[:4] != b"\x7fELF" or hashlib.sha256(data).hexdigest() != SHA256:
        raise ValueError("source fingerprint mismatch")
    print(json.dumps({
        "schema_version": 1, "kind": "portunex.identity.static_literals",
        "source": {"path": SOURCE, "size": SIZE, "sha256": SHA256},
        "runtime_verified": False, "business_rows_read": False,
        "extraction": "clipped ASCII runs (tab/LF/CR/0x20-0x7e), minimum 4 bytes",
        "markers": marker_scan(data), "windows": excerpts(data, WINDOWS),
    }, ensure_ascii=True))


if __name__ == "__main__":
    main()
