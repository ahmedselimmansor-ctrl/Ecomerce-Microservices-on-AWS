"""
Runs ``scripts/contract-parity.py`` as a test.

The parity check is stdlib-only so it can run in the development container,
where there is no pip and therefore no pydantic. Invoking it from pytest as well
means CI fails on drift through the normal test path rather than only through a
separate make target somebody might not run.
"""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[3]


def test_typescript_and_python_contracts_agree() -> None:
    result = subprocess.run(
        [sys.executable, str(REPO / "scripts/contract-parity.py")],
        capture_output=True,
        text=True,
        check=False,
    )

    assert result.returncode == 0, (
        "the TypeScript and Python contracts have drifted:\n" + result.stdout
    )
