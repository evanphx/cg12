# The placement benchmark corpus

Eight self-contained programs, each compiled by goc and run on its own, used to
answer one question: **how much does the number a benchmark reports depend on
where its code happened to land in `.text`?**

Every program follows the crypto signing benchmark's method
(`goc/testdata/crypto_signing_bench/main.go`), which is where it is written down
in full:

  - each case is a closure called through a func value, so the compiler cannot
    see through the call and delete the work;
  - setup is outside the timer;
  - `measure` warms up once and keeps the fastest of `rounds` timed rounds,
    because noise can only make a round slower;
  - the first case every program prints is `control/spin-fixed-work`, a fixed
    amount of integer arithmetic, and every other case is divided by it. That
    ratio -- the index -- is what is compared, so the machine's speed and its
    load divide out.

The workloads are deliberately different from one another in what they stress:

| program  | what it is                                            |
|----------|-------------------------------------------------------|
| `p256`   | ECDSA P-256 sign+verify: `bigmod` limb arithmetic      |
| `sha`    | SHA-256 and HMAC over a buffer: one tight block loop   |
| `interp` | a bytecode interpreter: a switch dispatch loop         |
| `regexp` | `regexp` matching: a second, larger interpreter        |
| `json`   | `encoding/json` round trip: reflection and interfaces  |
| `sortmap`| `sort.Slice` and map build/lookup: callbacks, hashing  |
| `flate`  | `compress/flate` round trip: table-driven loops        |
| `text`   | `strconv`/`fmt`/`strings.Builder`: string formatting   |

## Running the sweep

`GOC_TEXT_PAD=K` puts K bytes of no-ops in front of the first function, which
shifts the whole module's code by K and changes not one instruction. It stands in
for what a real commit does when it changes the size of some cold code upstream
of the thing being measured. Building each program at several values of K and
timing the results measures the spread a benchmark's number has for reasons that
have nothing to do with the code it is benchmarking.

`GOC_FUNC_ALIGN`, `GOC_ALIGN_LOOP_FUNCS_ONLY` and `GOC_LOOP_ALIGN` select the
placement policy to measure that spread under. See `arm64/align.go`.

## The measurements

`results_shift_phase.tsv` is the grid at shifts 0, 4, ... 28, which move a
function's address *and* its phase inside the 32-byte fetch granule.
`results_shift_address.tsv` is the control: shifts 32, 64, 96, 128, which are all
0 mod 32 and so move only the address. Both are three reps of every cell, one run
of every binary per rep in a shuffled order, pinned to one core. Columns are
program, policy, shift, case, rep, nanoseconds.

`analysis_shift_phase.txt` and `analysis_shift_address.txt` are `analyze.py`'s
output on each. The conclusion drawn from them is in CCWORK_REPORT.md, "Should
goc align function entries".

Regenerating them needs the environment knobs in `arm64/align.go` and about an
hour on a quiet machine:

    go build -o "$TMPDIR/goc" ./cmd/goc
    python3 sweep.py build && python3 sweep.py run 3
    python3 analyze.py
