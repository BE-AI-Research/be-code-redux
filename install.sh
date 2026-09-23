#!/bin/sh
# BE-Code installer — Linux & macOS.
#
#   ./install.sh                 install for this user (~/.local/bin)
#   ./install.sh --system       install system-wide (/usr/local/bin, needs sudo)
#   PREFIX=/opt/be ./install.sh install under a custom prefix ($PREFIX/bin)
#   ./install.sh --no-setup     skip the first-run setup wizard
#
# It also runs with no checkout at all, straight from the repository:
#
#   curl -fsSL https://raw.githubusercontent.com/BE-AI-Research/be-code-redux/main/install.sh | sh
#
# Beside a checkout it prefers building from source when a Go toolchain
# (>=1.25, the floor go.mod sets) is present; otherwise it falls back to a
# prebuilt binary in dist/ or bin/ matching this platform — but only one that
# reports the same version as this tree's build.mk, so a stale binary left over
# from an older checkout is never installed under a newer version's name.
#
# Run on its own it reads the version from the repository's build.mk,
# downloads that release's binary for this platform, and falls back to fetching
# the source and building it when the release has no such asset. BE_CODE_REPO,
# BE_CODE_REF and BE_CODE_VERSION override where it fetches from and what it
# installs. Safe to re-run: upgrades in place. Undo with ./uninstall.sh.
set -eu

# Where a checkout-less install fetches from.
REPO="${BE_CODE_REPO:-BE-AI-Research/be-code-redux}"
REPO_REF="${BE_CODE_REF:-main}"
RAW_URL="https://raw.githubusercontent.com/$REPO/$REPO_REF"
REL_URL="https://github.com/$REPO/releases/download"
SRC_URL="https://codeload.github.com/$REPO/tar.gz/$REPO_REF"
MODULE=github.com/brown-enterprises/be-code

SRC_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
BINARY=be-code
RUN_SETUP=1
SYSTEM=0

# usage is printed rather than sed'd out of this file: piped through a shell
# there is no file to read, and $0 is the shell itself.
usage() {
    cat <<USAGE
BE-Code installer — Linux & macOS.

  ./install.sh                 install for this user (~/.local/bin)
  ./install.sh --system        install system-wide (/usr/local/bin, needs sudo)
  ./install.sh --no-setup      skip the first-run setup wizard
  PREFIX=/opt/be ./install.sh  install under a custom prefix (\$PREFIX/bin)

Without a checkout, install the current release straight from the repository:

  curl -fsSL $RAW_URL/install.sh | sh

  BE_CODE_REPO     repository to fetch from (default $REPO)
  BE_CODE_REF      branch or tag to read the version from (default $REPO_REF)
  BE_CODE_VERSION  install this exact release instead of the ref's version

Undo everything with ./uninstall.sh (or curl the same way).
USAGE
}

for arg in "$@"; do
    case "$arg" in
        --system)   SYSTEM=1 ;;
        --no-setup) RUN_SETUP=0 ;;
        -h|--help)  usage; exit 0 ;;
        *) echo "unknown option: $arg (try --help)" >&2; exit 1 ;;
    esac
done

if [ "${PREFIX:-}" != "" ]; then
    BIN_DIR="$PREFIX/bin"
elif [ "$SYSTEM" = 1 ]; then
    BIN_DIR=/usr/local/bin
else
    BIN_DIR="$HOME/.local/bin"
fi

say()  { printf '%s\n' "$*"; }
fail() { printf 'error: %s\n' "$*" >&2; exit 1; }

# ---- obtain a binary --------------------------------------------------------

# Everything is staged in a temp dir: run from curl there is no source tree to
# write into, and one that exists may not be writable.
WORK=$(mktemp -d "${TMPDIR:-/tmp}/be-code-install.XXXXXX")
TMP_BIN="$WORK/$BINARY"
trap 'rm -rf "$WORK"' EXIT

have() { command -v "$1" >/dev/null 2>&1; }

# fetch URL DEST — non-zero on a 404, no network, or no downloader at all.
fetch() {
    if have curl; then curl -fsSL --retry 2 -o "$2" "$1"
    elif have wget; then wget -qO "$2" "$1"
    else return 127
    fi
}

goversion_ok() {
    v=$(go env GOVERSION 2>/dev/null | sed 's/^go//') || return 1
    major=${v%%.*}; rest=${v#*.}; minor=${rest%%.*}
    [ "${major:-0}" -gt 1 ] 2>/dev/null && return 0
    [ "${major:-0}" -eq 1 ] && [ "${minor:-0}" -ge 25 ] 2>/dev/null
}

# binary_version prints the version a binary reports ("0.8.0"), or nothing
# if it cannot run here (wrong platform, not executable).
binary_version() {
    "$1" --version 2>/dev/null | sed -n 's/^be-code version //p' | head -1
}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
esac

# A checkout beside this script installs from it; anything else — a curl into a
# shell, a copy of this one file — installs from the repository.
LOCAL=0
if [ -f "$SRC_DIR/build.mk" ] && [ -f "$SRC_DIR/go.mod" ]; then LOCAL=1; fi

# The version this tree is: what a source build is stamped with, and what a
# prebuilt binary must report to be accepted in its place. Remote installs
# learn it from the repository instead (remote_install). The read is guarded
# because a failing command substitution ends the script under `set -e`.
ver=dev
if [ "$LOCAL" = 1 ]; then
    ver=$(sed -n 's/^VERSION *:= *//p' "$SRC_DIR/build.mk" 2>/dev/null || true)
    ver=${ver:-dev}
fi

# remote_install puts the release binary for this platform in $TMP_BIN, or
# builds one from the fetched source when the release has no such asset. It
# sets $ver to what it installed.
remote_install() {
    if [ -n "${BE_CODE_VERSION:-}" ]; then
        ver=$BE_CODE_VERSION
    else
        fetch "$RAW_URL/build.mk" "$WORK/build.mk" || fail "cannot read $RAW_URL/build.mk — no network, no curl or wget, or $REPO has no build.mk on '$REPO_REF' yet. Pin a release with BE_CODE_VERSION=<x.y.z>, or clone the repository and run ./install.sh from it."
        ver=$(sed -n 's/^VERSION *:= *//p' "$WORK/build.mk")
        [ -n "$ver" ] || fail "no VERSION line in $RAW_URL/build.mk"
    fi

    asset="$REL_URL/v$ver/$BINARY-$os-$arch"
    say "fetching $BINARY $ver for $os/$arch from $REPO..."
    if fetch "$asset" "$TMP_BIN"; then
        chmod +x "$TMP_BIN"
        got=$(binary_version "$TMP_BIN")
        [ -n "$got" ] || fail "the downloaded binary does not run on this machine ($os/$arch): $asset"
        say "downloaded $BINARY $got ($os/$arch)"
        return 0
    fi

    say "no release asset for $os/$arch at v$ver; building from source instead"
    have go && goversion_ok || fail "no release binary for $os/$arch at v$ver and no Go >=1.25 toolchain to build one. Install Go 1.25+, or download a binary yourself from https://github.com/$REPO/releases"
    fetch "$SRC_URL" "$WORK/src.tar.gz" || fail "cannot fetch the source from $SRC_URL"
    (cd "$WORK" && tar -xzf src.tar.gz) || fail "cannot unpack the source archive"
    src=""
    for d in "$WORK"/*/; do
        if [ -f "$d/go.mod" ]; then src=$d; break; fi
    done
    [ -n "$src" ] || fail "the source archive from '$REPO_REF' has no go.mod at its root"
    say "building from source with $(go version | awk '{print $3}')..."
    (cd "$src" && go build -ldflags "-s -w -X $MODULE/cmd.Version=$ver" -o "$TMP_BIN" .) \
        || fail "the source build failed"
    say "built $BINARY $ver from $REPO@$REPO_REF"
}

built=0
stale=""

if [ "$LOCAL" = 0 ]; then
    remote_install
    built=1
fi

if [ "$built" = 0 ] && have go && goversion_ok; then
    say "building from source with $(go version | awk '{print $3}')..."
    if (cd "$SRC_DIR" && go build -ldflags "-s -w -X $MODULE/cmd.Version=$ver" -o "$TMP_BIN" .); then
        built=1
        say "built $BINARY $ver from source"
    else
        say "warning: source build failed; trying a prebuilt binary"
    fi
fi

if [ "$built" = 0 ]; then
    for cand in "$SRC_DIR/dist/$BINARY-$os-$arch" "$SRC_DIR/bin/$BINARY"; do
        [ -f "$cand" ] || continue
        chmod +x "$cand" 2>/dev/null || true
        got=$(binary_version "$cand")
        if [ -z "$got" ]; then
            say "skipping ${cand#"$SRC_DIR"/}: it does not run on this machine ($os/$arch)"
            continue
        fi
        if [ "$got" != "$ver" ]; then
            say "skipping ${cand#"$SRC_DIR"/}: it reports $got but this tree is $ver"
            stale="$stale ${cand#"$SRC_DIR"/}=$got"
            continue
        fi
        cp "$cand" "$TMP_BIN"
        say "using prebuilt binary ${cand#"$SRC_DIR"/} ($got, $os/$arch)"
        built=1
        break
    done
fi

if [ "$built" = 0 ]; then
    if [ -n "$stale" ]; then
        fail "no Go >=1.25 toolchain here, and the prebuilt binaries are from another version ($stale) while this tree is $ver. Install Go on this machine, or run 'make -f build.mk release' where the tree was packaged and copy the fresh dist/ over."
    fi
    fail "no Go >=1.25 toolchain and no usable prebuilt binary found (dist/$BINARY-<os>-<arch> or bin/$BINARY). Install Go from your package manager, run 'make -f build.mk release' on a machine that has it, or install the published release with: curl -fsSL $RAW_URL/install.sh | sh"
fi

chmod +x "$TMP_BIN"
"$TMP_BIN" --help >/dev/null 2>&1 || fail "built/prebuilt binary failed a smoke test on this machine"
got=$(binary_version "$TMP_BIN")
[ "$got" = "$ver" ] || fail "the binary about to be installed reports version '$got', not the $ver being installed"

# ---- install ---------------------------------------------------------------

SUDO=""
if [ ! -d "$BIN_DIR" ]; then
    mkdir -p "$BIN_DIR" 2>/dev/null || SUDO="sudo"
    [ -z "$SUDO" ] || $SUDO mkdir -p "$BIN_DIR"
fi
if [ ! -w "$BIN_DIR" ]; then
    SUDO="sudo"
    say "$BIN_DIR is not writable; using sudo"
fi

$SUDO install -m 0755 "$TMP_BIN" "$BIN_DIR/$BINARY"
say "installed $BIN_DIR/$BINARY ($("$BIN_DIR/$BINARY" --version 2>/dev/null))"

# ---- shell completions (best effort) ---------------------------------------

install_completion() {
    shell=$1 dir=$2 file=$3
    [ -d "$dir" ] || mkdir -p "$dir" 2>/dev/null || return 0
    [ -w "$dir" ] || return 0
    "$BIN_DIR/$BINARY" completion "$shell" > "$dir/$file" 2>/dev/null \
        && say "installed $shell completion: $dir/$file"
}
install_completion bash "${XDG_DATA_HOME:-$HOME/.local/share}/bash-completion/completions" "$BINARY"
install_completion zsh  "$HOME/.zsh/completions" "_$BINARY"
install_completion fish "$HOME/.config/fish/completions" "$BINARY.fish"

# ---- PATH check ------------------------------------------------------------

case ":$PATH:" in
    *":$BIN_DIR:"*) ;;
    *)
        say ""
        say "NOTE: $BIN_DIR is not on your PATH. Add this to your shell profile:"
        say "  export PATH=\"$BIN_DIR:\$PATH\""
        ;;
esac

# ---- first-run setup -------------------------------------------------------

say ""
if [ "$RUN_SETUP" = 1 ] && [ ! -f "$HOME/.be-code/config.json" ] && [ -t 0 ] && [ -t 1 ]; then
    say "running first-run setup (Ctrl+C to skip; re-run later with '$BINARY setup')"
    "$BIN_DIR/$BINARY" setup || say "setup skipped; run '$BINARY setup' when a backend is up"
else
    say "next steps:"
    say "  $BINARY setup    # probe local backends and pick a model"
    say "  $BINARY doctor   # check backend + workspace toolchain health"
    say "  $BINARY          # start the TUI in a project directory"
fi
if [ "$LOCAL" = 1 ]; then
    say "uninstall any time with: ./uninstall.sh"
else
    say "uninstall any time with: curl -fsSL $RAW_URL/uninstall.sh | sh"
fi
