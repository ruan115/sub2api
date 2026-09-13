"""Constants for version 1 of the offline recovery contract catalog."""

SCHEMA_VERSION = 1

# Owner names intentionally mirror the only two first-phase catalog roots.
OWNERS = frozenset(("portunex", "isthmus"))

# Later phases can add stronger evidence to a record.  First-phase manifests
# deliberately use only ``discovered`` so discovery is never confused with an
# implementation or a behavior verification.
STATUSES = frozenset(("discovered", "specified", "implemented", "verified"))

# A manifest may omit an empty collection, but any collection it includes must
# be a list of records using the common entry shape.
DISCOVERED_COLLECTIONS = frozenset(
    (
        "api_paths",
        "pages",
        "database_entities",
        "assets",
        "protocols",
    )
)

MANIFEST_FILENAME = "manifest.json"
