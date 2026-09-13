"""Pin reviewed metadata, not a restorable schema or working authentication.

These tests never connect to PostgreSQL or execute the collector. The provenance
flags are reviewed collection claims; passing tests is not a database audit.
"""

from __future__ import annotations

import copy
import hashlib
import re
import unittest
from datetime import datetime
from pathlib import Path

from recoverykit.evidence import EvidenceError
from recoverykit.evidence.filesystem import load_json_bytes


ROOT = Path(__file__).resolve().parents[3]
COLLECTOR_PATH = "recovery/collectors/postgres/identity-inventory.sql"
INVENTORY_PATH = "recovery/baselines/portunex/postgres/identity-inventory.json"
PROVENANCE_PATH = "recovery/baselines/portunex/postgres/provenance.json"
TABLES = ["api_keys", "auth_sessions", "users"]
COLUMN_FIELDS = {
    "table", "position", "name", "type", "column_not_null", "has_default",
    "identity", "generated",
}
COUNT_FIELDS = {
    "tables", "columns", "foreign_keys", "indexes", "routines",
    "user_triggers", "policies",
}


class InventoryArtifactTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.collector_bytes = (ROOT / COLLECTOR_PATH).read_bytes()
        cls.inventory_bytes = (ROOT / INVENTORY_PATH).read_bytes()
        cls.inventory = load_json_bytes(cls.inventory_bytes)
        cls.provenance = load_json_bytes((ROOT / PROVENANCE_PATH).read_bytes())

    def assertInventory(self, document):
        self.assertIs(type(document), dict)
        self.assertEqual(set(document), {
            "schema_version", "kind", "scope", "columns", "database",
            "collected_at", "public_counts", "server_version",
            "transaction_read_only",
        })
        self.assertIs(type(document["schema_version"]), int)
        self.assertEqual(document["schema_version"], 1)
        self.assertEqual(document["kind"], "portunex.identity.catalog_inventory")
        self.assertEqual(document["database"], "portunex")
        self.assertEqual(document["transaction_read_only"], "on")
        self.assertIs(type(document["server_version"]), str)
        self.assertRegex(document["server_version"], r"^18\.6(?: |$)")
        self.assertIs(type(document["collected_at"]), str)
        # strptime accepts PostgreSQL's variable-width fractional seconds on
        # Python 3.9 too; fromisoformat on that runtime accepts only 3 or 6.
        collected = datetime.strptime(document["collected_at"], "%Y-%m-%dT%H:%M:%S.%f%z")
        self.assertIsNotNone(collected.utcoffset())

        scope = document["scope"]
        self.assertIs(type(scope), dict)
        self.assertEqual(set(scope), {
            "schema", "tables", "business_rows_read", "expressions_read",
            "reconstructable_schema",
        })
        self.assertEqual(scope["schema"], "public")
        self.assertEqual(scope["tables"], TABLES)
        for flag in ("business_rows_read", "expressions_read", "reconstructable_schema"):
            self.assertIs(scope[flag], False)

        counts = document["public_counts"]
        self.assertIs(type(counts), dict)
        self.assertEqual(set(counts), COUNT_FIELDS)
        for count in counts.values():
            self.assertIs(type(count), int)
            self.assertGreaterEqual(count, 0)

        columns = document["columns"]
        self.assertIs(type(columns), list)
        self.assertTrue(columns)
        for column in columns:
            self.assertIs(type(column), dict)
            self.assertEqual(set(column), COLUMN_FIELDS)
            self.assertIn(column["table"], TABLES)
            self.assertIs(type(column["position"]), int)
            self.assertGreater(column["position"], 0)
            for field in ("name", "type", "identity", "generated"):
                self.assertIs(type(column[field]), str)
            self.assertRegex(column["name"], r"^[a-z][a-z0-9_]*$")
            self.assertIn(column["type"], {
                "bigint", "text", "boolean", "jsonb", "timestamp with time zone",
                "public.citext", "numeric(30,18)",
            })
            self.assertIn(column["identity"], ("", "a", "d"))
            self.assertIn(column["generated"], ("", "s", "v"))
            self.assertIs(type(column["column_not_null"]), bool)
            self.assertIs(type(column["has_default"]), bool)
        self.assertEqual({column["table"] for column in columns}, set(TABLES))
        order = [(column["table"], column["position"]) for column in columns]
        self.assertEqual(order, sorted(order))
        self.assertEqual(len(order), len(set(order)))
        names = [(column["table"], column["name"]) for column in columns]
        self.assertEqual(len(names), len(set(names)))

    def assertProvenance(self, document, collector_bytes, inventory_bytes):
        self.assertIs(type(document), dict)
        self.assertEqual(set(document), {
            "schema_version", "kind", "source", "collector_path", "collector_sha256",
            "inventory_path", "inventory_sha256", "inventory_representation",
            "execution_exit_code", "business_rows_read", "schema_restore_verified",
        })
        self.assertIs(type(document["schema_version"]), int)
        self.assertEqual(document["schema_version"], 1)
        self.assertEqual(document["kind"], "portunex.identity.catalog_provenance")
        # Never follow a path supplied by the provenance document.
        self.assertEqual(document["collector_path"], COLLECTOR_PATH)
        self.assertEqual(document["inventory_path"], INVENTORY_PATH)
        for field in ("source", "inventory_representation"):
            self.assertIs(type(document[field]), str)
            self.assertGreater(len(document[field]), 0)
            self.assertLessEqual(len(document[field]), 4096)
        self.assertIn("not raw psql stdout", document["inventory_representation"])
        self.assertIs(type(document["execution_exit_code"]), int)
        self.assertEqual(document["execution_exit_code"], 0)
        self.assertIs(document["business_rows_read"], False)
        self.assertIs(document["schema_restore_verified"], False)
        for field, content in (("collector_sha256", collector_bytes),
                               ("inventory_sha256", inventory_bytes)):
            self.assertIs(type(document[field]), str)
            self.assertRegex(document[field], r"^[0-9a-f]{64}$")
            self.assertEqual(document[field], hashlib.sha256(content).hexdigest())

    def test_reviewed_inventory_has_strict_metadata_structure(self):
        self.assertInventory(self.inventory)

    def test_catalog_counts_and_scoped_column_counts_are_distinct(self):
        self.assertEqual(self.inventory["public_counts"], {
            "tables": 36, "columns": 387, "foreign_keys": 28, "indexes": 135,
            "routines": 48, "user_triggers": 0, "policies": 0,
        })
        self.assertEqual({table: sum(column["table"] == table
                                    for column in self.inventory["columns"])
                          for table in TABLES},
                         {"api_keys": 11, "auth_sessions": 9, "users": 10})

    def test_observed_types_and_id_flags_do_not_invent_schema_rules(self):
        columns = {(column["table"], column["name"]): column
                   for column in self.inventory["columns"]}
        self.assertEqual(columns["users", "email"]["type"], "public.citext")
        for field in ("points", "daily_recharge_limit"):
            self.assertEqual(columns["users", field]["type"], "numeric(30,18)")
        for table in TABLES:
            identifier = columns[table, "id"]
            self.assertEqual(identifier["type"], "bigint")
            self.assertIs(identifier["column_not_null"], True)
            self.assertIs(identifier["has_default"], False)
            self.assertEqual(identifier["identity"], "")
            self.assertEqual(identifier["generated"], "")
        # Field names and text types reveal neither values nor storage algorithms.
        for table, field in (("users", "password_phc"), ("auth_sessions", "token"),
                             ("api_keys", "key_text")):
            self.assertEqual(columns[table, field]["type"], "text")

    def test_inventory_rejects_scope_expansion_or_compatibility_promotion(self):
        variants = []
        for field in ("business_rows_read", "expressions_read", "reconstructable_schema"):
            for value in (True, 0, "false"):
                changed = copy.deepcopy(self.inventory)
                changed["scope"][field] = value
                variants.append(changed)
        for field in ("rows", "sample_token", "default_expression"):
            changed = copy.deepcopy(self.inventory)
            changed["columns"][0][field] = "synthetic-only"
            variants.append(changed)
        changed = copy.deepcopy(self.inventory)
        changed["scope"]["tables"].append("provider_credentials")
        variants.append(changed)
        variants.append(dict(self.inventory, business_verified=True))
        for number, variant in enumerate(variants):
            with self.subTest(variant=number), self.assertRaises(AssertionError):
                self.assertInventory(variant)

    def test_inventory_rejects_duplicate_reordered_and_invalid_columns(self):
        variants = []
        for field, value in (("position", True), ("position", 0), ("name", "id"),
                             ("column_not_null", 1), ("has_default", "false"),
                             ("identity", False), ("type", "double precision")):
            changed = copy.deepcopy(self.inventory)
            changed["columns"][1][field] = value
            variants.append(changed)
        changed = copy.deepcopy(self.inventory)
        changed["columns"][1]["position"] = changed["columns"][0]["position"]
        variants.append(changed)
        changed = copy.deepcopy(self.inventory)
        changed["columns"].reverse()
        variants.append(changed)
        changed = copy.deepcopy(self.inventory)
        changed["public_counts"]["tables"] = True
        variants.append(changed)
        for number, variant in enumerate(variants):
            with self.subTest(variant=number), self.assertRaises(AssertionError):
                self.assertInventory(variant)

    def test_shared_json_loader_rejects_duplicate_members_at_every_depth(self):
        for content in (
            b'{"schema_version":1,"schema_version":2}',
            b'{"scope":{"business_rows_read":true,"business_rows_read":false}}',
            b'{"columns":[{"name":"id","name":"token"}]}',
            b'{"collector_sha256":"first","collector_sha256":"second"}',
        ):
            with self.subTest(content=content), self.assertRaises(EvidenceError):
                load_json_bytes(content)

    def test_provenance_binds_collector_and_normalized_inventory_bytes(self):
        self.assertProvenance(self.provenance, self.collector_bytes, self.inventory_bytes)

    def test_provenance_rejects_tampering_and_unreviewed_paths(self):
        for collector, inventory in ((self.collector_bytes + b"\n", self.inventory_bytes),
                                     (self.collector_bytes, self.inventory_bytes + b"\n")):
            with self.subTest(tampering=True), self.assertRaises(AssertionError):
                self.assertProvenance(self.provenance, collector, inventory)
        for field, value in (("collector_path", "../outside.sql"),
                             ("inventory_path", "/outside.json"),
                             ("execution_exit_code", False),
                             ("business_rows_read", 0), ("schema_restore_verified", True)):
            changed = dict(self.provenance, **{field: value})
            with self.subTest(field=field), self.assertRaises(AssertionError):
                self.assertProvenance(changed, self.collector_bytes, self.inventory_bytes)

    def test_reviewed_collector_keeps_explicit_readonly_catalog_boundaries(self):
        # A regression guard for this reviewed SQL, not a general SQL safety parser.
        sql = "\n".join(line.split("--", 1)[0]
                        for line in self.collector_bytes.decode("utf-8").splitlines()).strip()
        self.assertTrue(sql.startswith("BEGIN READ ONLY;"))
        self.assertTrue(sql.endswith("ROLLBACK;"))
        for statement in ("SET LOCAL statement_timeout = '3s';",
                          "SET LOCAL lock_timeout = '1s';",
                          "SET LOCAL search_path = pg_catalog;"):
            self.assertIn(statement, sql)
        relations = set(re.findall(r"\b(?:FROM|JOIN)\s+([a-z_.]+)", sql, re.I))
        self.assertEqual(relations, {"pg_catalog." + name for name in (
            "pg_class", "pg_namespace", "pg_attribute", "pg_constraint",
            "pg_proc", "pg_trigger", "pg_policy",
        )})
        self.assertEqual(set(re.findall(r"current_setting\('([^']+)'\)", sql)),
                         {"transaction_read_only", "server_version"})


if __name__ == "__main__":
    unittest.main()
