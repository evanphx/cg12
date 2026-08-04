#!/usr/bin/env python3
"""Turns sweep.py's rows into the two numbers the decision rests on: how much a
case's cost moves when only the code's address moves, and what each policy costs
on average once that movement is averaged out.

    python3 analyze.py [results.tsv]

Each case is reported two ways.

  - index: the case's nanoseconds divided by control/spin-fixed-work's, measured
    in the same process a moment earlier. This is the crypto benchmark's
    statistic and it is the primary one here for the same reason: it divides out
    whatever else the machine was doing. Its cost is that the control is itself
    a function that moves when the shift moves, so a little of the case's spread
    is really the control's.
  - raw: elapsed nanoseconds. Free of that circularity and exposed to the box's
    load instead.

The controls' own raw spread is printed as the floor: nothing smaller than it is
a measurement of anything.

Minimum across reps throughout: elapsed time's noise is one-sided.
"""

import collections
import math
import os
import sys

POLICIES = ['none', 'a16', 'a32', 'a64', 'loop32', 'head32']
PROGRAMS = ['p256', 'sha', 'interp', 'regexp', 'json', 'sortmap', 'flate', 'text']
CONTROL = 'control/spin-fixed-work'


def load(path):
    """best[(program, policy, pad, case)] = fastest nanoseconds seen."""
    best = {}
    for line in open(path):
        fields = line.rstrip('\n').split('\t')
        if len(fields) != 6:
            continue
        program, policy, pad, case, _, nanos = fields
        key = (program, policy, int(pad), case)
        value = int(nanos)
        if key not in best or value < best[key]:
            best[key] = value
    return best


def geomean(values):
    return math.exp(sum(math.log(v) for v in values) / len(values))


def spread(values):
    return 100.0 * (max(values) - min(values)) / min(values)


def summarize(name, per_case):
    values = sorted(per_case)
    if not values:
        return
    p90 = values[min(len(values) - 1, int(0.9 * len(values)))]
    print('%-9s %8.2f%% %8.2f%% %8.2f%% %8.2f%%' % (
        name, values[len(values) // 2], sum(values) / len(values), p90, values[-1]))


def main():
    path = sys.argv[1] if len(sys.argv) > 1 else os.path.join(
        os.environ.get('TMPDIR', '/tmp'), 'sweep', 'results.tsv')
    best = load(path)
    pads = sorted({pad for (_, _, pad, _) in best})

    cases, controls = [], []
    for program in PROGRAMS:
        for case in sorted({c for (p, _, _, c) in best if p == program}):
            (controls if case == CONTROL else cases).append((program, case))

    def series(program, policy, case, indexed):
        """The case's cost at each shift, or None where it was not measured."""
        out = []
        for pad in pads:
            value = best.get((program, policy, pad, case))
            base = best.get((program, policy, pad, CONTROL))
            if value is None or (indexed and not base):
                continue
            out.append(value / base if indexed else float(value))
        return out

    for indexed in (True, False):
        label = 'index (case / control, same process)' if indexed else 'raw nanoseconds'
        print()
        print('# Placement-induced spread, %s: (max-min)/min across shifts %s' % (label, pads))
        print('%-9s %-24s%s' % ('program', 'case', ''.join('%9s' % p for p in POLICIES)))
        collected = collections.defaultdict(list)
        for program, case in cases + controls:
            row = []
            for policy in POLICIES:
                values = series(program, policy, case, indexed)
                if len(values) < 2:
                    row.append('       --')
                    continue
                value = spread(values)
                if (program, case) in cases:
                    collected[policy].append(value)
                else:
                    collected[policy + ' control'].append(value)
                row.append('%8.2f%%' % value)
            print('%-9s %-24s%s%s' % (program, case, ''.join(row),
                                      '  (control)' if case == CONTROL else ''))
        print()
        print('# Spread summary over the %d timed cases, %s' % (len(cases), label))
        print('%-9s %9s %9s %9s %9s' % ('policy', 'median', 'mean', 'p90', 'worst'))
        for policy in POLICIES:
            summarize(policy, collected[policy])
        floor = sum((collected[p + ' control'] for p in POLICIES), [])
        summarize('CONTROLS', floor)
        print('           ^ the controls under every policy: the measurement floor')

    # Mean cost, with placement luck averaged out of both sides.
    print()
    print('# Mean cost per case, geometric mean of the index over the %d shifts, vs none'
          % len(pads))
    print('%-9s %-24s %9s%s' % ('program', 'case', 'none', ''.join('%10s' % p for p in POLICIES[1:])))
    ratios = collections.defaultdict(list)
    for program, case in cases:
        base = series(program, 'none', case, True)
        if not base:
            continue
        baseline = geomean(base)
        row = []
        for policy in POLICIES[1:]:
            values = series(program, policy, case, True)
            if not values:
                row.append('        --')
                continue
            ratio = geomean(values) / baseline
            ratios[policy].append(ratio)
            row.append('%+9.2f%%' % (100 * (ratio - 1)))
        print('%-9s %-24s %9.3f%s' % (program, case, baseline, ''.join(row)))

    print()
    print('# Mean over the %d timed cases (geometric), vs none' % len(cases))
    for policy in POLICIES[1:]:
        if ratios[policy]:
            better = sum(1 for r in ratios[policy] if r < 1)
            print('%-9s %+7.2f%%   faster on %d of %d cases, slower on %d' % (
                policy, 100 * (geomean(ratios[policy]) - 1), better,
                len(ratios[policy]), len(ratios[policy]) - better))

    # The square wave itself.
    print()
    print('# Index by shift under `none`, as a %% of the shift-0 value')
    print('%-9s %-24s%s' % ('program', 'case', ''.join('%8d' % p for p in pads)))
    for program, case in cases + controls:
        values = []
        for pad in pads:
            v = best.get((program, 'none', pad, case))
            c = best.get((program, 'none', pad, CONTROL))
            values.append(v / c if v and c else None)
        if not values[0]:
            continue
        print('%-9s %-24s%s' % (program, case, ''.join(
            '%7.1f%%' % (100 * (v / values[0] - 1)) if v else '      --' for v in values)))

    total = len(cases + controls) * len(POLICIES) * len(pads)
    have = sum(1 for (pr, ca) in cases + controls for po in POLICIES for pa in pads
               if (pr, po, pa, ca) in best)
    print()
    print('# %d of %d (program, policy, shift, case) cells measured' % (have, total))


if __name__ == '__main__':
    main()
