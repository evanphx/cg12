# VERDICT: PENDING — verification of `ccwork/batch-reconcile` in progress

This file is written as results land, not at the end. The verdict line above is `PENDING`
until every check below has run and been read; if this job is cut short while it still says
`PENDING`, treat that as **FAIL** — the checks that were not completed are listed under
"Not yet run".

Branch: `ccwork/verify-batch-reconcile`, off `ccwork/batch-reconcile` (`76030b4`).
That branch's own report is preserved verbatim at `docs/report-batch-reconcile.md`, following
the precedent those jobs set. No source change is proposed by this job unless a check finds a
defect.

## What is being verified

`ccwork/batch-reconcile` reconciles `goc compile-batch` with §19's multi-pack selection by
introducing a `packSet` that reads every candidate pack's manifest up front and each pack's
objects lazily, retaining them for the life of the process. Its own report left its central
safety property unverified: **a program compiled in a process that has already compiled other
programs must produce the same executable as a program compiled alone.**

`git diff --name-only main...HEAD` over `goc/ ir/ opt/ arm64/ amd64/ link/ obj/ lower/ parse/
internal/` is empty — confirmed again here. Only `cmd/goc/` and `analysis/batchdiff/` change.
So no compile *can* emit different code for a reason this branch introduced; what can differ is
when state is read and what a worker carries between programs.

## Environment

- linux/arm64, 64 cores, ~240 GB RAM, gcc 13.3, Go 1.26.1.
- `CCWORK_CPU_SLOTS=8`; this job uses 8 compile workers unless a step says otherwise.
- The box is **shared**. Load average at the start of this job was 8.28 / 11.42 / 10.55, so
  another job was resident throughout. Every wall-clock number below is reported with the load
  it was taken under, and the A/B is run back-to-back so both arms see the same neighbours.
- Scratch under `$TMPDIR` on `/dev/md0` (659 GB free), not tmpfs.

## The pack set, and which program picks which pack

The matrix's seven roots (`runtimeCapabilityPackRoots`) were built here and their manifests
read directly:

| pack | root | closure |
| --- | --- | ---: |
| 0 | (runtime only) | 29 |
| 1 | `net/http` | 181 |
| 2 | `net/smtp` | 161 |
| 3 | `crypto/x509` | 140 |
| 4 | `crypto/ecdsa` | 113 |
| 6 | `crypto/hpke` | 110 |
| 5 | `crypto/ecdh` | 93 |

`chooseManifest` takes the usable candidate with the largest closure. Usability was measured,
not guessed: every corpus program that imports anything under `net/` or `crypto/` (28 of them)
was compiled against each rich pack offered alone, and the pack was recorded as usable exactly
when the compile succeeded rather than failing with "none of the N prebuilt runtimes offered is
usable by this program". No probe failed for any other reason.

The resulting selection over the 358-program corpus:

| pack | programs |
| --- | --- |
| 1 (`net/http`) | `stdlib_http_parse_roundtrip`, `stdlib_http_cookiejar`, `stdlib_http_multipart_form`, `stdlib_http_client_server`, `stdlib_http_redirect_keepalive`, `stdlib_http_tls_client_server` |
| 2 (`net/smtp`) | `stdlib_smtp_session` |
| 3 (`crypto/x509`) | `stdlib_crypto_x509_ed25519` |
| 4 (`crypto/ecdsa`) | `stdlib_crypto_ecdsa` |
| 6 (`crypto/hpke`) | `stdlib_crypto_hpke` |
| 5 (`crypto/ecdh`) | `stdlib_crypto_ecdh_x25519` |
| 0 (fallback) | the other 347 |

All seven packs are therefore live, and the interleaved-batch case the briefing asks for is
constructible from this corpus.

## Results

### 1a. Corpus-wide leak check — 358 programs, three ways, seven packs

    batchdiff -goc goc -runtime <all seven packs> -j 7 <358 programs, rich programs interleaved>

The program order was constructed rather than taken from the glob: alphabetically the eleven
programs that select a rich pack sit next to each other, so a shared queue would hand them out
at the same moment and each worker would meet at most one. They were spread evenly through the
347 fallback programs instead, cycling `net/http`, `crypto/x509`, `net/http`, `crypto/ecdsa`,
`net/http`, `crypto/hpke`, `net/http`, `crypto/ecdh`, `net/http`, `net/smtp`, `net/http`.

    one-shot         358 programs in 217.2s, 0 failed
    batch            358 programs in 183.6s, 0 failed
    batch-reversed   358 programs in 170.5s, 0 failed
    summed per-program compile wall: one-shot=1499.8s batch=1178.5s (321.3s saved, 21.4%)

    identical=325 differing=33

**No program built one way and failed another** — `0 failed` in all three passes, and no
`BUILD DISAGREES` line. 325 of 358 are byte-identical alone, in a batch, and in a reversed
batch.

The 33 that differ are being resolved individually below; the plan already states
(§5.10, "Compiling the same program twice does not give the same binary") that **39 of the 358
corpus programs vary at all** across repeated one-shot compiles, so a set of 33 is the
expected size for the pre-existing front-end nondeterminism rather than for a leak. The
pattern in the digests already points that way — several programs have `alone == reversed`
but `batch` different, and three have `alone == batch` but `reversed` different, which is what
three independent draws from a nondeterministic compiler look like and not what a
batch-versus-alone leak looks like. That is an argument, not a proof, so each of the 33 was
recompiled alone five times to show the same variation without batch mode at all.

### 1b. Every one of the 33 differs without batch mode too

Each of the 33 was compiled five more times by a plain one-shot `goc` — the identical command
line the `.alone` pass used, `goc -runtime <seven packs> -o out prog.go` — and the distinct
executables counted. **29 of the 33 produced between 2 and 5 distinct executables in 5
one-shot compiles**, which settles them: they are nondeterministic with no batch process
anywhere in the picture.

| distinct executables in 5 solo compiles | programs |
| ---: | ---: |
| 5 (the sampler saturates) | 17 |
| 4 | 5 |
| 3 | 4 |
| 2 | 3 |
| 1 | 4 |

Seventeen programs gave a different image on every one of five compiles, so those counts are
lower bounds on the entropy rather than measurements of it.

The four that looked stable in five compiles were given 20 to 60 more, all written to a single
fixed output path so nothing about the destination could be the variable. **All four are
nondeterministic too, and in each case a plain one-shot `goc` reproduces the exact digest the
batch produced:**

| program | solo compiles | distinct images | digests seen | corpus run |
| --- | ---: | ---: | --- | --- |
| `stdlib_encoding_json_roundtrip.go` | 25 | 2 | `b3b06ea8`, `0cc28f52` | alone=`b3b06ea8` batch=rev=`0cc28f52` |
| `stdlib_net_mail_textproto.go` | 53 | 2 | `e7353aad` (3×), `7f118fb9` (50×) | alone=`e7353aad` batch=rev=`7f118fb9` |
| `stdlib_runtime_trace_start_probe.go` | 25 | 2 | `727b0ea9`, `455da42f` | alone=batch=`727b0ea9` rev=`455da42f` |
| `stdlib_testing_quick.go` | 25 | 5 | incl. `41e747d9`, `1e9c3aa8`, `e3a72d74` | alone=`41e747d9` batch=`1e9c3aa8` rev=`e3a72d74` |

Every digest the three passes produced for these four is reproducible by a one-shot compile,
including all three of `stdlib_testing_quick.go`'s. `stdlib_net_mail_textproto.go` is the
skewed one — 3 of 53 compiles take the other branch — which is why five repeats had missed it.

The output path is not what varies either: `.alone`, `.batch` and `.reversed` are three
different output paths for every one of the 358 programs, and 325 of them are byte-identical
across all three.

None of this is new. §5.10 already records that goc's output is not reproducible, that the
remaining causes are in the front end, and that **39 of the 358 corpus programs vary at all**.
The 33 found here are a subset of that population, and the whole set of 39 is unrelated to
this branch: no file that decides what a compile emits differs from `main`.

### 1c. Behaviour — 358/358 identical

The bytes cannot speak for a program whose compile is already nondeterministic, so all three
builds of all 358 programs were run and their exit status and combined output compared. The
binaries from the run above were reused, so this is the same three builds, not a fourth:

    SAME 358    DIFFERS 0

Every corpus program behaves identically whether it was compiled alone, in a batch, or in a
reversed batch — including all 33 whose bytes differ.

### 1d. What actually differs in the 33 is addresses, not content

Bytes are a blunt instrument against a compiler whose function layout is not reproducible, so
the 33 were compared structurally as well: `nm --defined-only -S`, reduced to (type, size,
name) and sorted, plus the file size.

    33 of 33: symbol set identical alone/batch, identical alone/reversed, file size identical

Same symbols, same sizes, same number of them (36148 for `stdlib_smtp_session.go`, 25846 for
`stdlib_crypto_ecdsa.go`, 22866 for `bytes_replace_allocs.go`), same total image size — only
the addresses move. That is exactly the residue §5.10 names: "441 interface-call wrapper
functions land in the module in a different order on each compile. Same functions, same code,
different addresses."

### 1e. The new case — one worker, seven packs, interleaved

The corpus run's shared queue cannot guarantee that any single worker meets programs choosing
*different* packs: only eleven of 358 programs choose a rich pack. So a second run pinned the
grouping. **One** batch worker (`-j 1`), 33 programs, ordered so that a rich-pack program sits
between fallback programs throughout:

    plain, net/http, plain, plain, crypto/x509, plain, plain, net/http, plain, plain,
    crypto/ecdsa, plain, plain, net/http, plain, plain, crypto/hpke, plain, plain, net/http,
    plain, plain, crypto/ecdh, plain, plain, net/http, plain, plain, net/smtp, plain, plain,
    net/http, plain

That single process therefore read all seven packs, each on the first program that selected
it, and kept them while compiling programs that selected the others. Result:

    one-shot         33 programs in 252.0s, 0 failed
    batch            33 programs in 218.7s, 0 failed
    batch-reversed   33 programs in 213.9s, 0 failed
    identical=23 differing=10
    behaviour: identical=33 differing=0

`batchdiff`'s own triage called 3 of the 10 leaks — `bytes_replace_allocs.go`,
`stdlib_crypto_ecdsa.go`, `stdlib_smtp_session.go` — but its triage recompiles a differing
program alone exactly **once** and calls it a leak if that one repeat matches. Against a
compiler that yields up to five distinct images in five compiles, one repeat decides nothing.
All three were resolved by repeating properly, and in each case a plain one-shot `goc`
reproduces the digest that had been attributed to the batch:

| program | digest `batchdiff` called batch-only | reproduced by a one-shot compile | distinct images in 12 solo compiles |
| --- | --- | --- | ---: |
| `bytes_replace_allocs.go` | `1da4fe4787d7` (and 4 others seen) | yes — 10 distinct values | 10 |
| `stdlib_crypto_ecdsa.go` | `f71870049f71` | yes | 2 |
| `stdlib_smtp_session.go` | `0954b405b6a7` | yes | 2 |

`stdlib_crypto_ecdsa.go` and `stdlib_smtp_session.go` were byte-identical all three ways in
the 358-program run and differed only here, which is itself the behaviour of a coin flip
rather than of a leak. All 33 curated programs behave identically across the three builds.

**There is no leak.** No program's bytes differ between a batch and a solitary compile for any
reason that a solitary compile does not reproduce on its own, no program's symbol content
differs, and no program behaves differently.

### 2. `scripts/determinism-check.sh`

With the seven packs:

    hello.go                            round1:identical(942b6223782f883a)  round2:identical(942b6223782f883a)
    fmt_sprintf.go                      round1:identical(18bb962e04c87aee)  round2:identical(18bb962e04c87aee)
    gc_struct.go                        round1:identical(78f936781428f778)  round2:identical(78f936781428f778)
    runtime_cleanup_frame_retention.go  round1:identical(1223fe641a7fe742)  round2:identical(1223fe641a7fe742)
    runtime_defer_capture_allocs.go     round1:DIFFERENT  round2:DIFFERENT

With no pack at all (the monolithic compile path):

    hello.go                            round1:identical(cf3c0fbdf176bf8f)  round2:identical(cf3c0fbdf176bf8f)
    fmt_sprintf.go                      round1:identical(1a2ec8b6bd5d8fc3)  round2:identical(1a2ec8b6bd5d8fc3)
    gc_struct.go                        round1:identical(7a91825db343e217)  round2:identical(7a91825db343e217)
    runtime_cleanup_frame_retention.go  round1:identical(c09f3902a7c430dd)  round2:identical(c09f3902a7c430dd)
    runtime_defer_capture_allocs.go     round1:DIFFERENT  round2:DIFFERENT

4 of 5 byte-identical cold (`CG12_NOCACHE=1`) against warm on both compile paths, in both
rounds, and stable across rounds; `runtime_defer_capture_allocs.go` is the known §5.10
front-end residue and is the only program that differs, on either path. **PASS**, and it
matches what §19's own report recorded for this check.

### 3. The test suites

`go build ./...` and `go vet ./...` clean. `make test-unit`: **PASS**, every package `ok`
(`arm64`, `arm64/a64`, `bpf`, `cmd/cc`, `cmd/cg12`, `internal/backendtest`, `internal/gometa`,
`internal/runtimepack`, `internal/testenv`, `interp`, `ir`, `lift`, `link`, `lower`, `obj`,
`opt`, `parse`, `pe`, `plan9asm`, `plan9asm/sem`, `wasm`, and the `analysis`/`cc` packages).

## Not yet run

1. Leak check — corpus-wide, and the curated interleaved single-worker case.
2. `scripts/determinism-check.sh`, with the seven packs and with no pack.
3. `make test-unit`, `make test-goc-cmd`, `make test-goc-corpus`.
4. The full 338-capability matrix with a subtest census.
5. The monolithic batch path and `make test-goc-coverage`.
6. The matrix A/B against `main`.
