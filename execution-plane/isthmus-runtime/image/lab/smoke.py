"""Base-only, nonprivileged, no-network checks; no model/account execution."""
import json
import shlex
from imagekit.context import verify_context
from imagekit.lock import ImageError, validate_lock

SMOKE = """set -eu
test "$(id -u)" = 1000
test "$(id -g)" = 1000
for p in /home/claude /home/claude/.claude /home/claude/.cache /home/claude/.cache/isthmus /home/claude/.local /home/claude/.local/bin; do
  test "$(stat -c '%u:%g:%a' "$p")" = 1000:1000:700
done
test ! -s /etc/machine-id
test ! -e /var/lib/dbus/machine-id
test -z "$(find /home/claude -type f -print -quit)"
test "$(ls /sys/class/net)" = lo
awk '/^CapEff:/ { if ($2 != "0000000000000000") exit 1; cap=1 } /^NoNewPrivs:/ { if ($2 != 1) exit 1; nnp=1 } END { if (!cap || !nnp) exit 1 }' /proc/self/status
if touch /etc/isthmus-write-negative 2>/dev/null; then exit 1; fi
if unshare --mount /bin/true 2>/dev/null; then exit 1; fi
dpkg-query -W ca-certificates passwd procps util-linux
"""


def read(lab, name):
    return json.loads((lab.root / name).read_text())


def smoke_script(lock):
    lock = validate_lock(lock)
    commands = [SMOKE, 'audit="$(dpkg --audit)"', 'test -z "$audit"']
    for package in lock["packages"]:
        expected = package["version"] + " " + package["architecture"] + " install ok installed"
        commands.append('test "$(dpkg-query -W -f=\'${Version} ${Architecture} ${Status}\' '
                        + shlex.quote(package["name"]) + ')" = ' + shlex.quote(expected))
    commands.append("echo base-only-smoke-pass")
    return "\n".join(commands)


def smoke(lab):
    verify_context(lab.root / "context")
    image_id = read(lab, "built-image.json")["id"]
    lock = read(lab, "context/artifacts.lock.json")
    cid = lab.create_probe("smoke", image_id, ["/bin/sh", "-ec", smoke_script(lock)])
    lab.save("smoke-container.json", {"id": cid})
    lab.owned(cid)
    lab.logged("base-smoke", ["start", "-a", cid], timeout=30, stop_on_abort=cid)
    if lab.owned(cid)["State"]["ExitCode"] != 0:
        raise ValueError("smoke_failed")
    default = lab.create_probe("default", image_id, [])
    lab.save("default-container.json", {"id": default})
    result = lab.run("start", "-a", default, check=False)
    if result.returncode != 1 or lab.owned(default)["State"]["ExitCode"] != 1:
        raise ValueError("default_false_contract_failed")
    sentinel = lab.root / "context/unexpected-synthetic-input"
    with sentinel.open("x") as out:
        out.write("synthetic public negative, never a credential\n")
    try:
        try:
            verify_context(lab.root / "context")
        except ImageError:
            pass
        else:
            raise ValueError("unexpected_context_file_accepted")
    finally:
        sentinel.unlink()
    verify_context(lab.root / "context")
    if lab.baseline() != read(lab, "baseline.json"):
        raise ValueError("existing_container_baseline_changed")
    lab.save("smoke-result.json", {"base_only": True, "smoke": "pass", "default_exit": 1,
             "locked_packages_verified": len(lock["packages"]), "dpkg_audit_empty": True,
             "unexpected_context_rejected": True, "existing_containers_unchanged": True,
             "runtime_ready": False, "network_isolation_accepted": False})
    print(json.dumps({"phase": "smoke", "status": "base_only_smoke_pass"}))
