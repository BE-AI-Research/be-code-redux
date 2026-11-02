#!/bin/sh
# Exercises install.sh's verify_download against local files (curl and wget
# both read file:// URLs), without touching the network or installing anything.
# Run from the repository root: sh test/install/verify_download_test.sh
set -u
here=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/be-code-verify-test.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
REPO=example/repo
fails=0

# Pull the functions out of the installer rather than running it.
eval "$(sed -n '/^say()/p; /^have()/p; /^fetch() {/,/^}/p; /^sha256_of() {/,/^}/p; /^verify_download() {/,/^}/p' "$here/install.sh")"
# Every check runs verify_download in a subshell, so the installer's own
# exit-on-failure fail is what is exercised and only that subshell ends.
fail() { printf 'error: %s\n' "$*" >&2; exit 1; }

printf 'the binary\n' > "$WORK/be-code-linux-amd64"
printf 'not the binary\n' > "$WORK/other"
good=$(sha256_of "$WORK/be-code-linux-amd64")
[ -n "$good" ] || { echo "no SHA-256 tool on this machine"; exit 1; }

check() { # NAME EXPECT(ok|fail) SUMS_CONTENT [ERROR_SUBSTRING]
    name=$1; expect=$2; printf '%s' "$3" > "$WORK/sums"
    if (verify_download "$WORK/be-code-linux-amd64" "$name" "file://$WORK/sums" >/dev/null 2>&1); then got=ok; else got=fail; fi
    if [ "$got" != "$expect" ]; then echo "FAIL: $name expected $expect, got $got ($3)"; fails=$((fails+1)); return; fi
    echo "ok: $name -> $got"
}

check be-code-linux-amd64 ok   "$good  be-code-linux-amd64
0000  be-code-linux-arm64
"
check be-code-linux-amd64 ok   "$good *be-code-linux-amd64
"                                                      # the binary-mode marker
check be-code-linux-amd64 fail "$good  dist/be-code-linux-amd64
"                                                      # a dist/ prefix never matches (RELEASE.md step 4)
check be-code-linux-amd64 fail "0000  be-code-linux-amd64
"                                                      # a mismatch
check be-code-linux-amd64 fail ""                      # no line at all
check be-code-linux-amd64 fail "$good"                 # no trailing newline is still a line
rm -f "$WORK/sums"
if (verify_download "$WORK/be-code-linux-amd64" be-code-linux-amd64 "file://$WORK/missing" >/dev/null 2>&1); then
    echo "FAIL: a missing SHA256SUMS must not verify"; fails=$((fails+1))
else echo "ok: missing listing -> fail"; fi

[ "$fails" = 0 ] && echo "verify_download PASS" || { echo "verify_download FAIL ($fails)"; exit 1; }
