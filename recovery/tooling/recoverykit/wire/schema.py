"""Strict, bounded JSON schema for reviewed static wire observations."""

from __future__ import annotations

import re
from typing import Any, Dict, List, Mapping, Sequence
from urllib.parse import urlsplit

from ..evidence.errors import EvidenceError
from ..evidence.filesystem import absolute_path, json_bytes, load_json_bytes, read_regular, relative_path
from ..evidence.policy import MAX_METADATA_BYTES, check_content
from .errors import WireError


SCHEMA_VERSION = 1
MAX_OBSERVATIONS = 256
MAX_ANCHORS_PER_OBSERVATION = 8
MAX_ANCHOR_SPAN_BYTES = 8192
MAX_PATH_BYTES = 2048
MAX_STATEMENT_BYTES = 8192
MAX_LITERAL_BYTES = MAX_ANCHOR_SPAN_BYTES

ASPECTS = frozenset(
    (
        "method",
        "path",
        "request_header",
        "request_body",
        "credentials",
        "response_field",
        "status",
        "pagination",
        "error",
    )
)

_STABLE_ID = re.compile(r"^[a-z][a-z0-9_.-]{0,127}$")
_MANIFEST_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
_SHA256 = re.compile(r"^[0-9a-f]{64}$")

_DOCUMENT_KEYS = frozenset(("schema_version", "id", "artifact_manifest_id", "observations"))
_OBSERVATION_KEYS = frozenset(("id", "record_id", "path", "aspect", "statement", "anchors"))
_ANCHOR_KEYS = frozenset(("artifact", "start", "end", "sha256", "literal"))


def load_observation_document(document_path: object) -> Dict[str, Any]:
    """Load a no-follow, UTF-8, secret-screened reviewed observation document."""

    try:
        path = absolute_path(document_path)
        raw = read_regular(path.parent, path.name, MAX_METADATA_BYTES)
    except EvidenceError as error:
        raise WireError("invalid_document") from error
    try:
        check_content(raw, text_only=True)
    except EvidenceError as error:
        raise WireError("unsafe_input") from error
    try:
        document = load_json_bytes(raw)
    except EvidenceError as error:
        raise WireError("invalid_document") from error
    return validate_observation_document(document)


def validate_observation_document(document: object) -> Dict[str, Any]:
    """Validate a parsed document without inspecting its source artifacts."""

    if not isinstance(document, Mapping):
        _fail_document()
    _require_exact_keys(document, _DOCUMENT_KEYS)
    if type(document["schema_version"]) is not int or document["schema_version"] != SCHEMA_VERSION:
        _fail_document()
    identifier = _stable_id(document["id"])
    artifact_manifest_id = _manifest_id(document["artifact_manifest_id"])
    observations = document["observations"]
    if not isinstance(observations, list):
        _fail_document()
    if not 1 <= len(observations) <= MAX_OBSERVATIONS:
        raise WireError("input_limit")

    normalized: List[Dict[str, Any]] = []
    observation_ids = set()
    for observation in observations:
        checked = _validate_observation(observation)
        if checked["id"] in observation_ids:
            _fail_document()
        observation_ids.add(checked["id"])
        normalized.append(checked)
    normalized_document = {
        "schema_version": SCHEMA_VERSION,
        "id": identifier,
        "artifact_manifest_id": artifact_manifest_id,
        "observations": normalized,
    }
    # A compact UTF-8 input can expand materially when it is represented as
    # canonical JSON (for example, non-ASCII statements).  Bound that normal
    # form too, rather than treating a raw-byte read cap as a memory guarantee.
    if len(json_bytes(normalized_document)) > MAX_METADATA_BYTES:
        raise WireError("input_limit")
    return normalized_document


def _validate_observation(observation: object) -> Dict[str, Any]:
    if not isinstance(observation, Mapping):
        _fail_document()
    _require_exact_keys(observation, _OBSERVATION_KEYS)
    identifier = _stable_id(observation["id"])
    record_id = _stable_id(observation["record_id"])
    path = _path(observation["path"])
    aspect = observation["aspect"]
    if not isinstance(aspect, str) or aspect not in ASPECTS:
        _fail_document()
    _screen_text(aspect, 64)
    statement = _screen_text(observation["statement"], MAX_STATEMENT_BYTES)
    anchors = observation["anchors"]
    if not isinstance(anchors, list):
        _fail_document()
    if not 1 <= len(anchors) <= MAX_ANCHORS_PER_OBSERVATION:
        raise WireError("input_limit")
    normalized_anchors: List[Dict[str, Any]] = []
    anchor_keys = set()
    for anchor in anchors:
        checked = _validate_anchor(anchor)
        # Duplicate source anchors in one observation add no evidence and often
        # conceal hand-edited repeated assertions.  The same span may support a
        # different observation/aspect, which remains an explicit separate fact.
        key = (checked["artifact"], checked["start"], checked["end"], checked["sha256"], checked["literal"])
        if key in anchor_keys:
            _fail_document()
        anchor_keys.add(key)
        normalized_anchors.append(checked)
    return {
        "id": identifier,
        "record_id": record_id,
        "path": path,
        "aspect": aspect,
        "statement": statement,
        "anchors": normalized_anchors,
    }


def _validate_anchor(anchor: object) -> Dict[str, Any]:
    if not isinstance(anchor, Mapping):
        _fail_document()
    _require_exact_keys(anchor, _ANCHOR_KEYS)
    artifact = _artifact_path(anchor["artifact"])
    start = anchor["start"]
    end = anchor["end"]
    if type(start) is not int or type(end) is not int or start < 0 or end <= start:
        _fail_document()
    if end - start > MAX_ANCHOR_SPAN_BYTES:
        raise WireError("input_limit")
    sha256 = anchor["sha256"]
    if not isinstance(sha256, str) or not _SHA256.fullmatch(sha256):
        _fail_document()
    literal = _screen_text(anchor["literal"], MAX_LITERAL_BYTES)
    return {"artifact": artifact, "start": start, "end": end, "sha256": sha256, "literal": literal}


def _require_exact_keys(value: Mapping[str, Any], expected: Sequence[str]) -> None:
    if set(value) != set(expected):
        _fail_document()


def _stable_id(value: object) -> str:
    value = _screen_text(value, 128)
    if not _STABLE_ID.fullmatch(value):
        _fail_document()
    return value


def _manifest_id(value: object) -> str:
    value = _screen_text(value, 128)
    if not _MANIFEST_ID.fullmatch(value):
        _fail_document()
    return value


def _path(value: object) -> str:
    value = _screen_text(value, MAX_PATH_BYTES)
    # ``urlsplit`` rejects malformed bracketed authorities (for example
    # ``//[``) with ``ValueError``.  Keep parser implementation details out
    # of the public boundary just as we do for every other malformed document
    # value.
    try:
        split = urlsplit(value)
    except ValueError:
        _fail_document()
    if (
        not value.startswith("/")
        or value.startswith("//")
        or split.scheme
        or split.netloc
        or split.query
        or split.fragment
    ):
        _fail_document()
    return value


def _artifact_path(value: object) -> str:
    value = _screen_text(value, 1024)
    try:
        return relative_path(value)
    except EvidenceError as error:
        raise WireError("invalid_document") from error


def _screen_text(value: object, maximum_bytes: int) -> str:
    if not isinstance(value, str) or not value:
        _fail_document()
    try:
        encoded = value.encode("utf-8")
    except UnicodeError:
        _fail_document()
    if len(encoded) > maximum_bytes:
        raise WireError("input_limit")
    if any(ord(character) < 32 or ord(character) == 127 for character in value):
        _fail_document()
    try:
        check_content(encoded, text_only=True)
    except EvidenceError as error:
        raise WireError("unsafe_input") from error
    return value


def _fail_document() -> None:
    raise WireError("invalid_document")
