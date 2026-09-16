"""Synthetic metadata only: these tests never contact a Docker daemon."""

from copy import deepcopy
from pathlib import Path
import unittest

from recoverykit.lab.config import LabConfig, LabError
from recoverykit.lab.preflight import inspect_engine


PATHS = (
    "/version", "/v1.43/info", "/v1.43/containers/json?all=1",
    "/v1.43/networks", "/v1.43/volumes",
)


def metadata_fixture():
    networks = []
    for index, (name, driver) in enumerate((("bridge", "bridge"), ("host", "host"), ("none", "null")), 1):
        networks.append({
            "Name": name, "Id": str(index) * 64, "Driver": driver, "Scope": "local",
            "Internal": False, "Attachable": False, "Ingress": False, "EnableIPv6": False,
            "Labels": {}, "Options": {},
        })
    networks[0]["Options"] = {
        "com.docker.network.bridge.default_bridge": "true",
        "com.docker.network.bridge.enable_icc": "true",
        "com.docker.network.bridge.enable_ip_masquerade": "true",
        "com.docker.network.bridge.host_binding_ipv4": "0.0.0.0",
        "com.docker.network.bridge.name": "docker0",
        "com.docker.network.driver.mtu": "1500",
    }
    return {
        PATHS[0]: {"ApiVersion": "1.47", "MinAPIVersion": "1.24"},
        PATHS[1]: {
            "ID": "synthetic-engine-id", "Name": "ccmax-lab-test", "OSType": "linux",
            "Architecture": "aarch64", "DefaultRuntime": "runc",
            "Runtimes": {"runc": {"path": "runc"}},
            "SecurityOptions": ["name=apparmor", "name=seccomp,profile=builtin", "name=cgroupns"],
            "Swarm": {"LocalNodeState": "inactive", "ControlAvailable": False},
            "HttpProxy": "", "HttpsProxy": "", "NoProxy": "",
            "Containers": 0, "ContainersRunning": 0, "ContainersPaused": 0,
            "ContainersStopped": 0, "NCPU": 2, "MemTotal": 2 * 1024 ** 3,
        },
        PATHS[2]: [], PATHS[3]: networks, PATHS[4]: {"Volumes": None, "Warnings": None},
    }


class FakeEngine:
    def __init__(self, documents=None):
        self.documents = metadata_fixture() if documents is None else documents
        self.calls = []

    def get_json(self, path):
        self.calls.append(path)
        if path not in PATHS:
            raise AssertionError("unexpected path")
        return deepcopy(self.documents[path])


class LabPreflightTests(unittest.TestCase):
    def setUp(self):
        self.config = LabConfig(Path("/synthetic-private-lab/engine.sock"), "synthetic-engine-id", "ccmax-lab-test", "arm64")

    def assert_denied(self, documents, last_path, code=None):
        engine = FakeEngine(documents)
        with self.assertRaises(LabError) as caught:
            inspect_engine(self.config, engine)
        self.assertEqual(engine.calls, list(PATHS[:PATHS.index(last_path) + 1]))
        self.assertRegex(str(caught.exception), r"^[a-z][a-z_]+$")
        self.assertNotIn("synthetic-private-value", str(caught.exception))
        if code is not None:
            self.assertEqual(str(caught.exception), code)

    def test_only_fixed_gets_and_result_never_authorizes_execution(self):
        engine = FakeEngine()
        self.assertEqual(inspect_engine(self.config, engine), {
            "status": "docker_endpoint_checked", "isolation_verified": False,
            "execution_permitted": False, "created_resources": 0,
            "container_count": 0, "volume_count": 0, "network_count": 3,
        })
        self.assertEqual(engine.calls, list(PATHS))

    def test_architecture_aliases_are_normalized_but_bound(self):
        for requested, reported in (("arm64", "arm64"), ("arm64", "aarch64"), ("amd64", "amd64"), ("amd64", "x86_64")):
            with self.subTest(requested=requested, reported=reported):
                documents = metadata_fixture()
                documents[PATHS[1]]["Architecture"] = reported
                config = LabConfig(self.config.socket_path, self.config.engine_id, self.config.engine_name, requested)
                self.assertFalse(inspect_engine(config, FakeEngine(documents))["execution_permitted"])

    def test_version_negotiation_is_checked_before_other_requests(self):
        for maximum, minimum in (("1.43", "1.43"), ("1.44", "1.42"), ("1.99", "1.1")):
            with self.subTest(maximum=maximum, minimum=minimum):
                documents = metadata_fixture()
                documents[PATHS[0]] = {"ApiVersion": maximum, "MinAPIVersion": minimum}
                inspect_engine(self.config, FakeEngine(documents))
        for maximum, minimum in (("1.42", "1.24"), ("1.50", "1.44"), ("1.43", "1.44")):
            documents = metadata_fixture()
            documents[PATHS[0]] = {"ApiVersion": maximum, "MinAPIVersion": minimum}
            self.assert_denied(documents, PATHS[0], "engine_api_unsupported")

    def test_version_missing_or_ambiguous_types_fail_closed(self):
        for field in ("ApiVersion", "MinAPIVersion"):
            for value in (None, True, 1.43, 143, "1.043", "1.43\n", "1.43.0", "v1.43", "1." + "9" * 10000):
                with self.subTest(field=field, kind=type(value).__name__):
                    documents = metadata_fixture()
                    documents[PATHS[0]][field] = value
                    self.assert_denied(documents, PATHS[0])
            documents = metadata_fixture()
            del documents[PATHS[0]][field]
            self.assert_denied(documents, PATHS[0])

    def test_wrong_top_level_types_are_denied_at_that_request(self):
        for path in PATHS:
            for invalid in (None, True, 0, "synthetic-private-value", [] if path not in PATHS[2:4] else {}):
                with self.subTest(path=path, kind=type(invalid).__name__):
                    documents = metadata_fixture()
                    documents[path] = invalid
                    self.assert_denied(documents, path)

    def test_engine_identity_is_exact_and_early(self):
        for field, value in (("ID", "other-engine"), ("Name", "other-lab"), ("Architecture", "amd64"), ("OSType", "windows")):
            documents = metadata_fixture()
            documents[PATHS[1]][field] = value
            self.assert_denied(documents, PATHS[1], "engine_identity_mismatch")

    def test_required_info_fields_cannot_be_omitted(self):
        for field in metadata_fixture()[PATHS[1]]:
            with self.subTest(field=field):
                documents = metadata_fixture()
                del documents[PATHS[1]][field]
                self.assert_denied(documents, PATHS[1])

    def test_info_numbers_reject_bool_fraction_negative_and_nonfinite(self):
        for field in ("Containers", "ContainersRunning", "ContainersPaused", "ContainersStopped", "NCPU", "MemTotal"):
            for value in (False, True, -1, 0.0, float("inf"), "0", None):
                with self.subTest(field=field, kind=type(value).__name__):
                    documents = metadata_fixture()
                    documents[PATHS[1]][field] = value
                    self.assert_denied(documents, PATHS[1])
        for field in ("NCPU", "MemTotal"):
            documents = metadata_fixture()
            documents[PATHS[1]][field] = 0
            self.assert_denied(documents, PATHS[1])

    def test_resources_reported_by_info_are_not_ignored(self):
        for field in ("Containers", "ContainersRunning", "ContainersPaused", "ContainersStopped"):
            documents = metadata_fixture()
            documents[PATHS[1]][field] = 1
            self.assert_denied(documents, PATHS[1], "engine_not_empty")

    def test_security_options_are_present_known_unique_and_enforcing(self):
        for options in (None, {}, [], ["name=apparmor"], ["name=seccomp,profile=builtin"],
                        ["name=apparmor", "name=seccomp,profile=unconfined"],
                        ["name=apparmor", "name=seccomp,profile=custom"],
                        ["name=apparmor", "name=seccomp,profile=builtin", "name=rootless"],
                        ["name=apparmor", "name=seccomp,profile=builtin", "name=unknown"],
                        ["name=apparmor", "name=seccomp,profile=builtin", "name=apparmor"],
                        ["name=apparmor", "name=seccomp,profile=builtin", {}]):
            documents = metadata_fixture()
            documents[PATHS[1]]["SecurityOptions"] = options
            self.assert_denied(documents, PATHS[1])

    def test_runtime_must_be_known_runc(self):
        for field, value in (("DefaultRuntime", "runsc"), ("Runtimes", {}), ("Runtimes", []),
                             ("Runtimes", {"runc": None}), ("Runtimes", {"runc": {"path": "runsc"}}),
                             ("Runtimes", {"runc": {"path": "runc", "runtimeArgs": ["--root=/other"]}}),
                             ("Runtimes", {"runc": {"path": "runc"}, "unknown": {}})):
            documents = metadata_fixture()
            documents[PATHS[1]][field] = value
            self.assert_denied(documents, PATHS[1])

    def test_swarm_must_be_explicitly_inactive(self):
        for swarm in (None, {}, {"LocalNodeState": "inactive"}, {"ControlAvailable": False},
                      {"LocalNodeState": "active", "ControlAvailable": False},
                      {"LocalNodeState": "inactive", "ControlAvailable": True},
                      {"LocalNodeState": "inactive", "ControlAvailable": 0}):
            documents = metadata_fixture()
            documents[PATHS[1]]["Swarm"] = swarm
            self.assert_denied(documents, PATHS[1])
        for field, value in (("NodeID", "other-node"), ("NodeAddr", "192.0.2.1"),
                             ("Error", "synthetic-private-value"), ("RemoteManagers", [{}])):
            documents = metadata_fixture()
            documents[PATHS[1]]["Swarm"][field] = value
            self.assert_denied(documents, PATHS[1])

    def test_daemon_proxy_fields_are_explicitly_empty(self):
        for field in ("HttpProxy", "HttpsProxy", "NoProxy"):
            for value in ("synthetic-private-value", " ", None, False, []):
                documents = metadata_fixture()
                documents[PATHS[1]][field] = value
                self.assert_denied(documents, PATHS[1], "engine_proxy_configured")

    def test_proxy_go_field_names_do_not_replace_wire_fields(self):
        documents = metadata_fixture()
        for wire, wrong in (("HttpProxy", "HTTPProxy"), ("HttpsProxy", "HTTPSProxy")):
            documents[PATHS[1]][wrong] = documents[PATHS[1]].pop(wire)
        self.assert_denied(documents, PATHS[1])

    def test_all_containers_even_stopped_are_denied_without_inspection(self):
        for value in ([{"State": "exited"}], [{}], [None]):
            documents = metadata_fixture()
            documents[PATHS[2]] = value
            self.assert_denied(documents, PATHS[2], "engine_not_empty")

    def test_exactly_three_default_networks_with_unique_ids_and_names(self):
        for transform in (lambda ns: ns[:2], lambda ns: ns + [deepcopy(ns[0])],
                          lambda ns: [ns[0], ns[0], ns[2]]):
            documents = metadata_fixture()
            documents[PATHS[3]] = transform(documents[PATHS[3]])
            self.assert_denied(documents, PATHS[3])
        documents = metadata_fixture()
        documents[PATHS[3]][1]["Id"] = documents[PATHS[3]][0]["Id"]
        self.assert_denied(documents, PATHS[3])

    def test_network_fields_are_exact_not_truthy(self):
        invalid_fields = {
            "Name": ["owned-test-network", None], "Id": ["", True, "A" * 64],
            "Driver": ["overlay", None], "Scope": ["swarm", None],
            "Internal": [True, 0, None], "Attachable": [True, 0, None],
            "Ingress": [True, 0, None], "EnableIPv6": [True, 0, None],
            "Labels": [{"owner": "synthetic-private-value"}, [], ""], "Options": [None, [], ""],
        }
        for field, values in invalid_fields.items():
            for value in values:
                with self.subTest(field=field, kind=type(value).__name__):
                    documents = metadata_fixture()
                    documents[PATHS[3]][0][field] = value
                    self.assert_denied(documents, PATHS[3])
        for field in metadata_fixture()[PATHS[3]][0]:
            documents = metadata_fixture()
            del documents[PATHS[3]][0][field]
            self.assert_denied(documents, PATHS[3])

    def test_network_driver_is_bound_to_default_name(self):
        documents = metadata_fixture()
        documents[PATHS[3]][0]["Driver"] = "host"
        self.assert_denied(documents, PATHS[3])

    def test_network_options_cannot_add_unknown_or_routed_port_capability(self):
        for key, value in (
            ("com.docker.network.bridge.default_bridge", "false"),
            ("com.docker.network.bridge.enable_icc", True),
            ("com.docker.network.bridge.enable_ip_masquerade", "1"),
            ("com.docker.network.bridge.host_binding_ipv4", "192.0.2.1"),
            ("com.docker.network.bridge.name", "existing-custom-interface"),
            ("com.docker.network.driver.mtu", "0"), ("com.docker.network.driver.mtu", "01500"),
            ("com.docker.network.driver.mtu", True),
            ("com.docker.network.bridge.gateway_mode_ipv4", "routed"),
            ("com.docker.network.bridge.trusted_host_interfaces", "eth0"),
            ("synthetic-unknown-option", "synthetic-private-value"),
        ):
            documents = metadata_fixture()
            documents[PATHS[3]][0]["Options"][key] = value
            self.assert_denied(documents, PATHS[3])
        for index in (1, 2):
            documents = metadata_fixture()
            documents[PATHS[3]][index]["Options"]["synthetic-unknown-option"] = "true"
            self.assert_denied(documents, PATHS[3])

    def test_legitimate_empty_metadata_forms_and_default_option_variants(self):
        documents = metadata_fixture()
        documents[PATHS[4]] = {"Volumes": [], "Warnings": []}
        for network in documents[PATHS[3]]:
            network["Labels"] = None
        options = documents[PATHS[3]][0]["Options"]
        options["com.docker.network.bridge.enable_icc"] = "false"
        options["com.docker.network.bridge.enable_ip_masquerade"] = "false"
        options["com.docker.network.bridge.host_binding_ipv4"] = "127.0.0.1"
        self.assertFalse(inspect_engine(self.config, FakeEngine(documents))["isolation_verified"])

    def test_optional_runtime_and_network_wire_fields_can_be_empty(self):
        documents = metadata_fixture()
        info = documents[PATHS[1]]
        info["SecurityOptions"].remove("name=cgroupns")
        info["Runtimes"] = {"runc": {"path": "/usr/bin/runc", "runtimeArgs": None},
                            "io.containerd.runc.v2": {"path": "runc", "runtimeArgs": []}}
        info["Swarm"].update(NodeID="", NodeAddr="", Error="", RemoteManagers=None)
        for index, network in enumerate(documents[PATHS[3]]):
            network.update(Containers=None, ConfigOnly=False, ConfigFrom={"Network": ""})
            if index != 0:
                network["Options"] = None
        self.assertFalse(inspect_engine(self.config, FakeEngine(documents))["isolation_verified"])

    def test_optional_network_metadata_cannot_contradict_empty_defaults(self):
        for field, value in (("Containers", {"other-container": {}}), ("Containers", []),
                             ("Containers", False), ("ConfigOnly", True), ("ConfigOnly", 0),
                             ("ConfigFrom", {"Network": "other-network"}), ("ConfigFrom", None)):
            documents = metadata_fixture()
            documents[PATHS[3]][0][field] = value
            self.assert_denied(documents, PATHS[3])

    def test_empty_default_options_must_still_be_explicit(self):
        for index in (1, 2):
            documents = metadata_fixture()
            del documents[PATHS[3]][index]["Options"]
            self.assert_denied(documents, PATHS[3])

    def test_volumes_and_warnings_are_empty_but_not_arbitrary_falsy(self):
        for value in ({}, {"Volumes": None}, {"Warnings": None},
                      {"Volumes": [{"Name": "existing"}], "Warnings": None},
                      {"Volumes": False, "Warnings": None}, {"Volumes": {}, "Warnings": None},
                      {"Volumes": None, "Warnings": ["synthetic-private-value"]},
                      {"Volumes": None, "Warnings": False}):
            documents = metadata_fixture()
            documents[PATHS[4]] = value
            self.assert_denied(documents, PATHS[4])

    def test_engine_failure_is_not_retried_or_followed_by_any_other_get(self):
        class BrokenEngine(FakeEngine):
            def get_json(self, path):
                self.calls.append(path)
                raise LabError("engine_unavailable")

        engine = BrokenEngine()
        with self.assertRaisesRegex(LabError, "^engine_unavailable$"):
            inspect_engine(self.config, engine)
        self.assertEqual(engine.calls, [PATHS[0]])


if __name__ == "__main__":
    unittest.main()
