"""S2b2 synthetic enrollment suite, reusing the bounded S2b Linux sandbox.

Control TLS and worker mTLS are actual component listeners in one container.
The Docker protocol tests use a fake daemon: no live bootstrap or production
deployment is claimed. Private keys are created only inside the tmpfs probe.
"""
import sys

from lab.host import Lab
from lab.mtls import probe


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit("explicit private lab root required")
    try:
        probe(Lab(sys.argv[1]), enrollment=True)
    except Exception:
        raise SystemExit("enrollment_probe_failed; inspect private bounded evidence")
