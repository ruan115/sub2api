"""Offline guards for reviewed catalog definitions, not restored authentication.

The SQL guard is deliberately specific to this reviewed collector. It is not a
SQL sandbox or a proof about arbitrary future queries; no test connects to a DB.
"""

from __future__ import annotations

import copy
import hashlib
import json
import re
import unittest
from datetime import datetime
from pathlib import Path

from recoverykit.evidence import EvidenceError
from recoverykit.evidence.filesystem import load_json_bytes
from recoverykit.evidence.policy import check_content


ROOT = Path(__file__).resolve().parents[3]
COLLECTOR_PATH = "recovery/collectors/postgres/identity-definition.sql"
DEFINITION_PATH = "recovery/baselines/portunex/postgres/identity-definition.json"
PROVENANCE_PATH = "recovery/baselines/portunex/postgres/definition-provenance.json"
TABLES = ["api_keys", "auth_sessions", "users"]
COLUMN_FIELDS = {
    "table", "position", "name", "type", "column_not_null", "has_default",
    "identity", "generated", "default_expression", "collation_schema",
    "collation_name",
}
CONSTRAINT_FIELDS = {
    "table", "name", "type", "validated", "deferrable", "initially_deferred",
    "no_inherit", "column_positions", "referenced_schema", "referenced_table",
    "referenced_column_positions", "definition",
}
INDEX_FIELDS = {
    "table", "name", "method", "unique", "primary", "valid", "ready",
    "nulls_not_distinct", "key_attribute_count", "total_attribute_count",
    "column_positions", "definition", "predicate", "expressions",
}


class DefinitionArtifactTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.collector_bytes = (ROOT / COLLECTOR_PATH).read_bytes()
        cls.definition_bytes = (ROOT / DEFINITION_PATH).read_bytes()
        cls.definition = load_json_bytes(cls.definition_bytes)
        cls.provenance_bytes = (ROOT / PROVENANCE_PATH).read_bytes()
        cls.provenance = load_json_bytes(cls.provenance_bytes)

    def assertText(self, value, *, nullable=False):
        if nullable and value is None:
            return
        self.assertIs(type(value), str)
        self.assertLessEqual(len(value), 8192)
        self.assertNotRegex(value, r"[\x00-\x1f\x7f]")

    def assertDefinition(self, document):
        self.assertIs(type(document), dict)
        self.assertEqual(set(document), {
            "schema_version", "kind", "collected_at", "transaction_read_only",
            "database", "server_version", "scope", "columns", "constraints",
            "indexes", "extensions",
        })
        self.assertIs(type(document["schema_version"]), int)
        self.assertEqual(document["schema_version"], 1)
        self.assertEqual(document["kind"], "portunex.identity.catalog_definition")
        self.assertEqual(document["transaction_read_only"], "on")
        self.assertEqual(document["database"], "portunex")
        self.assertText(document["server_version"])
        self.assertRegex(document["server_version"], r"^18\.6(?: |$)")
        self.assertText(document["collected_at"])
        self.assertIsNotNone(datetime.strptime(
            document["collected_at"], "%Y-%m-%dT%H:%M:%S.%f%z").utcoffset())
        scope = document["scope"]
        self.assertIs(type(scope), dict)
        self.assertEqual(set(scope), {
            "schema", "tables", "extensions", "business_rows_read",
            "expressions_read", "function_bodies_read",
            "sequence_current_values_read", "reconstructable_schema",
        })
        self.assertEqual(scope["schema"], "public")
        self.assertEqual(scope["tables"], TABLES)
        self.assertEqual(scope["extensions"], ["citext"])
        self.assertIs(scope["expressions_read"], True)
        for field in ("business_rows_read", "function_bodies_read",
                      "sequence_current_values_read", "reconstructable_schema"):
            self.assertIs(scope[field], False)

        for group, fields in (("columns", COLUMN_FIELDS),
                              ("constraints", CONSTRAINT_FIELDS),
                              ("indexes", INDEX_FIELDS)):
            entries = document[group]
            self.assertIs(type(entries), list)
            self.assertTrue(entries)
            keys = []
            for entry in entries:
                self.assertIs(type(entry), dict)
                self.assertEqual(set(entry), fields)
                self.assertIn(entry["table"], TABLES)
                self.assertRegex(entry["name"], r"^[a-z][a-z0-9_]*$")
                keys.append((entry["table"], entry.get("position", entry["name"])))
            self.assertEqual(keys, sorted(keys))
            self.assertEqual(len(keys), len(set(keys)))
            self.assertEqual({entry["table"] for entry in entries}, set(TABLES))

        positions = {table: set() for table in TABLES}
        column_names = set()
        for column in document["columns"]:
            self.assertIs(type(column["position"]), int)
            self.assertGreater(column["position"], 0)
            positions[column["table"]].add(column["position"])
            key = (column["table"], column["name"])
            self.assertNotIn(key, column_names)
            column_names.add(key)
            for flag in ("column_not_null", "has_default"):
                self.assertIs(type(column[flag]), bool)
            self.assertEqual(column["has_default"], column["default_expression"] is not None)
            self.assertIn(column["identity"], ("", "a", "d"))
            self.assertIn(column["generated"], ("", "s", "v"))
            self.assertText(column["type"])
            for field in ("default_expression", "collation_schema", "collation_name"):
                self.assertText(column[field], nullable=True)
            self.assertEqual(column["collation_schema"] is None,
                             column["collation_name"] is None)

        for constraint in document["constraints"]:
            self.assertIn(constraint["type"], ("c", "f", "n", "p", "u", "x"))
            self.assertText(constraint["definition"])
            for flag in ("validated", "deferrable", "initially_deferred", "no_inherit"):
                self.assertIs(type(constraint[flag]), bool)
            self.assertIs(type(constraint["column_positions"]), list)
            for position in constraint["column_positions"]:
                self.assertIs(type(position), int)
                self.assertIn(position, positions[constraint["table"]])
            if constraint["type"] == "f":
                self.assertEqual(constraint["referenced_schema"], "public")
                self.assertIn(constraint["referenced_table"], TABLES)
                referenced = constraint["referenced_column_positions"]
                self.assertIs(type(referenced), list)
                self.assertEqual(len(referenced), len(constraint["column_positions"]))
                for position in referenced:
                    self.assertIs(type(position), int)
                    self.assertIn(position, positions[constraint["referenced_table"]])
            else:
                for field in ("referenced_schema", "referenced_table",
                              "referenced_column_positions"):
                    self.assertIsNone(constraint[field])

        for index in document["indexes"]:
            self.assertIn(index["method"], ("btree", "gin"))
            for flag in ("unique", "primary", "valid", "ready", "nulls_not_distinct"):
                self.assertIs(type(index[flag]), bool)
            for field in ("key_attribute_count", "total_attribute_count"):
                self.assertIs(type(index[field]), int)
                self.assertGreater(index[field], 0)
            self.assertLessEqual(index["key_attribute_count"], index["total_attribute_count"])
            self.assertIs(type(index["column_positions"]), list)
            self.assertEqual(len(index["column_positions"]), index["total_attribute_count"])
            for position in index["column_positions"]:
                self.assertIs(type(position), int)
                self.assertIn(position, positions[index["table"]] | {0})
            self.assertText(index["definition"])
            for field in ("predicate", "expressions"):
                self.assertText(index[field], nullable=True)
            self.assertIn(" ON public." + index["table"] + " USING ", index["definition"])

        self.assertIs(type(document["extensions"]), list)
        self.assertEqual(document["extensions"], [{
            "name": "citext", "schema": "public", "version": "1.8", "relocatable": True,
        }])
        self.assertIs(document["extensions"][0]["relocatable"], True)

    def assertProvenance(self, document, collector_bytes, definition_bytes):
        self.assertIs(type(document), dict)
        self.assertEqual(set(document), {
            "schema_version", "kind", "source", "collector_path", "collector_sha256",
            "definition_path", "definition_sha256", "definition_representation",
            "raw_stdout_sha256", "raw_stdout_bytes", "raw_stdout_retained",
            "execution_exit_code", "business_rows_read", "function_bodies_read",
            "sequence_current_values_read", "schema_restore_verified",
            "authentication_compatibility_verified", "content_screening", "expression_review",
        })
        self.assertIs(type(document["schema_version"]), int)
        self.assertEqual(document["schema_version"], 1)
        self.assertEqual(document["kind"], "portunex.identity.definition_provenance")
        # Never follow paths from a provenance document.
        self.assertEqual(document["collector_path"], COLLECTOR_PATH)
        self.assertEqual(document["definition_path"], DEFINITION_PATH)
        for field in ("source", "definition_representation", "content_screening", "expression_review"):
            self.assertText(document[field])
            self.assertTrue(document[field])
        self.assertIn("not raw psql stdout", document["definition_representation"])
        self.assertIs(type(document["execution_exit_code"]), int)
        self.assertEqual(document["execution_exit_code"], 0)
        for field in ("raw_stdout_retained", "business_rows_read", "function_bodies_read",
                      "sequence_current_values_read", "schema_restore_verified",
                      "authentication_compatibility_verified"):
            self.assertIs(document[field], False)
        for field, content in (("collector_sha256", collector_bytes),
                               ("definition_sha256", definition_bytes)):
            self.assertRegex(document[field], r"^[0-9a-f]{64}$")
            self.assertEqual(document[field], hashlib.sha256(content).hexdigest())
        # Raw stdout was screened in memory, not retained: this receipt cannot
        # independently recompute it and does not pretend normalized bytes can.
        self.assertRegex(document["raw_stdout_sha256"], r"^[0-9a-f]{64}$")
        self.assertIs(type(document["raw_stdout_bytes"]), int)
        self.assertGreater(document["raw_stdout_bytes"], 0)
        self.assertLessEqual(document["raw_stdout_bytes"], 256 * 1024)

    def test_reviewed_definition_has_scoped_structure(self):
        self.assertDefinition(self.definition)
        self.assertEqual({key: len(self.definition[key]) for key in (
            "columns", "constraints", "indexes", "extensions")}, {
            "columns": 30, "constraints": 14, "indexes": 17, "extensions": 1,
        })

    def test_prior_inventory_is_preserved_and_column_observations_agree(self):
        prior = load_json_bytes((ROOT / "recovery/baselines/portunex/postgres/identity-inventory.json").read_bytes())
        projected = [{key: column[key] for key in prior["columns"][0]}
                     for column in self.definition["columns"]]
        self.assertEqual(projected, prior["columns"])
        self.assertNotEqual(prior["collected_at"], self.definition["collected_at"])
        self.assertIs(prior["scope"]["expressions_read"], False)

    def test_defaults_do_not_invent_id_generation_or_expiration_rules(self):
        columns = {(column["table"], column["name"]): column
                   for column in self.definition["columns"]}
        for table in TABLES:
            self.assertIsNone(columns[table, "id"]["default_expression"])
            self.assertEqual(columns[table, "id"]["identity"], "")
        self.assertIsNone(columns["auth_sessions", "expires_at"]["default_expression"])
        self.assertEqual(columns["users", "role"]["default_expression"], "'user'::text")
        self.assertEqual(columns["users", "email"]["type"], "public.citext")
        self.assertEqual({column["default_expression"] for column in columns.values()},
                         {None, "true", "false", "now()", "0", "'user'::text"})

    def test_reviewed_soft_delete_uniqueness_and_foreign_keys_are_exact(self):
        indexes = {index["name"]: index for index in self.definition["indexes"]}
        for name in ("idx_api_keys_key_text_unique", "idx_auth_sessions_token_unique",
                     "idx_users_email_unique"):
            self.assertIs(indexes[name]["unique"], True)
            self.assertIs(indexes[name]["nulls_not_distinct"], False)
            self.assertEqual(indexes[name]["predicate"], "(deleted_at IS NULL)")
        foreign_keys = [c for c in self.definition["constraints"] if c["type"] == "f"]
        self.assertEqual({c["table"] for c in foreign_keys}, {"api_keys", "auth_sessions"})
        for constraint in foreign_keys:
            self.assertEqual(constraint["definition"],
                             "FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE")

    def test_definition_rejects_scope_expansion_or_false_compatibility(self):
        variants = []
        for field in ("business_rows_read", "function_bodies_read",
                      "sequence_current_values_read", "reconstructable_schema"):
            for value in (True, 0, "false"):
                changed = copy.deepcopy(self.definition)
                changed["scope"][field] = value
                variants.append(changed)
        for group, field in (("columns", "sample_password_phc"),
                             ("constraints", "business_rows"), ("indexes", "token_value")):
            changed = copy.deepcopy(self.definition)
            changed[group][0][field] = "synthetic-only"
            variants.append(changed)
        changed = copy.deepcopy(self.definition)
        changed["scope"]["tables"].append("provider_credentials")
        variants.append(changed)
        for number, variant in enumerate(variants):
            with self.subTest(variant=number), self.assertRaises(AssertionError):
                self.assertDefinition(variant)

    def test_definition_rejects_invalid_order_and_metadata_types(self):
        variants = []
        for group, field, value in (("columns", "position", True),
                                    ("columns", "has_default", 0),
                                    ("columns", "collation_name", "default"),
                                    ("constraints", "validated", 1),
                                    ("indexes", "unique", "true"),
                                    ("indexes", "total_attribute_count", True)):
            changed = copy.deepcopy(self.definition)
            changed[group][0][field] = value
            variants.append(changed)
        for group in ("columns", "constraints", "indexes"):
            changed = copy.deepcopy(self.definition)
            changed[group].reverse()
            variants.append(changed)
            changed = copy.deepcopy(self.definition)
            changed[group].append(copy.deepcopy(changed[group][0]))
            variants.append(changed)
        for number, variant in enumerate(variants):
            with self.subTest(variant=number), self.assertRaises(AssertionError):
                self.assertDefinition(variant)

    def test_provenance_binds_new_artifact_without_rewriting_prior_snapshot(self):
        self.assertProvenance(self.provenance, self.collector_bytes, self.definition_bytes)

    def test_provenance_rejects_tampering_or_claim_promotion(self):
        for collector, definition in ((self.collector_bytes + b"\n", self.definition_bytes),
                                      (self.collector_bytes, self.definition_bytes + b"\n")):
            with self.subTest(tampering=True), self.assertRaises(AssertionError):
                self.assertProvenance(self.provenance, collector, definition)
        for field, value in (("collector_path", "../outside.sql"),
                             ("definition_path", "/outside.json"),
                             ("raw_stdout_retained", True), ("raw_stdout_bytes", False),
                             ("execution_exit_code", False), ("business_rows_read", 0),
                             ("schema_restore_verified", True),
                             ("authentication_compatibility_verified", True)):
            with self.subTest(field=field), self.assertRaises(AssertionError):
                self.assertProvenance(dict(self.provenance, **{field: value}),
                                      self.collector_bytes, self.definition_bytes)

    def test_collector_only_deparses_allowlisted_catalog_definitions(self):
        sql = "\n".join(line.split("--", 1)[0]
                        for line in self.collector_bytes.decode("utf-8").splitlines()).strip()
        statements = [statement.strip() for statement in sql.split(";") if statement.strip()]
        self.assertEqual(statements[:4], [
            "BEGIN READ ONLY", "SET LOCAL statement_timeout = '3s'",
            "SET LOCAL lock_timeout = '1s'", "SET LOCAL search_path = pg_catalog",
        ])
        self.assertEqual(len(statements), 6)
        self.assertTrue(statements[4].startswith("SELECT jsonb_build_object("))
        self.assertEqual(statements[-1], "ROLLBACK")
        self.assertEqual(sql.count("WHERE n.nspname = 'public' AND c.relname IN ('api_keys', 'auth_sessions', 'users')"), 3)
        self.assertEqual(sql.count("AND c.relkind IN ('r', 'p')"), 3)
        self.assertEqual(sql.count("WHERE e.extname = 'citext'"), 1)
        relations = set(re.findall(r"\b(?:FROM|JOIN)\s+([a-z_.]+)", sql, re.I))
        self.assertEqual(relations, {"pg_catalog." + name for name in (
            "pg_attribute", "pg_class", "pg_namespace", "pg_attrdef", "pg_collation",
            "pg_constraint", "pg_index", "pg_am", "pg_extension",
        )})
        self.assertEqual(set(re.findall(r"current_setting\('([^']+)'\)", sql)),
                         {"transaction_read_only", "server_version"})
        self.assertEqual(set(re.findall(r"pg_catalog\.(pg_get_[a-z]+)\(", sql)),
                         {"pg_get_expr", "pg_get_constraintdef", "pg_get_indexdef"})
        self.assertNotRegex(sql, r"(?i)\b(pg_authid|pg_proc|pg_get_functiondef|nextval|currval|setval|pg_read_file|copy|insert|update|delete|execute|commit)\b")

    def test_reviewed_artifacts_pass_shared_secret_screening(self):
        for content in (self.collector_bytes, self.definition_bytes, self.provenance_bytes):
            check_content(content, text_only=True)
        synthetic = json.dumps({"default_expression": "sk-" + "a" * 40}).encode("utf-8")
        with self.assertRaises(EvidenceError):
            check_content(synthetic, text_only=True)


if __name__ == "__main__":
    unittest.main()
