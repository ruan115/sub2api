import sys

from lab.host import Lab
from lab.livebootstrap.probe import probe

if len(sys.argv) != 2:
    raise SystemExit("explicit private lab root required")
try:
    probe(Lab(sys.argv[1]))
except Exception:
    raise SystemExit("live_bootstrap_probe_failed; inspect private bounded evidence")
