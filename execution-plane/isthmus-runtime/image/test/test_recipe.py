import unittest
from unittest.mock import patch

from imagekit.lock import ImageError
from imagekit.recipe import render_checksums, render_recipe
from test.test_lock import fixture


class RecipeTests(unittest.TestCase):
    def test_recipe_binds_digest_platform_and_only_base_inputs(self):
        document = fixture()
        recipe = render_recipe(document).decode()
        self.assertIn("FROM --platform=linux/amd64 " + document["base_image"], recipe)
        self.assertNotIn("@@", recipe)
        instructions = [line.strip() for line in recipe.splitlines() if line and not line.startswith("#")]
        runs = [line for line in instructions if line.startswith("RUN ")]
        self.assertEqual(len(runs), 2)
        self.assertTrue(all(line.startswith("RUN --network=none ") for line in runs))
        self.assertEqual([line for line in instructions if line.startswith("COPY ")], [
            "COPY packages/ /tmp/isthmus-base-packages/", "COPY packages.sha256 /tmp/isthmus-base-packages/SHA256SUMS"])
        for forbidden in ("ARG ", "ADD ", "VOLUME ", "EXPOSE ", "HEALTHCHECK ", "ENTRYPOINT ", "ONBUILD "):
            self.assertFalse(any(line.startswith(forbidden) for line in instructions))
        for forbidden in ("apt-get", "curl ", "wget ", "setcap ", "--mount", "|| true", "# syntax="):
            self.assertNotIn(forbidden, recipe)
        self.assertIn("USER 1000:1000", recipe)
        self.assertIn('CMD ["/bin/false"]', recipe)
        self.assertIn("sha256sum --check SHA256SUMS", recipe)
        self.assertIn("dpkg --install ./*.deb", recipe)

    def test_home_intermediate_directories_have_explicit_private_ownership(self):
        recipe = render_recipe(fixture()).decode()
        install = recipe.split("install -d -o 1000 -g 1000 -m 0700", 1)[1].split("&&", 1)[0]
        targets = install.replace("\\", " ").split()
        for path in ("/home/claude/.cache", "/home/claude/.cache/isthmus", "/home/claude/.local",
                     "/home/claude/.local/bin", "/home/claude/.claude"):
            self.assertIn(path, targets)

    def test_checksums_have_canonical_order_and_no_paths(self):
        document = fixture()
        expected = "".join(f"{p['sha256']}  {p['file']}\n" for p in document["packages"]).encode()
        document["packages"].reverse()
        self.assertEqual(render_checksums(document), expected)

    def test_missing_duplicate_or_extra_template_markers_fail(self):
        for content in (b"FROM scratch", b"@@PLATFORM@@ @@PLATFORM@@ @@BASE_IMAGE@@",
                        b"@@PLATFORM@@ @@BASE_IMAGE@@ @@UNKNOWN@@"):
            with patch("imagekit.recipe.read_regular", return_value=content), self.assertRaises(ImageError):
                render_recipe(fixture())
