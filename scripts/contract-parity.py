#!/usr/bin/env python3
"""
Asserts that the TypeScript and Python contracts describe the same thing.

``libs/ts-contracts`` and ``libs/py-contracts`` are one contract expressed
twice. Nothing generates one from the other — code generation here would mean a
build step between editing a schema and seeing the error, and the schemas are
small enough that the duplication is cheaper than the machinery. What is *not*
cheaper is the duplication silently drifting, which is what this closes.

It is deliberately static. It parses the TypeScript with a regex and the Python
with ``ast``, so it needs neither ``tsc`` nor ``pydantic`` installed — which
matters, because there is no pip in the development container and CI would
otherwise be the first thing to notice a mismatch.

What it checks:

  1. every model listed below exists on both sides
  2. their field sets are identical, comparing the TypeScript name against the
     Python field's ``alias`` (or its name when there is no alias)
  3. ``ERROR_CODES`` holds the same values in the same order

What it does NOT check: types, constraints, defaults, or optionality. Those are
real gaps. A field that is ``z.number()`` in TypeScript and ``str`` in Python
passes this and fails at runtime. The mesh size is stated rather than implied.

Usage:  scripts/contract-parity.py
Exit:   0 clean, 1 findings.
"""

from __future__ import annotations

import ast
import re
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent

TS_DIR = REPO / "libs/ts-contracts/src"
PY_DIR = REPO / "libs/py-contracts/souq_contracts"

# The models the Python side implements. It is a subset on purpose — Python only
# runs search and recommendations, so mirroring the cart and order schemas would
# be dead code that still has to be kept in step.
#
# A model added here and missing on either side is a failure, which is what
# makes this list the register of what Python is expected to cover.
PAIRED_MODELS: dict[str, str] = {
    # TypeScript export  ->  Python class
    "Money": "Money",
    "Address": "Address",
    "Image": "Image",
    "SearchRequest": "SearchRequest",
    "SearchFacetValue": "SearchFacetValue",
    "SearchFacet": "SearchFacet",
    "SearchHit": "SearchHit",
    "SearchResponse": "SearchResponse",
    "SuggestResponse": "SuggestResponse",
    "RecommendationRequest": "RecommendationRequest",
    "RecommendationResponse": "RecommendationResponse",
}

# Fields the Python side legitimately omits or adds, with the reason. An
# unexplained difference is a failure; a listed one is a decision.
ALLOWED_DIFFERENCES: dict[str, dict[str, str]] = {
    "ProblemDetails": {
        "*": "extra='allow' — RFC 9457 extension members are top-level siblings",
    },
}


# ---------------------------------------------------------------------------
# TypeScript
# ---------------------------------------------------------------------------

def read_ts_sources() -> str:
    return "\n".join(p.read_text() for p in sorted(TS_DIR.glob("*.ts")) if not p.name.endswith(".test.ts"))


def ts_object_fields(source: str, export_name: str) -> set[str] | None:
    """
    The field names of ``export const <name> = z.object({ ... })``.

    A brace-counting scan rather than a regex for the body: nested ``z.object``
    calls are routine and a non-greedy match stops at the first inner brace,
    which silently yields a short field list and a passing test.
    """
    match = re.search(
        rf"export const {re.escape(export_name)}\s*=\s*z\.object\(\{{", source
    )
    if not match:
        return None

    start = match.end() - 1
    depth = 0
    for i in range(start, len(source)):
        if source[i] == "{":
            depth += 1
        elif source[i] == "}":
            depth -= 1
            if depth == 0:
                body = source[start + 1 : i]
                break
    else:
        return None

    # Top-level keys only. A nested object's keys sit at depth > 0 and belong to
    # a different model.
    fields: set[str] = set()
    depth = 0
    for line in body.splitlines():
        stripped = line.strip()
        if depth == 0:
            key = re.match(r"([A-Za-z_]\w*)\s*:", stripped)
            if key:
                fields.add(key.group(1))
        depth += line.count("{") + line.count("(") + line.count("[")
        depth -= line.count("}") + line.count(")") + line.count("]")
        depth = max(0, depth)

    return fields


def ts_error_codes(source: str) -> list[str]:
    match = re.search(r"export const ERROR_CODES = \[(.*?)\] as const;", source, re.S)
    if not match:
        return []
    return re.findall(r"'([A-Z_]+)'", match.group(1))


# ---------------------------------------------------------------------------
# Python
# ---------------------------------------------------------------------------

def read_py_models() -> dict[str, set[str]]:
    """
    Class name -> the wire names of its fields.

    The wire name is the ``alias`` when the field has one, because that is what
    actually appears in JSON. Comparing Python's snake_case attribute names
    against TypeScript's camelCase would report every aliased field as a
    mismatch and the check would be abandoned within a week.
    """
    models: dict[str, set[str]] = {}

    for path in sorted(PY_DIR.glob("*.py")):
        tree = ast.parse(path.read_text(), filename=str(path))

        for node in ast.walk(tree):
            if not isinstance(node, ast.ClassDef):
                continue

            fields: set[str] = set()
            for statement in node.body:
                if not isinstance(statement, ast.AnnAssign) or not isinstance(
                    statement.target, ast.Name
                ):
                    continue

                name = statement.target.id
                if name.startswith("_") or name == "model_config":
                    continue

                fields.add(_alias_of(statement) or name)

            if fields:
                models[node.name] = fields

    return models


def _alias_of(statement: ast.AnnAssign) -> str | None:
    """The ``alias=`` argument of a ``Field(...)`` default, if there is one."""
    if not isinstance(statement.value, ast.Call):
        return None
    func = statement.value.func
    if not (isinstance(func, ast.Name) and func.id == "Field"):
        return None

    for keyword in statement.value.keywords:
        if keyword.arg == "alias" and isinstance(keyword.value, ast.Constant):
            return str(keyword.value.value)
    return None


def py_error_codes() -> list[str]:
    tree = ast.parse((PY_DIR / "primitives.py").read_text())

    for node in ast.walk(tree):
        if isinstance(node, ast.AnnAssign) and isinstance(node.target, ast.Name):
            if node.target.id == "ERROR_CODES" and isinstance(node.value, ast.Tuple):
                return [
                    e.value for e in node.value.elts
                    if isinstance(e, ast.Constant) and isinstance(e.value, str)
                ]
    return []


# ---------------------------------------------------------------------------

def main() -> int:
    if not PY_DIR.exists():
        print("  libs/py-contracts is not present; nothing to compare")
        return 0

    ts_source = read_ts_sources()
    py_models = read_py_models()
    findings: list[str] = []

    for ts_name, py_name in sorted(PAIRED_MODELS.items()):
        ts_fields = ts_object_fields(ts_source, ts_name)
        py_fields = py_models.get(py_name)

        if ts_fields is None:
            findings.append(f"{ts_name}: no `export const {ts_name} = z.object(` in libs/ts-contracts")
            continue
        if py_fields is None:
            findings.append(f"{py_name}: no such class in libs/py-contracts")
            continue

        allowed = ALLOWED_DIFFERENCES.get(ts_name, {})
        if "*" in allowed:
            continue

        for missing in sorted(ts_fields - py_fields - set(allowed)):
            findings.append(f"{ts_name}.{missing}: in TypeScript, missing from Python")
        for extra in sorted(py_fields - ts_fields - set(allowed)):
            findings.append(f"{py_name}.{extra}: in Python, missing from TypeScript")

    ts_codes = ts_error_codes(ts_source)
    codes = py_error_codes()

    if ts_codes != codes:
        for missing in [c for c in ts_codes if c not in codes]:
            findings.append(f"ERROR_CODES.{missing}: in TypeScript, missing from Python")
        for extra in [c for c in codes if c not in ts_codes]:
            findings.append(f"ERROR_CODES.{extra}: in Python, missing from TypeScript")
        if sorted(ts_codes) == sorted(codes):
            findings.append(
                "ERROR_CODES: same values, different order — the lists are grouped by "
                "status code and the grouping is the documentation"
            )

    for finding in findings:
        print(f"  {finding}")

    checked = len(PAIRED_MODELS)
    print(
        f"\n  {checked} paired models, {len(ts_codes)} error codes, "
        f"{len(findings)} finding(s)"
    )
    print("  (names only — types, constraints and optionality are not compared)")

    return 1 if findings else 0


if __name__ == "__main__":
    sys.exit(main())
