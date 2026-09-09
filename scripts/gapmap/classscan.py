#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Defect-class heuristics scan ("脚本扫" for the ledger's non-C2 columns).

The ledger's coverage matrix has eight defect classes; C1 / C5 / C7 had grep
scans on 2026-09-06 and C2 has the promise-revert sampler. This script gives
the remaining classes a repeatable first pass so a cell can move from 未审 to
脚本扫: it lists CANDIDATES for a human to judge, it never decides.

  C3 回调外累积状态  package-level mutable state; state mutated from callbacks
  C4 跨包字面量耦合  string literals (config keys, names, prefixes) shared by
                    two or more packages — across repositories when several
                    roots are given
  C5 静默吞错        blank-assigned results (`_ =`, `_, _ =`, `x, _ :=`)
  C6 常量指标        gauges / observations fed a literal; health checks that
                    can only ever answer OK
  C7 释放无 defer    Lock without a following `defer ... Unlock`; Open / Create
                    without a `defer ... Close` nearby
  C8 快慢路径不对称  function pairs that look like two code paths for one
                    decision (X / XBatch, X / XLocked, X / tryX, ...)

Usage:
  classscan.py --classes C3,C4,C6,C8 --report out.md ROOT[:pkg,pkg,...] ...

ROOT is a module root; the optional package list restricts the scan to those
package directories (relative to ROOT). Test files and *_gen.go are skipped.
"""
import argparse
import collections
import os
import re
import sys

LITERAL = re.compile(r'"((?:[^"\\\n]|\\.){3,})"')
FUNC = re.compile(r'^func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)\s*[\[(]', re.M)
PKG_VAR = re.compile(r'^var\s+([A-Za-z_]\w*)\s+(?:=\s*)?(make\(|map\[|\[\]|&|new\(|\*|sync\.Map|atomic\.)', re.M)
CALLBACK = re.compile(r'\.(Range|ForEach|Each|Walk|WalkDir|Visit|OnEach|Subscribe|Watch|Handle|HandleFunc|On[A-Z]\w*)\(\s*func\b')
BLANK = re.compile(r'^\s*(?:_\s*=\s*|_\s*,\s*_\s*:?=\s*|[A-Za-z_]\w*\s*,\s*_\s*:?=\s*)[A-Za-z_][\w.]*\(')
LOCK = re.compile(r'\.(R?Lock)\(\)')
UNLOCK = re.compile(r'defer\s+[\w.]+\.R?Unlock\(\)')
OPEN = re.compile(r'\b(?:os\.(?:Open|Create|OpenFile)|net\.(?:Listen|Dial)\w*|\w+\.Open)\(')
CLOSE = re.compile(r'defer\s+[\w.()]*Close\(\)|\.Close\(\)')
GAUGE_CONST = re.compile(r'\.(Set|SetGauge|Observe|Record\w*|Gauge\w*|Histogram\w*)\(\s*(?:"[^"]*"\s*,\s*)?(?:nil\s*,\s*)?(-?\d+(?:\.\d+)?)\s*\)')
HEALTH_OK = re.compile(r'Status:\s*health\.StatusOK')
HEALTH_BAD = re.compile(r'Status:\s*health\.Status(?:Fail|Degraded|Warn)')
PAIR_SUFFIX = ("Many", "Batch", "All", "Fast", "Slow", "Locked", "Unlocked", "Cached", "Retry", "Once", "Sync", "Async", "Remote", "Local", "Now", "Later", "Direct", "Pipelined", "Strict")
PAIR_PREFIX = ("try", "must", "maybe", "fast", "slow", "do", "run")
NOISE_LITERALS = {"json", "yaml", "bson", "true", "false", "null", "nil", "error", "string", "int64", "int32", "_id"}


def go_files(root, pkgs):
    dirs = [os.path.join(root, p) for p in pkgs] if pkgs else [root]
    for base in dirs:
        for dirpath, dirnames, filenames in os.walk(base):
            dirnames[:] = [d for d in dirnames if not d.startswith(".") and d not in ("vendor", "testdata", "node_modules")]
            if pkgs is None or True:
                pass
            for name in filenames:
                if not name.endswith(".go") or name.endswith("_test.go") or name.endswith("_gen.go"):
                    continue
                path = os.path.join(dirpath, name)
                if pkgs and os.path.relpath(dirpath, root) not in pkgs:
                    continue
                yield path


def package_of(root, path):
    return os.path.relpath(os.path.dirname(path), root)


def scan_c3(files, root):
    hits = []
    for path in files:
        text = open(path, encoding="utf-8", errors="replace").read()
        for m in PKG_VAR.finditer(text):
            line = text.count("\n", 0, m.start()) + 1
            hits.append((path, line, "package-level mutable: " + m.group(0).strip()[:90]))
        for m in CALLBACK.finditer(text):
            line = text.count("\n", 0, m.start()) + 1
            body = text[m.end():m.end() + 600]
            depth, end = 1, 0
            for i, ch in enumerate(body):
                if ch == "{":
                    depth += 1
                elif ch == "}":
                    depth -= 1
                    if depth == 0:
                        end = i
                        break
            snippet = body[:end] if end else body
            if re.search(r'\bappend\(|\+\+|\+=|\[[^\]]+\]\s*=', snippet):
                hits.append((path, line, "callback mutates captured state: ." + m.group(1) + "(func…"))
    return hits


def scan_c4(roots):
    where = collections.defaultdict(set)
    sample = {}
    for root, files in roots:
        for path in files:
            pkg = os.path.basename(root) + "/" + package_of(root, path)
            for m in LITERAL.finditer(open(path, encoding="utf-8", errors="replace").read()):
                lit = m.group(1)
                if "%" in lit or " " in lit or lit in NOISE_LITERALS or lit.startswith(("http", "github.com", "go.")):
                    continue
                if not re.search(r'[._:/-]', lit) and not lit.isupper():
                    continue
                where[lit].add(pkg)
                sample.setdefault((lit, pkg), path)
    shared = [(lit, sorted(pkgs)) for lit, pkgs in where.items() if len(pkgs) >= 2]
    shared.sort(key=lambda item: (-len(item[1]), item[0]))
    return shared


def scan_c5(files):
    hits = []
    for path in files:
        for no, line in enumerate(open(path, encoding="utf-8", errors="replace"), 1):
            if BLANK.match(line):
                hits.append((path, no, line.strip()[:100]))
    return hits


def scan_c6(files):
    hits = []
    for path in files:
        text = open(path, encoding="utf-8", errors="replace").read()
        for m in GAUGE_CONST.finditer(text):
            line = text.count("\n", 0, m.start()) + 1
            hits.append((path, line, "constant fed to metric: " + m.group(0)[:90]))
        ok, bad = len(HEALTH_OK.findall(text)), len(HEALTH_BAD.findall(text))
        if ok and not bad:
            hits.append((path, 0, "health check can only answer OK (%d OK, 0 Fail)" % ok))
    return hits


def scan_c7(files):
    hits = []
    for path in files:
        lines = open(path, encoding="utf-8", errors="replace").read().split("\n")
        for i, line in enumerate(lines):
            if LOCK.search(line) and "defer" not in line:
                window = "\n".join(lines[i + 1:i + 3])
                if not UNLOCK.search(window):
                    hits.append((path, i + 1, "lock without a following defer Unlock: " + line.strip()[:80]))
            if OPEN.search(line) and ":=" in line:
                window = "\n".join(lines[i + 1:i + 8])
                if not CLOSE.search(window):
                    hits.append((path, i + 1, "open/listen without Close nearby: " + line.strip()[:80]))
    return hits


def scan_c8(files, root):
    names = collections.defaultdict(set)
    for path in files:
        pkg = package_of(root, path)
        for m in FUNC.finditer(open(path, encoding="utf-8", errors="replace").read()):
            names[pkg].add(m.group(1))
    pairs = []
    for pkg, funcs in sorted(names.items()):
        lower = {f.lower(): f for f in funcs}
        for f in sorted(funcs):
            for suffix in PAIR_SUFFIX:
                if f.endswith(suffix) and len(f) > len(suffix) and f[:-len(suffix)] in funcs:
                    pairs.append((pkg, f[:-len(suffix)], f))
            for prefix in PAIR_PREFIX:
                if f.lower().startswith(prefix) and len(f) > len(prefix):
                    rest = f[len(prefix):]
                    if rest and rest[0].isupper() and rest.lower() in lower and lower[rest.lower()] != f:
                        pairs.append((pkg, lower[rest.lower()], f))
    return sorted(set(pairs))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("roots", nargs="+", help="ROOT or ROOT:pkg,pkg")
    parser.add_argument("--classes", default="C3,C4,C5,C6,C7,C8")
    parser.add_argument("--report", default="-")
    args = parser.parse_args()
    classes = set(c.strip().upper() for c in args.classes.split(","))
    roots = []
    for spec in args.roots:
        root, _, pkgs = spec.partition(":")
        root = os.path.abspath(root)
        pkg_list = [p for p in pkgs.split(",") if p] or None
        roots.append((root, sorted(go_files(root, pkg_list))))
    out = []
    for root, files in roots:
        label = os.path.basename(root)
        by_pkg = collections.defaultdict(list)
        for path in files:
            by_pkg[package_of(root, path)].append(path)
        out.append("# %s — %d files in %d packages\n" % (label, len(files), len(by_pkg)))
        for cls, fn in (("C3", lambda fs: scan_c3(fs, root)), ("C5", scan_c5), ("C6", scan_c6), ("C7", scan_c7)):
            if cls not in classes:
                continue
            out.append("## %s\n" % cls)
            for pkg, fs in sorted(by_pkg.items()):
                hits = fn(fs)
                out.append("### `%s` — %d\n" % (pkg, len(hits)))
                for path, line, text in hits:
                    out.append("- `%s:%d` %s" % (os.path.relpath(path, root), line, text))
                out.append("")
        if "C8" in classes:
            out.append("## C8 — function pairs to read side by side\n")
            pairs = scan_c8(files, root)
            for pkg, a, b in pairs:
                out.append("- `%s`: `%s` / `%s`" % (pkg, a, b))
            out.append("")
    if "C4" in classes:
        out.append("# C4 — literals shared by two or more packages\n")
        for lit, pkgs in scan_c4(roots):
            out.append("- `%s` ← %s" % (lit, ", ".join(pkgs)))
        out.append("")
    report = "\n".join(out) + "\n"
    if args.report == "-":
        sys.stdout.write(report)
    else:
        with open(args.report, "w", encoding="utf-8") as fh:
            fh.write(report)
        print("wrote %s (%d lines)" % (args.report, report.count("\n")))


if __name__ == "__main__":
    main()
