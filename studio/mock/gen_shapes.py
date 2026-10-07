#!/usr/bin/env python3
"""Generates shapes_gen.go: for every operation in the three Supabase API type files (the same
OpenAPI specs Studio is built against), the success status, whether the response is an array,
an object or empty, and the required top-level fields. The mock answers unknown routes from this
table and its tests check the hand-written P0 handlers against the Required lists.

    python3 studio/mock/gen_shapes.py <path to packages/api-types/types> > studio/mock/shapes_gen.go
    gofmt -w studio/mock/shapes_gen.go

The input is read-only; nothing is downloaded. Reads the `paths`, `operations` and
`components.schemas` sections of platform.d.ts, api-v1.d.ts and api-v2.d.ts (openapi-typescript
output, one declaration per line, so a line-based reader is enough).
"""
import os
import re
import sys

FILES = ("platform.d.ts", "api-v1.d.ts", "api-v2.d.ts")


def block_end(lines, start, indent):
    close = " " * indent + "}"
    i = start + 1
    while i < len(lines) and lines[i].rstrip() != close:
        i += 1
    return i


def parse(path):
    lines = open(path, encoding="utf8").read().split("\n")
    i_paths = lines.index("export interface paths {")
    i_ops = lines.index("export interface operations {")
    i_comp = next(i for i, l in enumerate(lines) if l.startswith("export interface components {"))

    paths = {}
    i = i_paths + 1
    while i < min(i_ops, i_comp):
        m = re.match(r"^  '(/[^']*)': \{$", lines[i])
        if m:
            end = block_end(lines, i, 2)
            for l in lines[i:end]:
                mm = re.match(r"^    (get|put|post|delete|patch|head): operations\['([^']+)'\]", l)
                if mm:
                    paths.setdefault(m.group(1), {})[mm.group(1).upper()] = mm.group(2)
            i = end
        i += 1

    ops = {}
    i = i_ops + 1
    while i < len(lines):
        m = re.match(r"^  '?([\w.\-]+)'?: \{$", lines[i])
        if m:
            end = block_end(lines, i, 2)
            txt = "\n".join(lines[i:end])
            resp = None
            for rm in re.finditer(r"^      (\d{3}): \{\n(.*?)^      \}", txt, re.S | re.M):
                if rm.group(1).startswith("2"):
                    c = re.search(r"'application/json': (.+)$", rm.group(2), re.M)
                    resp = (int(rm.group(1)), c.group(1).strip() if c else None)
                    break
            ops[m.group(1)] = resp
            i = end
        i += 1

    schemas = {}
    cs = next(i for i, l in enumerate(lines) if i > i_comp and l == "  schemas: {")
    ce = block_end(lines, cs, 2)
    i = cs + 1
    while i < ce:
        m = re.match(r"^    (\w+): (.*)$", lines[i])
        if m:
            if m.group(2).endswith("{"):
                end = block_end(lines, i, 4)
                req = []
                for k in range(i + 1, end):
                    pm = re.match(r"^      (\S+?)(\?)?:(?: (.*))?$", lines[k])
                    if pm and not pm.group(2) and not pm.group(1).startswith("["):
                        # the property's text: this line plus the lines up to the next property
                        j = k + 1
                        while j < end and not re.match(r"^      [\w'\[]", lines[j]):
                            j += 1
                        block = [(pm.group(3) or "")] + [l.strip() for l in lines[k + 1 : j] if not l.strip().startswith(("/**", "*", "//"))]
                        req.append((pm.group(1).strip("'"), zero_kind(block)))
                schemas[m.group(1)] = ("object", req)
                i = end
            else:
                schemas[m.group(1)] = ("alias", m.group(2))
        i += 1
    return paths, ops, schemas


def zero_kind(block):
    """One letter for the neutral value of a field type, from the lines of its declaration:
    n null, s string, i number, b boolean, a array, o object, or the first string literal of an
    enum (prefixed with a quote)."""
    first = block[0].strip()
    last = block[-1].strip()
    text = " ".join(block)
    if len(block) > 1 and (last.endswith("[]") and (first.startswith(("{", "(")) or first == "")):
        return "a"
    if first.startswith("{"):
        return "o"
    if first.endswith("[]") and not first.startswith("|"):
        return "a"
    if first.startswith("string"):
        return "s"
    if first.startswith("number"):
        return "i"
    if first.startswith("boolean"):
        return "b"
    if first.startswith("null"):
        return "n"
    m = re.match(r"'([^']*)'", first)
    if not m and first == "" and len(block) > 1:
        m = re.match(r"\|\s*'([^']*)'", block[1])
        if not m and re.match(r"\|\s*\{", block[1]):
            return "o"
    if m:
        return "'" + m.group(1)
    if first.startswith("components["):
        return "a" if first.endswith("[]") else "o"
    if "null" in text.split("|")[-1]:
        return "n"
    return "n"


def classify(expr, schemas, depth=0):
    """-> (kind, required) with kind in array|object|empty"""
    if expr is None:
        return "empty", []
    m = re.match(r"components\['schemas'\]\['(\w+)'\](\[\])?", expr)
    if not m:
        return ("object", []) if expr.startswith("{") else ("object", [])
    name, arr = m.group(1), bool(m.group(2))
    s = schemas.get(name)
    if s and s[0] == "alias" and depth < 4:
        kind, req = classify(s[1], schemas, depth + 1)
        return ("array" if arr else kind), req
    req = s[1] if s and s[0] == "object" else []
    return ("array" if arr else "object"), req


def main():
    d = sys.argv[1]
    rows = {}
    for f in FILES:
        paths, ops, schemas = parse(os.path.join(d, f))
        for p, ms in paths.items():
            for method, op in ms.items():
                status, expr = ops.get(op) or (200, None)
                kind, req = classify(expr, schemas)
                rows[(method, p)] = (status, kind, req)
    out = []
    out.append("// Code generated by gen_shapes.py from packages/api-types/types/*.d.ts; DO NOT EDIT.\n")
    out.append("package main\n")
    out.append("// shapes maps \"METHOD /path/template\" to the success status and response shape the")
    out.append("// Supabase OpenAPI specs document for it.")
    out.append("var shapes = map[string]shape{")
    for (method, p) in sorted(rows, key=lambda k: (k[1], k[0])):
        status, kind, req = rows[(method, p)]
        reqs = ", ".join('{"%s", "%s"}' % (r[0], r[1]) for r in req)
        out.append('\t"%s %s": {Status: %d, Kind: "%s", Required: []field{%s}},' % (method, p, status, kind, reqs))
    out.append("}\n")
    sys.stdout.write("\n".join(out))


if __name__ == "__main__":
    main()
