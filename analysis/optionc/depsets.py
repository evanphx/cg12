#!/usr/bin/env python3
"""Read cmd/depsets output and answer stage 1's two questions.

  dist   PREFIX...                     the dependency-set size distribution
  inval  BASE_PREFIX EDIT_PREFIX       what an edit invalidates, against what it changed

A memo for function f, recorded on the BASE compile, is valid on the EDIT
compile exactly when nothing f's recorded set names has a different input
digest -- where the input is the module after the per-function prefix
(mem2reg + clean), which is the .mid file. What the edit genuinely changed is
the .post file: the output of the stage being memoised.
"""

import sys
import statistics


def read_digests(path):
    out = {}
    with open(path) as handle:
        for line in handle:
            digest, name = line.rstrip("\n").split(" ", 1)
            out[name] = digest
    return out


def read_deps(path):
    names = []
    consulted = {}
    spliced = {}
    nosplit = set()
    costinline = set()
    sites = {}
    live = []
    with open(path) as handle:
        header = handle.readline().split()
        assert header == ["V", "1"], header
        count = int(handle.readline().split()[1])
        for _ in range(count):
            names.append(handle.readline().rstrip("\n"))
        for line in handle:
            parts = line.split()
            tag = parts[0]
            if tag == "C":
                consulted[names[int(parts[1])]] = [names[int(i)] for i in parts[3:]]
            elif tag == "S":
                spliced[names[int(parts[1])]] = [names[int(i)] for i in parts[3:]]
            elif tag == "X":
                nosplit.add(names[int(parts[1])])
            elif tag == "K":
                costinline.add(names[int(parts[1])])
            elif tag == "T":
                sites[names[int(parts[1])]] = int(parts[2])
            elif tag == "L":
                live.append(names[int(parts[1])])
    return {
        "names": names,
        "consulted": consulted,
        "spliced": spliced,
        "nosplit": nosplit,
        "costinline": costinline,
        "sites": sites,
        "live": live,
    }


def percentile(values, fraction):
    if not values:
        return 0
    ordered = sorted(values)
    index = int(round(fraction * (len(ordered) - 1)))
    return ordered[index]


def describe(label, sizes):
    print(
        "  %-28s n=%-6d min=%-5d median=%-5d p95=%-6d p99=%-6d max=%-6d mean=%.1f"
        % (
            label,
            len(sizes),
            min(sizes) if sizes else 0,
            int(statistics.median(sizes)) if sizes else 0,
            percentile(sizes, 0.95),
            percentile(sizes, 0.99),
            max(sizes) if sizes else 0,
            statistics.mean(sizes) if sizes else 0.0,
        )
    )


def dist(prefix):
    deps = read_deps(prefix + ".deps")
    live = deps["live"]
    total = len(deps["names"])
    print("%s: %d functions handed to the whole-module stage, %d survive it" % (prefix, total, len(live)))
    for key in ("consulted", "spliced"):
        sizes = [len(deps[key].get(name, ())) for name in live]
        describe(key + " (surviving funcs)", sizes)
        empty = sum(1 for size in sizes if size == 0)
        print("    %d of %d have an empty %s set (%.1f%%)" % (empty, len(sizes), key, 100.0 * empty / len(sizes)))
        print("    total edges: %d, sum/|live| = %.1f" % (sum(sizes), sum(sizes) / len(sizes)))
    print("  nosplit-measured: %d   costinline-selected: %d" % (len(deps["nosplit"]), len(deps["costinline"])))
    # The reverse direction is what decides an invalidation: how many memos does
    # one changed function knock out?
    reverse = {}
    liveset = set(live)
    for name in live:
        for dep in deps["consulted"].get(name, ()):
            reverse.setdefault(dep, set()).add(name)
    fanout = [len(reverse.get(name, ())) for name in deps["names"]]
    describe("reverse fan-out (per func)", fanout)
    worst = sorted(reverse.items(), key=lambda item: -len(item[1]))[:10]
    print("  the ten functions whose change invalidates the most memos:")
    for name, dependents in worst:
        print("    %-60s %6d" % (name[:60], len(dependents)))


def invalidate(base_prefix, edit_prefix, key="consulted"):
    deps = read_deps(base_prefix + ".deps")
    base_mid = read_digests(base_prefix + ".mid")
    edit_mid = read_digests(edit_prefix + ".mid")
    base_post = read_digests(base_prefix + ".post")
    edit_post = read_digests(edit_prefix + ".post")

    changed_input = {
        name
        for name in set(base_mid) | set(edit_mid)
        if base_mid.get(name) != edit_mid.get(name)
    }
    changed_output = {
        name
        for name in set(base_post) | set(edit_post)
        if base_post.get(name) != edit_post.get(name)
    }

    live = deps["live"]
    invalid = set()
    for name in live:
        if name in changed_input:
            invalid.add(name)
            continue
        for dep in deps[key].get(name, ()):
            if dep in changed_input:
                invalid.add(name)
                break

    print("%s -> %s  (key = %s)" % (base_prefix, edit_prefix, key))
    print("  functions whose INPUT (post-prefix IR) moved:  %d" % len(changed_input))
    for name in sorted(changed_input)[:12]:
        print("      %s" % name)
    if len(changed_input) > 12:
        print("      ... and %d more" % (len(changed_input) - 12))
    print("  functions whose OUTPUT genuinely changed:      %d of %d" % (len(changed_output), len(live)))
    print("  memos the recorded sets INVALIDATE:            %d of %d (%.2f%%)"
          % (len(invalid), len(live), 100.0 * len(invalid) / len(live)))
    if changed_output:
        print("  ratio invalidated / genuinely changed:         %.2fx" % (len(invalid) / len(changed_output)))
    missed = changed_output - invalid
    print("  UNSOUND: changed output the key called valid:  %d" % len(missed))
    for name in sorted(missed)[:10]:
        print("      !! %s" % name)
    hits = len(live) - len(invalid)
    print("  memo hit rate:                                 %.2f%% (%d of %d)"
          % (100.0 * hits / len(live), hits, len(live)))
    return len(invalid), len(changed_output), len(live)


def main():
    if len(sys.argv) < 3:
        print(__doc__)
        sys.exit(2)
    mode = sys.argv[1]
    if mode == "dist":
        for prefix in sys.argv[2:]:
            dist(prefix)
            print()
    elif mode == "inval":
        invalidate(sys.argv[2], sys.argv[3], "consulted")
        print()
        invalidate(sys.argv[2], sys.argv[3], "spliced")
    else:
        print(__doc__)
        sys.exit(2)


if __name__ == "__main__":
    main()
