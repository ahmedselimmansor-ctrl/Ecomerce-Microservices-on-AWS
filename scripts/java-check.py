#!/usr/bin/env python3
"""
A static cross-reference check for the two Java services.

There is no JDK in the development container, so `mvn compile` cannot run here
and CI is the first thing that ever sees these files. That gap already produced
one real bug: a controller called `TokenService.describe(...)`, a method that
was never written, and nothing local said so.

This closes the most valuable part of that gap without a compiler. It resolves
every type name a file uses and every call it makes against a type we declare,
and reports the ones that cannot exist:

  1. unresolvable imports of our own packages
  2. simple type names that are neither imported, in the same package,
     java.lang, nor nested in the file
  3. calls to `OurType.method(...)` where the method is not declared
  4. `new OurType(...)` with an arity no constructor accepts
  5. package declarations that disagree with the directory

It deliberately does NOT try to be a type checker. It has no type inference, so
a call through a local variable is invisible to it, and generics, overload
resolution and assignability are all out of scope. It is a net with a known
mesh size: everything it catches is a genuine compile error, and passing it
means far less than `mvn compile` passing.

Usage:  scripts/java-check.py [root ...]      (default: services/*/src)
Exit:   0 clean, 1 findings.
"""

from __future__ import annotations

import re
import sys
from dataclasses import dataclass, field
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent

# Types the JDK and our dependencies provide. Anything here resolves without a
# declaration of ours. Kept explicit rather than "assume unknown is fine",
# because "assume fine" is what lets a missing import through.
JAVA_LANG = {
    "Object", "String", "Integer", "Long", "Double", "Float", "Boolean", "Byte",
    "Short", "Character", "Number", "Math", "System", "Thread", "Runnable",
    "Exception", "RuntimeException", "Error", "Throwable", "IllegalStateException",
    "IllegalArgumentException", "UnsupportedOperationException", "NullPointerException",
    "SecurityException", "InterruptedException", "ClassCastException", "Class",
    "StringBuilder", "Iterable", "Comparable", "CharSequence", "Enum", "Record",
    "AutoCloseable", "Cloneable", "ThreadLocal", "Process", "Package", "Void",
    "NumberFormatException", "ArithmeticException", "IndexOutOfBoundsException",
    "StackOverflowError", "OutOfMemoryError", "Deprecated", "Override",
    "SuppressWarnings", "FunctionalInterface", "SafeVarargs",
}

# Annotations and marker types that come from frameworks. Their presence is
# checked by the import scan below; this set only stops them being reported as
# unresolved when they are used unqualified after a wildcard import.
KEYWORDS = {
    "if", "for", "while", "switch", "return", "new", "this", "super", "try",
    "catch", "finally", "throw", "throws", "case", "default", "break", "continue",
    "do", "else", "instanceof", "assert", "synchronized", "yield", "record",
    "var", "void", "int", "long", "double", "float", "boolean", "byte", "short",
    "char", "class", "interface", "enum", "extends", "implements", "package",
    "import", "public", "private", "protected", "static", "final", "abstract",
    "native", "transient", "volatile", "strictfp", "sealed", "permits", "non",
}

PRIMITIVES = {"int", "long", "double", "float", "boolean", "byte", "short", "char", "void", "var"}


# --------------------------------------------------------------------------
# Lexical preparation
# --------------------------------------------------------------------------

def strip_comments_and_strings(src: str) -> str:
    """
    Blank out text blocks, string and char literals, and comments.

    Order matters: text blocks first, because a triple-quoted block routinely
    contains SQL with `--` in it, and a naive comment pass would eat the rest of
    the line and unbalance everything after it.
    """
    out = re.sub(r'"""(?:.|\n)*?"""', '""', src)
    out = re.sub(r'"(?:[^"\\\n]|\\.)*"', '""', out)
    out = re.sub(r"'(?:[^'\\\n]|\\.)'", "' '", out)
    out = re.sub(r"/\*(?:.|\n)*?\*/", lambda m: "\n" * m.group(0).count("\n"), out)
    out = re.sub(r"//[^\n]*", "", out)
    return out


def split_top_level(text: str) -> list[str]:
    """Split an argument list on commas that are not inside brackets."""
    parts, depth, current = [], 0, []
    for ch in text:
        if ch in "([{<":
            depth += 1
        elif ch in ")]}>":
            depth -= 1
        if ch == "," and depth == 0:
            parts.append("".join(current))
            current = []
        else:
            current.append(ch)
    tail = "".join(current)
    if tail.strip() or parts:
        parts.append(tail)
    return [p for p in parts if p.strip()]


# --------------------------------------------------------------------------
# Model
# --------------------------------------------------------------------------

@dataclass
class TypeInfo:
    name: str
    package: str
    kind: str                              # class | interface | enum | record
    methods: dict[str, set[int]] = field(default_factory=dict)   # name -> arities
    ctor_arities: set[int] = field(default_factory=set)
    constants: set[str] = field(default_factory=set)
    supertypes: list[str] = field(default_factory=list)
    file: Path = Path()

    @property
    def fqn(self) -> str:
        return f"{self.package}.{self.name}"


@dataclass
class FileInfo:
    path: Path
    package: str
    imports: dict[str, str]                # simple name -> fqn
    static_imports: set[str]
    wildcard_packages: list[str]
    declared: list[str]                    # simple names declared in this file
    body: str
    fields: dict[str, str] = field(default_factory=dict)   # field name -> declared type


DECL = re.compile(
    r"\b(?:public|protected|private|static|final|abstract|sealed|non-sealed)?\s*"
    r"(?:public|protected|private|static|final|abstract|sealed|non-sealed|\s)*"
    r"\b(class|interface|enum|record)\s+([A-Z]\w*)"
)

# `Type name(args)` or `Type name(args) throws ...` at the start of a member.
METHOD = re.compile(
    r"^[ \t]*(?:@\w+(?:\([^)]*\))?[ \t]*)*"
    r"(?:(?:public|protected|private|static|final|abstract|synchronized|native|default|strictfp)[ \t]+)*"
    r"(?:<[^>]{0,200}>[ \t]+)?"
    r"([\w.$]+(?:<[^;{=]{0,200}>)?(?:\[\])*)[ \t]+"
    r"(\w+)[ \t]*\(",
    re.MULTILINE,
)

CTOR = re.compile(
    r"^[ \t]*(?:@\w+(?:\([^)]*\))?[ \t]*)*"
    r"(?:(?:public|protected|private)[ \t]+)?"
    r"(%s)[ \t]*\(",
    re.MULTILINE,
)


def find_matching(text: str, open_index: int) -> int:
    """Index of the bracket closing the one at open_index, or -1."""
    pairs = {"(": ")", "{": "}", "<": ">"}
    closer = pairs[text[open_index]]
    opener = text[open_index]
    depth = 0
    for i in range(open_index, len(text)):
        if text[i] == opener:
            depth += 1
        elif text[i] == closer:
            depth -= 1
            if depth == 0:
                return i
    return -1


def parse_file(path: Path) -> tuple[FileInfo, list[TypeInfo]]:
    raw = path.read_text()
    body = strip_comments_and_strings(raw)

    pkg_match = re.search(r"^\s*package\s+([\w.]+)\s*;", body, re.MULTILINE)
    package = pkg_match.group(1) if pkg_match else ""

    imports: dict[str, str] = {}
    static_imports: set[str] = set()
    wildcards: list[str] = []

    for m in re.finditer(r"^\s*import\s+(static\s+)?([\w.*]+)\s*;", body, re.MULTILINE):
        is_static, target = bool(m.group(1)), m.group(2)
        if target.endswith(".*"):
            wildcards.append(target[:-2])
            continue
        simple = target.rsplit(".", 1)[-1]
        if is_static:
            static_imports.add(simple)
        else:
            imports[simple] = target

    types: list[TypeInfo] = []
    declared: list[str] = []

    for m in DECL.finditer(body):
        kind, name = m.group(1), m.group(2)
        declared.append(name)

        info = TypeInfo(name=name, package=package, kind=kind, file=path)

        # Supertypes, so a class extending something we cannot see is exempt
        # from the "method must be declared" check.
        header_end = body.find("{", m.end())
        header = body[m.end():header_end] if header_end > 0 else ""
        for clause in re.finditer(r"\b(?:extends|implements)\s+([^{]+)", header):
            for st in split_top_level(clause.group(1)):
                st = st.strip().split("<")[0].strip()
                if st:
                    info.supertypes.append(st.rsplit(".", 1)[-1])

        # Record components become accessors and the canonical constructor.
        if kind == "record":
            paren = body.find("(", m.end())
            if 0 < paren < (header_end if header_end > 0 else len(body)):
                close = find_matching(body, paren)
                components = split_top_level(body[paren + 1:close])
                for comp in components:
                    tokens = re.findall(r"\w+", comp)
                    if tokens:
                        info.methods.setdefault(tokens[-1], set()).add(0)
                info.ctor_arities.add(len(components))

        # Body of this type, bounded by its own braces, so a nested type's
        # members are not attributed to its enclosing type.
        if header_end > 0:
            close = find_matching(body, header_end)
            type_body = body[header_end:close if close > 0 else len(body)]
        else:
            type_body = ""

        for mm in METHOD.finditer(type_body):
            ret, mname = mm.group(1), mm.group(2)
            if mname in KEYWORDS or ret in KEYWORDS - PRIMITIVES:
                continue
            open_paren = type_body.index("(", mm.end() - 1)
            close_paren = find_matching(type_body, open_paren)
            if close_paren < 0:
                continue
            arity = len(split_top_level(type_body[open_paren + 1:close_paren]))
            info.methods.setdefault(mname, set()).add(arity)

        for cm in re.finditer(CTOR.pattern % re.escape(name), type_body, re.MULTILINE):
            open_paren = type_body.index("(", cm.end() - 1)
            close_paren = find_matching(type_body, open_paren)
            if close_paren < 0:
                continue
            info.ctor_arities.add(len(split_top_level(type_body[open_paren + 1:close_paren])))

        if kind == "enum" and header_end > 0:
            first = type_body[1:type_body.find(";") if ";" in type_body else len(type_body)]
            for const in re.findall(r"\b([A-Z][A-Z0-9_]*)\b", first):
                info.constants.add(const)

        for const in re.findall(
            r"\bstatic\s+(?:final\s+)?[\w.<>\[\]]+\s+([A-Z][A-Z0-9_]*)\s*[=;]", type_body
        ):
            info.constants.add(const)

        types.append(info)

    # Instance fields, so `catalogue.batch(...)` can be resolved to the
    # declared type of `catalogue`. Only fields — a local variable's type is
    # often `var`, and tracking scopes is where this stops being a regex.
    fields: dict[str, str] = {}
    for fm in re.finditer(
            r"^[ \t]+(?:private|protected|public)[ \t]+(?:final[ \t]+)?"
            r"(?!static\b)([A-Z]\w*)(?:<[^;=]{0,200}>)?[ \t]+(\w+)[ \t]*[;=]",
            body, re.MULTILINE):
        fields[fm.group(2)] = fm.group(1)

    return FileInfo(path, package, imports, static_imports, wildcards, declared, body,
                    fields), types


# --------------------------------------------------------------------------
# Checks
# --------------------------------------------------------------------------

def check(roots: list[Path]) -> int:
    files: list[FileInfo] = []
    by_fqn: dict[str, TypeInfo] = {}
    by_simple: dict[str, list[TypeInfo]] = {}
    nested_in: dict[str, set[str]] = {}
    java_files: list[Path] = []

    for root in roots:
        java_files.extend(sorted(p.resolve() for p in root.rglob("*.java")))

    if not java_files:
        print("no Java sources found")
        return 0

    for path in java_files:
        info, types = parse_file(path)
        files.append(info)
        siblings = {t.name for t in types}
        for t in types:
            by_fqn[t.fqn] = t
            by_simple.setdefault(t.name, []).append(t)
            # Nested types are also reachable as Outer.Nested, and every type in
            # a file can name every other one that way.
            nested_in[t.name] = siblings - {t.name}
            if len(types) > 1 and t is not types[0]:
                by_fqn[f"{types[0].fqn}.{t.name}"] = t

    findings: list[str] = []

    def report(path: Path, message: str) -> None:
        try:
            shown = path.resolve().relative_to(REPO)
        except ValueError:
            shown = path
        findings.append(f"{shown}: {message}")

    ours = {t.name for t in by_fqn.values()}

    for f in files:
        # ---- 5. package declaration matches the directory --------------
        expected = None
        parts = f.path.parts
        for marker in ("java",):
            if marker in parts:
                idx = len(parts) - 1 - parts[::-1].index(marker)
                expected = ".".join(parts[idx + 1:-1])
                break
        if expected and f.package != expected:
            report(f.path, f"package is '{f.package}' but the directory says '{expected}'")

        # ---- 1. imports of our own packages resolve --------------------
        for simple, fqn in f.imports.items():
            if not fqn.startswith("dev.souq"):
                continue
            if fqn in by_fqn:
                continue
            # `import a.b.Outer.Nested;`
            head, _, tail = fqn.rpartition(".")
            if head in by_fqn and tail in {n.name for n in by_simple.get(tail, [])}:
                continue
            report(f.path, f"imports {fqn}, which is not declared anywhere")

        visible = set(f.declared) | set(f.imports) | set(f.static_imports) | JAVA_LANG

        # ---- 2 + 3 + 4. usages ----------------------------------------
        # `new Xxx(`
        for m in re.finditer(
                r"\bnew\s+([A-Z]\w*)(?:\.([A-Z]\w*))?\s*(?:<[^>()]{0,120}>)?\s*\(", f.body):
            outer, nested = m.group(1), m.group(2)
            if nested:
                # Only meaningful when the outer type is one of ours. A builder
                # on a third-party class (RSAKey.Builder) is invisible here.
                if outer in nested_in and nested not in nested_in[outer]:
                    report(f.path,
                           f"`new {outer}.{nested}(...)` but {outer} declares no nested {nested}")
                if outer not in nested_in:
                    continue
                name = nested
            else:
                name = outer
            if name not in visible and name not in ours:
                report(f.path, f"`new {name}(...)` but {name} is not imported or declared")
                continue
            if name in f.declared or (name in visible and name in ours):
                candidates = [t for t in by_simple.get(name, [])
                              if t.package == f.package or f.imports.get(name) == t.fqn]
                if len(candidates) == 1:
                    target = candidates[0]
                    open_paren = m.end() - 1
                    close_paren = find_matching(f.body, open_paren)
                    if close_paren > 0 and target.ctor_arities:
                        arity = len(split_top_level(f.body[open_paren + 1:close_paren]))
                        if arity not in target.ctor_arities:
                            report(f.path,
                                   f"`new {name}(...)` passes {arity} argument(s); "
                                   f"declared constructors take {sorted(target.ctor_arities)}")

        # `field.method(...)` where the field's declared type is one of ours.
        for m in re.finditer(r"(?<![\w.])([a-z]\w*)\.(\w+)\s*\(", f.body):
            receiver, member = m.group(1), m.group(2)

            declared_type = f.fields.get(receiver)
            if declared_type is None:
                continue

            candidates = [t for t in by_simple.get(declared_type, [])
                          if t.package == f.package or f.imports.get(declared_type) == t.fqn]
            if len(candidates) != 1:
                continue
            target = candidates[0]

            # Inherited members are invisible here, so a type with a supertype
            # we did not parse is exempt.
            if any(sup not in ours for sup in target.supertypes):
                continue
            if target.kind == "interface":
                continue

            if member not in target.methods:
                report(f.path,
                       f"calls `{receiver}.{member}(...)` on a {target.name}, "
                       f"which {target.fqn} does not declare")

        # `Xxx.member`
        for m in re.finditer(r"(?<![\w.])([A-Z]\w*)\.(\w+)\s*(\()?", f.body):
            qualifier, member, is_call = m.group(1), m.group(2), bool(m.group(3))

            # SCREAMING_SNAKE is a constant, not a type.
            if qualifier.upper() == qualifier and len(qualifier) > 1:
                continue

            if qualifier not in visible and qualifier not in ours:
                # Could be a package fragment or a fully-qualified name we split
                # mid-way; only report when nothing plausible explains it.
                if not any(qualifier in w for w in f.wildcard_packages) and qualifier not in PRIMITIVES:
                    report(f.path, f"uses `{qualifier}.{member}` but {qualifier} is not imported or declared")
                continue

            candidates = [t for t in by_simple.get(qualifier, [])
                          if t.package == f.package or f.imports.get(qualifier) == t.fqn]
            if len(candidates) != 1:
                continue
            target = candidates[0]

            # A type with a supertype we did not parse may inherit the member.
            if any(s not in ours for s in target.supertypes):
                continue

            if member in nested_in.get(qualifier, set()):
                # A nested type, reached as Outer.Nested. Arity is checked by the
                # `new` pass above when it is a constructor.
                continue

            if is_call:
                if member not in target.methods and member not in {"values", "valueOf", "class"}:
                    report(f.path,
                           f"calls `{qualifier}.{member}(...)`, which {target.fqn} does not declare")
            else:
                known = target.constants | set(target.methods) | {"class", "this"}
                if member.isupper() and member not in known:
                    report(f.path,
                           f"reads `{qualifier}.{member}`, which {target.fqn} does not declare")

    for line in findings:
        print(f"  {line}")

    print(f"\n  {len(java_files)} files, {len(by_fqn)} types, "
          f"{len(findings)} finding(s)")
    return 1 if findings else 0


if __name__ == "__main__":
    args = [Path(a) for a in sys.argv[1:]]
    if not args:
        args = sorted(REPO.glob("services/*/src"))
    sys.exit(check(args))
