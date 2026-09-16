"""Render only the checked-in recipe, without build args or arbitrary commands."""
from pathlib import Path

from recoverykit.evidence.filesystem import read_regular

from .lock import ImageError, validate_lock


def render_recipe(lock):
    lock = validate_lock(lock)
    content = read_regular(Path(__file__).absolute().parent.parent, "Dockerfile.base", 16 * 1024)
    for marker, value in ((b"@@PLATFORM@@", lock["platform"]), (b"@@BASE_IMAGE@@", lock["base_image"])):
        if content.count(marker) != 1:
            raise ImageError("recipe_template_invalid")
        content = content.replace(marker, value.encode("ascii"))
    if b"@@" in content:
        raise ImageError("recipe_template_invalid")
    return content


def render_checksums(lock):
    lock = validate_lock(lock)
    return "".join(f"{p['sha256']}  {p['file']}\n" for p in lock["packages"]).encode("ascii")
