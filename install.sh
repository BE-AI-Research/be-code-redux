#!/bin/sh
# BE-Code installer — Linux & macOS.
#
#   ./install.sh                 install for this user (~/.local/bin)
#   ./install.sh --system       install system-wide (/usr/local/bin, needs sudo)
#   PREFIX=/opt/be ./install.sh install under a custom prefix ($PREFIX/bin)
#   ./install.sh --no-setup     skip the first-run setup wizard
#
# Prefers building from source when a Go toolchain (>=1.22) is present;
# otherwise falls back to a prebuilt binary in bin/ matching this platform.
# Safe to re-run: upgrades in place. Undo everything with ./uninstall.sh.
set -eu

SRC_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
BINARY=be-code
RUN_SETUP=1
SYSTEM=0

for arg in "$@"; do
    case "$arg" in
        --system)   SYSTEM=1 ;;
        --no-setup) RUN_SETUP=0 ;;
        -h|--help)  sed -n '2,12p' "$0"; exit 0 ;;
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

TMP_BIN="$SRC_DIR/.install-build-$$"
trap 'rm -f "$TMP_BIN"' EXIT

goversion_ok() {
    v=$(go env GOVERSION 2>/dev/null | sed 's/^go//') || return 1
    major=${v%%.*}; rest=${v#*.}; minor=${rest%%.*}
    [ "${major:-0}" -gt 1 ] 2>/dev/null && return 0
    [ "${major:-0}" -eq 1 ] && [ "${minor:-0}" -ge 22 ] 2>/dev/null
}

built=0
if command -v go >/dev/null 2>&1 && goversion_ok && [ -f "$SRC_DIR/go.mod" ]; then
    say "building from source with $(go version | awk '{print $3}')..."
    ver=$(sed -n 's/^VERSION *:= *//p' "$SRC_DIR/build.mk" 2>/dev/null)
    ver=${ver:-dev}
    if (cd "$SRC_DIR" && go build -ldflags "-s -w -X github.com/brown-enterprises/be-code/cmd.Version=$ver" -o "$TMP_BIN" .); then
        built=1
    else
        say "warning: source build failed; trying prebuilt binary"
    fi
fi

if [ "$built" = 0 ]; then
    os=$(uname -s | tr '[:upper:]' '[:lower:]')
    arch=$(uname -m)
    case "$arch" in
        x86_64|amd64) arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
    esac
    for cand in "$SRC_DIR/dist/$BINARY-$os-$arch" "$SRC_DIR/bin/$BINARY"; do
        if [ -f "$cand" ]; then
            cp "$cand" "$TMP_BIN"
            say "using prebuilt binary: ${cand#"$SRC_DIR"/} (verify it matches $os/$arch)"
            built=1
            break
        fi
    done
fi

[ "$built" = 1 ] || fail "no Go >=1.22 toolchain and no prebuilt binary found (bin/$BINARY or dist/). Install Go from your package manager, or run 'make release' on a machine that has it."

chmod +x "$TMP_BIN"
"$TMP_BIN" --help >/dev/null 2>&1 || fail "built/prebuilt binary failed a smoke test on this machine"

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
say "installed $BIN_DIR/$BINARY ($("$BIN_DIR/$BINARY" --help 2>/dev/null | head -1 | cut -c1-40)...)"

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
say "uninstall any time with: ./uninstall.sh"
