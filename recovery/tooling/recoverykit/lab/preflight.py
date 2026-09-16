"""Fail-closed metadata checks, not a VM or network-isolation attestation.

Only the supplied engine performs I/O. The five GETs cannot prove outer-VM
mounts, forwarding, firewall rules, image identity, or a truthful daemon; even
success never permits execution. No cleanup, repair, or resource creation is
performed here.
"""

import re

from .config import LabError


_API = (1, 43)
_VERSION = re.compile(r"(?:0|[1-9][0-9]{0,2})\.(?:0|[1-9][0-9]{0,2})\Z")
_NETWORK_ID = re.compile(r"[a-f0-9]{64}\Z")
_INTEGER = re.compile(r"[1-9][0-9]{0,4}\Z")
_ARCHITECTURES = {"amd64": "amd64", "x86_64": "amd64", "arm64": "arm64", "aarch64": "arm64"}
_SECURITY = frozenset({"name=apparmor", "name=seccomp,profile=builtin", "name=cgroupns"})
_RUNC_PATHS = frozenset({"runc", "/bin/runc", "/sbin/runc", "/usr/bin/runc", "/usr/sbin/runc"})
_DRIVERS = {"bridge": "bridge", "host": "host", "none": "null"}
_BRIDGE_PREFIX = "com.docker.network.bridge."
_BRIDGE_OPTIONS = {
    _BRIDGE_PREFIX + "default_bridge": frozenset({"true"}),
    _BRIDGE_PREFIX + "enable_icc": frozenset({"true", "false"}),
    _BRIDGE_PREFIX + "enable_ip_masquerade": frozenset({"true", "false"}),
    _BRIDGE_PREFIX + "host_binding_ipv4": frozenset({"0.0.0.0", "127.0.0.1"}),
    _BRIDGE_PREFIX + "name": frozenset({"docker0"}),
    _BRIDGE_PREFIX + "gateway_mode_ipv4": frozenset({"nat"}),
}
_MTU = "com.docker.network.driver.mtu"


def _api_version(value):
    if type(value) is not str or not _VERSION.fullmatch(value):
        raise LabError("engine_api_invalid")
    return tuple(int(part) for part in value.split("."))


def _version(document):
    if type(document) is not dict:
        raise LabError("engine_response_invalid")
    maximum = _api_version(document.get("ApiVersion"))
    minimum = _api_version(document.get("MinAPIVersion"))
    if not minimum <= _API <= maximum:
        raise LabError("engine_api_unsupported")


def _info(config, document):
    if type(document) is not dict:
        raise LabError("engine_response_invalid")
    architecture = document.get("Architecture")
    if (document.get("ID") != config.engine_id or document.get("Name") != config.engine_name
            or document.get("OSType") != "linux" or type(architecture) is not str
            or _ARCHITECTURES.get(architecture) != config.architecture):
        raise LabError("engine_identity_mismatch")

    # These are JSON wire names, not the differently capitalized Go members.
    for field in ("HttpProxy", "HttpsProxy", "NoProxy"):
        if document.get(field) != "":
            raise LabError("engine_proxy_configured")

    for field in ("Containers", "ContainersRunning", "ContainersPaused", "ContainersStopped"):
        value = document.get(field)
        if type(value) is not int or value < 0:
            raise LabError("engine_response_invalid")
        if value != 0:
            raise LabError("engine_not_empty")
    for field in ("NCPU", "MemTotal"):
        value = document.get(field)
        if type(value) is not int or not 0 < value <= (1 << 63) - 1:
            raise LabError("engine_response_invalid")

    security = document.get("SecurityOptions")
    if (type(security) is not list or not 2 <= len(security) <= len(_SECURITY)
            or any(type(option) is not str or option not in _SECURITY for option in security)
            or len(set(security)) != len(security)
            or "name=apparmor" not in security or "name=seccomp,profile=builtin" not in security):
        raise LabError("engine_security_unsupported")

    runtimes = document.get("Runtimes")
    if (document.get("DefaultRuntime") != "runc" or type(runtimes) is not dict
            or "runc" not in runtimes or set(runtimes) - {"runc", "io.containerd.runc.v2"}):
        raise LabError("engine_runtime_unsupported")
    for runtime in runtimes.values():
        if (type(runtime) is not dict or type(runtime.get("path")) is not str
                or runtime["path"] not in _RUNC_PATHS
                or ("runtimeArgs" in runtime and runtime["runtimeArgs"] not in (None, []))):
            raise LabError("engine_runtime_unsupported")

    swarm = document.get("Swarm")
    if (type(swarm) is not dict or swarm.get("LocalNodeState") != "inactive"
            or swarm.get("ControlAvailable") is not False):
        raise LabError("engine_swarm_unsupported")
    # Optional inactive-state identity fields must not contradict the core state.
    for field in ("NodeID", "NodeAddr", "Error"):
        if field in swarm and swarm[field] != "":
            raise LabError("engine_swarm_unsupported")
    if "RemoteManagers" in swarm and swarm["RemoteManagers"] not in (None, []):
        raise LabError("engine_swarm_unsupported")


def _network_options(name, options):
    if options is None and name != "bridge":
        return
    if type(options) is not dict:
        raise LabError("engine_networks_invalid")
    if name != "bridge":
        if options:
            raise LabError("engine_networks_invalid")
        return
    if (options.get(_BRIDGE_PREFIX + "default_bridge") != "true"
            or options.get(_BRIDGE_PREFIX + "name") != "docker0"):
        raise LabError("engine_networks_invalid")
    for key, value in options.items():
        if type(value) is not str:
            raise LabError("engine_networks_invalid")
        if key == _MTU:
            if not _INTEGER.fullmatch(value) or not 576 <= int(value) <= 9000:
                raise LabError("engine_networks_invalid")
        elif key not in _BRIDGE_OPTIONS or value not in _BRIDGE_OPTIONS[key]:
            raise LabError("engine_networks_invalid")


def _networks(document):
    if type(document) is not list or len(document) != 3:
        raise LabError("engine_networks_invalid")
    names, identifiers = set(), set()
    for network in document:
        if type(network) is not dict:
            raise LabError("engine_networks_invalid")
        name, identifier = network.get("Name"), network.get("Id")
        if (type(name) is not str or name not in _DRIVERS or name in names
                or type(identifier) is not str or not _NETWORK_ID.fullmatch(identifier)
                or identifier in identifiers or network.get("Driver") != _DRIVERS[name]
                or network.get("Scope") != "local"):
            raise LabError("engine_networks_invalid")
        names.add(name)
        identifiers.add(identifier)
        for field in ("Internal", "Attachable", "Ingress", "EnableIPv6"):
            if network.get(field) is not False:
                raise LabError("engine_networks_invalid")
        if "Labels" not in network or not (network["Labels"] is None or type(network["Labels"]) is dict and not network["Labels"]):
            raise LabError("engine_networks_invalid")
        if "Options" not in network:
            raise LabError("engine_networks_invalid")
        _network_options(name, network["Options"])
        # The list API may omit these details. If present, reject contradiction
        # with a default, unoccupied network rather than silently ignoring it.
        if "Containers" in network and network["Containers"] is not None and (type(network["Containers"]) is not dict or network["Containers"]):
            raise LabError("engine_networks_invalid")
        if "ConfigOnly" in network and network["ConfigOnly"] is not False:
            raise LabError("engine_networks_invalid")
        if "ConfigFrom" in network and network["ConfigFrom"] != {"Network": ""}:
            raise LabError("engine_networks_invalid")


def _volumes(document):
    if type(document) is not dict or "Volumes" not in document or "Warnings" not in document:
        raise LabError("engine_response_invalid")
    volumes = document["Volumes"]
    if volumes is not None and type(volumes) is not list:
        raise LabError("engine_response_invalid")
    if volumes:
        raise LabError("engine_not_empty")
    warnings = document["Warnings"]
    if warnings is not None and (type(warnings) is not list or warnings):
        raise LabError("engine_warnings_present")


def inspect_engine(config, engine) -> dict:
    """Check a dedicated endpoint's current metadata; grant no execution right."""
    _version(engine.get_json("/version"))
    _info(config, engine.get_json("/v1.43/info"))
    containers = engine.get_json("/v1.43/containers/json?all=1")
    if type(containers) is not list:
        raise LabError("engine_response_invalid")
    if containers:
        raise LabError("engine_not_empty")
    _networks(engine.get_json("/v1.43/networks"))
    _volumes(engine.get_json("/v1.43/volumes"))
    return {
        "status": "docker_endpoint_checked", "isolation_verified": False,
        "execution_permitted": False, "created_resources": 0,
        "container_count": 0, "volume_count": 0, "network_count": 3,
    }
