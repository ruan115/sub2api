"""Real Docker public-certificate management probe, not a workload launcher."""
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[5]
sys.path[:0] = [str(ROOT / "recovery/tooling"), str(Path(__file__).resolve().parents[2])]
