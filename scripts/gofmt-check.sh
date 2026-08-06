#!/usr/bin/env bash
# Fail if any tracked Go file needs gofmt.
#
# stdlib/ is vendored Go standard-library source -- kept byte-for-byte as
# upstream so that what goc compiles is what Go ships -- so it is excluded, the
# same exclusion CI has always used.
set -uo pipefail
cd "$(dirname "$0")/.."

unformatted=$(find . -name '*.go' -not -path './stdlib/*' -print0 | xargs -0 gofmt -l)
if [ -n "$unformatted" ]; then
	echo "These files need gofmt:"
	echo "$unformatted"
	exit 1
fi
echo "gofmt: clean"
