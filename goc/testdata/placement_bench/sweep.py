#!/usr/bin/env python3
"""Builds the placement corpus under every placement policy at every text shift,
runs it, and writes one row per (program, policy, shift, case, rep).

    python3 sweep.py build   # compile the grid
    python3 sweep.py run N   # time it, N reps, appending to results.tsv
    python3 sweep.py size    # binary size per policy
    python3 sweep.py buildtime

The shift (GOC_TEXT_PAD) stands in for a commit that changes the size of some
cold code upstream of what is being measured: it moves every function and
changes not one instruction. Sweeping it under each policy is what turns "does
alignment help?" into a measurement.
"""

import os
import random
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.abspath(os.path.join(HERE, '..', '..', '..'))
WORK = os.path.join(os.environ.get('TMPDIR', '/tmp'), 'sweep')
GOC = os.path.join(os.environ.get('TMPDIR', '/tmp'), 'goc')
CORE = os.environ.get('BENCH_CORE', '50')

PROGRAMS = ['p256', 'sha', 'interp', 'regexp', 'json', 'sortmap', 'flate', 'text']

# name -> environment for the backend's placement policy.
POLICIES = {
    'none':   {},
    'a16':    {'GOC_FUNC_ALIGN': '16'},
    'a32':    {'GOC_FUNC_ALIGN': '32'},
    'a64':    {'GOC_FUNC_ALIGN': '64'},
    'loop32': {'GOC_FUNC_ALIGN': '32', 'GOC_ALIGN_LOOP_FUNCS_ONLY': '1'},
    'head32': {'GOC_FUNC_ALIGN': '32', 'GOC_ALIGN_LOOP_FUNCS_ONLY': '1',
               'GOC_LOOP_ALIGN': '32'},
}

PADS = [0, 4, 8, 12, 16, 20, 24, 28]

PARALLEL = 14


def binary(program, policy, pad):
    return os.path.join(WORK, '%s.%s.%d' % (program, policy, pad))


def environment(policy, pad):
    env = dict(os.environ)
    for key in ('GOC_FUNC_ALIGN', 'GOC_LOOP_ALIGN', 'GOC_ALIGN_LOOP_FUNCS_ONLY', 'GOC_TEXT_PAD'):
        env.pop(key, None)
    env.update(POLICIES[policy])
    env['GOC_TEXT_PAD'] = str(pad)
    return env


def grid():
    for program in PROGRAMS:
        for policy in POLICIES:
            for pad in PADS:
                yield program, policy, pad


def build():
    os.makedirs(WORK, exist_ok=True)
    jobs = []
    todo = [g for g in grid() if not os.path.exists(binary(*g))]
    print('%d builds' % len(todo), flush=True)
    done = 0
    for program, policy, pad in todo:
        source = os.path.join(HERE, program, 'main.go')
        jobs.append((subprocess.Popen(
            [GOC, '-O', '-o', binary(program, policy, pad), source],
            env=environment(policy, pad),
            stdout=subprocess.DEVNULL, stderr=subprocess.PIPE),
            program, policy, pad))
        while len(jobs) >= PARALLEL:
            done += drain(jobs, 1)
            print('  built %d/%d' % (done, len(todo)), flush=True)
    while jobs:
        done += drain(jobs, 1)
    print('  built %d/%d' % (done, len(todo)), flush=True)


def drain(jobs, atleast):
    finished = 0
    while finished < atleast:
        for i, (process, program, policy, pad) in enumerate(jobs):
            if process.poll() is not None:
                if process.returncode != 0:
                    sys.exit('build failed: %s %s %d\n%s' % (
                        program, policy, pad, process.stderr.read().decode()))
                jobs.pop(i)
                finished += 1
                break
        else:
            time.sleep(0.2)
    return finished


def run(reps):
    results = open(os.path.join(WORK, 'results.tsv'), 'a')
    order = list(grid())
    for rep in range(reps):
        random.Random(rep).shuffle(order)
        started = time.time()
        for n, (program, policy, pad) in enumerate(order):
            out = subprocess.run(['taskset', '-c', CORE, binary(program, policy, pad)],
                                 capture_output=True)
            # A run that dies is recorded for the cases it had already printed and
            # noted, not treated as a failure of the sweep. goc miscompiles
            # something on compress/flate's collector path -- see the report -- at
            # the same rate with and without alignment, so it is noise here rather
            # than a result, but silently dropping it would hide it.
            if out.returncode != 0 or b'runtime:' in out.stderr:
                print('  died: %s %s %d' % (program, policy, pad), flush=True)
            for line in out.stdout.decode().splitlines():
                if '\t' not in line or line.startswith('#'):
                    continue
                case, nanos = line.split('\t')
                results.write('%s\t%s\t%d\t%s\t%d\t%s\n' % (
                    program, policy, pad, case, rep, nanos))
            results.flush()
            if n % 48 == 47:
                print('  rep %d: %d/%d, %.0f s elapsed' % (
                    rep, n + 1, len(order), time.time() - started), flush=True)
        print('rep %d done in %.0f s' % (rep, time.time() - started), flush=True)


def size():
    for policy in POLICIES:
        for program in PROGRAMS:
            path = binary(program, policy, 0)
            print('%s\t%s\t%d' % (policy, program, os.path.getsize(path)))


def buildtime():
    """Wall-clock compile time per policy, one program, serially, best of three."""
    for policy in POLICIES:
        for program in ('p256', 'json'):
            best = None
            for _ in range(3):
                started = time.time()
                subprocess.run([GOC, '-O', '-o', os.path.join(WORK, 'timing.out'),
                                os.path.join(HERE, program, 'main.go')],
                               env=environment(policy, 0), check=True,
                               stdout=subprocess.DEVNULL)
                elapsed = time.time() - started
                best = elapsed if best is None else min(best, elapsed)
            print('%s\t%s\t%.2f' % (policy, program, best), flush=True)


if __name__ == '__main__':
    command = sys.argv[1] if len(sys.argv) > 1 else 'build'
    if command == 'build':
        build()
    elif command == 'run':
        run(int(sys.argv[2]) if len(sys.argv) > 2 else 1)
    elif command == 'size':
        size()
    elif command == 'buildtime':
        buildtime()
    else:
        sys.exit('unknown command %s' % command)
