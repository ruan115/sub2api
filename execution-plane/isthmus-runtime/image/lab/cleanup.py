"""Remove only the recorded, labelled resources of one reviewed lab run."""
import json
from lab.host import LABEL


def cleanup(lab):
    containers = []
    for name in ("prepare", "builder", "smoke", "default"):
        path = lab.root / (name + "-container.json")
        if path.exists():
            cid = json.loads(path.read_text())["id"]
            lab.owned(cid)
            containers.append(cid)
    volume = None
    path = lab.root / "builder-volume.json"
    if path.exists():
        volume = json.loads(path.read_text())["name"]
        if volume != lab.name + "-buildkit-cache":
            raise ValueError("cleanup_volume_name_mismatch")
        item = lab.json("volume", "inspect", volume)[0]
        if item["Name"] != volume or item.get("Labels", {}).get(LABEL) != lab.name:
            raise ValueError("cleanup_volume_owner_mismatch")
    # Verify every target before the first removal. Never prune, remove images,
    # traverse a source directory, or accept names without recorded exact IDs.
    lab.save("cleanup-targets.json", {"containers": containers, "volume": volume})
    for cid in containers:
        lab.owned(cid)
        lab.run("stop", "--time", "5", cid)
        lab.owned(cid)
        lab.run("rm", cid)
    if volume:
        item = lab.json("volume", "inspect", volume)[0]
        if item.get("Labels", {}).get(LABEL) != lab.name:
            raise ValueError("cleanup_volume_owner_changed")
        lab.run("volume", "rm", volume)
    baseline = json.loads((lab.root / "baseline.json").read_text())
    if lab.baseline() != baseline:
        raise ValueError("existing_container_baseline_changed")
    lab.save("cleanup.json", {"removed_containers": len(containers), "removed_cache_volumes": int(volume is not None),
                              "existing_containers_unchanged": True, "images_and_evidence_preserved": True})
    print(json.dumps({"phase": "cleanup", "status": "owned_resources_removed"}))
