# `make bench-crypto` as a check that only fires on real regressions

Branch: `ccwork/bench-crypto-noise`, off `main` = `6034f73`.

Earlier reports on this file are superseded, not deleted — the wave-7 gate report
is `git show 6034f73:CCWORK_REPORT.md`.

## The problem, as it was handed over

`make bench-crypto` failed **3 of 7** runs on `integration/wave7-fix`, a tree
proved to emit byte-identical code (848/848 corpus binaries identical to the gate
tree, the crypto benchmark program among them). The three failures were on
**different cases in opposite directions**:

| run | case | movement | tolerance |
|---|---|---|---|
| 1 | `p256/verify` | −4.3 % | 4.00 % |
| 3 | `p256/verify` | −4.2 % | 4.00 % |
| 4 | `p256/sign-verify` | +4.7 % | 4.00 % |

The gate tree — byte-identical binary, identical committed baseline — passed
**7/7** in the same session, and `main` passed 3/3. Interleaved same-source
measurement in that session put this box's run-to-run spread at up to **3.25 %**
against the check's **4.00 %** tolerance.

An instrument whose noise is 81 % of its tolerance fails intermittently on any
tree. That is the defect this branch repairs.

*(In progress. Sections are filled in as each measurement lands.)*
