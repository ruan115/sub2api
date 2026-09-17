"""Explicit lab-only single public binary bind; production provider is unchanged."""
import base64
import hashlib
import re

from lab.host import LABEL
from lab.toolchain import BASE_ID, ENVS

TMPFS = {"/tmp": "rw,noexec,nosuid,nodev,size=64m,mode=1777",
         "/run": "rw,noexec,nosuid,nodev,size=16m,mode=1777",
         "/home/claude": "rw,noexec,nosuid,nodev,size=128m,mode=0700,uid=1000,gid=1000"}


def public_config(value):
    if (not isinstance(value, dict) or set(value) != {"trust_sha256", "ticket_public_key"}
            or not isinstance(value["trust_sha256"], str)
            or not re.fullmatch(r"[a-f0-9]{64}", value["trust_sha256"])
            or not isinstance(value["ticket_public_key"], str)):
        raise ValueError("live_public_config_rejected")
    try:
        key = base64.b64decode(value["ticket_public_key"] + "=", validate=True)
    except Exception:
        raise ValueError("live_public_config_rejected") from None
    if len(key) != 32 or base64.b64encode(key).decode().rstrip("=") != value["ticket_public_key"]:
        raise ValueError("live_public_config_rejected")
    return value


def envs(label, public):
    if label not in ("a", "b"):
        raise ValueError("live_slot_rejected")
    public_config(public)
    account = "account-live-" + label
    values = {
        "EXECUTION_ACCOUNT_HASH": hashlib.sha256(account.encode()).hexdigest()[:32],
        "EXECUTION_SLOT_ID": "slot-live-" + label, "EXECUTION_NODE_ID": "node-live",
        "EXECUTION_EPOCH": "1", "EXECUTION_RUNTIME_GENERATION": "1",
        "EXECUTION_IMAGE_DIGEST": BASE_ID, "EXECUTION_LISTEN_ADDRESS": "127.0.0.1:8093",
        "EXECUTION_HEALTHCHECK_ADDRESS": "127.0.0.1:8093",
        "EXECUTION_IDENTITY_DIRECTORY": "/run/execution/identity",
        "EXECUTION_RUNTIME_TRUST_FILE": "/run/execution/runtime-ca.pem",
        "EXECUTION_BOOTSTRAP_CA_SHA256": public["trust_sha256"],
        "EXECUTION_TICKET_PUBLIC_KEY": public["ticket_public_key"],
        "EXECUTION_UPSTREAM_BASE_URL": "https://api.anthropic.com",
        "EXECUTION_EGRESS_PROXY_URL": "http://host-agent.execution.internal:8094",
        "EXECUTION_ALLOW_FAKE_ACTIVATION": "true",
    }
    return ENVS + [key + "=" + value for key, value in values.items()]


def args(lab, label, public):
    name = lab.name + "-live-" + label
    values = ["create", "--name", name, "--hostname", name, "--label", LABEL + "=" + lab.name,
              "--network", "none", "--read-only", "--user", "1000:1000", "--cap-drop", "ALL",
              "--security-opt", "no-new-privileges", "--memory", "1g", "--memory-swap", "1g",
              "--cpus", "1", "--pids-limit", "128", "--ulimit", "core=0:0", "--ipc", "private",
              "--cgroupns", "private", "--restart", "no", "--log-driver", "local",
              "--log-opt", "max-size=5m", "--log-opt", "max-file=2", "--workdir", "/",
              "--entrypoint", "/usr/bin/timeout", "--volume", str(lab.root / "live-bin/worker") + ":/worker:ro"]
    for target, value in TMPFS.items():
        values += ["--tmpfs", target + ":" + value]
    for value in envs(label, public):
        values += ["--env", value]
    return values + [BASE_ID, "--kill-after=5", "90", "/worker"]


def validate(value, lab, label, public):
    h, c = value["HostConfig"], value["Config"]
    name = lab.name + "-live-" + label
    if (not re.fullmatch(r"[a-f0-9]{64}", value["Id"]) or value["Name"] != "/" + name
            or value["Image"] != BASE_ID or c["Image"] != BASE_ID
            or c["Hostname"] != name or c["User"] != "1000:1000"
            or c.get("Labels", {}).get(LABEL) != lab.name or sorted(c["Env"]) != sorted(envs(label, public))
            or c["Entrypoint"] != ["/usr/bin/timeout"] or c["Cmd"] != ["--kill-after=5", "90", "/worker"]
            or c["WorkingDir"] != "/"):
        raise ValueError("live_instance_identity_mismatch")
    if (h["Privileged"] or not h["ReadonlyRootfs"] or h["NetworkMode"] != "none"
            or h["Memory"] != 1024**3 or h["MemorySwap"] != 1024**3 or h["NanoCpus"] != 10**9
            or h["PidsLimit"] != 128 or h.get("CapAdd") or h["CapDrop"] != ["ALL"]
            or h["SecurityOpt"] != ["no-new-privileges"]
            or h.get("PortBindings") or h.get("Devices") or h.get("DeviceRequests")
            or h.get("PidMode") not in ("", "private") or h["IpcMode"] != "private"
            or h["CgroupnsMode"] != "private" or h["RestartPolicy"]["Name"] != "no"
            or h["Tmpfs"] != TMPFS or h["Ulimits"] != [{"Name": "core", "Hard": 0, "Soft": 0}]):
        raise ValueError("live_instance_isolation_mismatch")
    if h.get("Binds") != [str(lab.root / "live-bin/worker") + ":/worker:ro"] or h.get("Mounts"):
        raise ValueError("live_bind_request_mismatch")
    mounts = value["Mounts"]
    binds = [m for m in mounts if m["Type"] == "bind"]
    if (len(binds) != 1 or binds[0]["Source"] != str(lab.root / "live-bin/worker")
            or binds[0]["Destination"] != "/worker" or binds[0]["RW"]
            or any(m["Type"] not in ("bind", "tmpfs") for m in mounts)
            or any(m["Type"] == "tmpfs" and m["Destination"] not in TMPFS for m in mounts)):
        raise ValueError("live_bind_actual_mismatch")
