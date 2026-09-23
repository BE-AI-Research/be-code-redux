#!/bin/sh
# BE-Code uninstaller — Linux & macOS.
#
#   ./uninstall.sh              remove the binary and shell completions;
#                               KEEPS ~/.be-code (config, sessions, history)
#   ./uninstall.sh --purge     also delete ~/.be-code after confirmation
#   ./uninstall.sh --yes       don't ask for confirmation (for scripts)
#
# Looks in ~/.local/bin, /usr/local/bin, $PREFIX/bin, and anywhere else
# 'be-code' resolves on PATH.
set -eu

BINARY=be-code
PURGE=0
ASSUME_YES=0

# Printed rather than sed'd out of this file: piped through a shell there is
# no file to read, and $0 is the shell itself.
usage() {
    cat <<'USAGE'
BE-Code uninstaller — Linux & macOS.

  ./uninstall.sh              remove the binary and shell completions;
                              KEEPS ~/.be-code (config, sessions, history)
  ./uninstall.sh --purge      also delete ~/.be-code after confirmation
  ./uninstall.sh --yes        don't ask for confirmation (for scripts)

Looks in ~/.local/bin, /usr/local/bin, $PREFIX/bin, and anywhere else
'be-code' resolves on PATH. Also runs without a checkout:

  curl -fsSL https://raw.githubusercontent.com/BE-AI-Research/be-code-redux/main/uninstall.sh | sh
USAGE
}

for arg in "$@"; do
    case "$arg" in
        --purge) PURGE=1 ;;
        --yes|-y) ASSUME_YES=1 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown option: $arg (try --help)" >&2; exit 1 ;;
    esac
done

say() { printf '%s\n' "$*"; }

# Piped through a shell (curl … | sh) stdin is the script itself, so a plain
# read would consume the script rather than the answer: ask the terminal.
confirm() {
    [ "$ASSUME_YES" = 1 ] && return 0
    if [ -t 0 ]; then
        printf '%s [y/N] ' "$1"
        read -r ans || return 1
    elif [ -r /dev/tty ]; then
        printf '%s [y/N] ' "$1" > /dev/tty
        read -r ans < /dev/tty || return 1
    else
        say "skipping (nothing to ask on): $1 — re-run with --yes"
        return 1
    fi
    case "$ans" in y|Y|yes|YES) return 0 ;; *) return 1 ;; esac
}

removed_any=0

# ---- binaries --------------------------------------------------------------

candidates="$HOME/.local/bin/$BINARY /usr/local/bin/$BINARY"
[ "${PREFIX:-}" != "" ] && candidates="$PREFIX/bin/$BINARY $candidates"
onpath=$(command -v "$BINARY" 2>/dev/null || true)
[ -n "$onpath" ] && candidates="$candidates $onpath"

seen=""
for p in $candidates; do
    case " $seen " in *" $p "*) continue ;; esac
    seen="$seen $p"
    [ -f "$p" ] || continue
    SUDO=""
    [ -w "$(dirname "$p")" ] || SUDO="sudo"
    if confirm "remove $p?"; then
        $SUDO rm -f "$p" && say "removed $p" && removed_any=1
    fi
done
[ "$removed_any" = 1 ] || say "no installed $BINARY binary found (already uninstalled?)"

# ---- shell completions -----------------------------------------------------

for f in \
    "${XDG_DATA_HOME:-$HOME/.local/share}/bash-completion/completions/$BINARY" \
    "$HOME/.zsh/completions/_$BINARY" \
    "$HOME/.config/fish/completions/$BINARY.fish"
do
    [ -f "$f" ] && rm -f "$f" && say "removed completion $f"
done

# ---- user data -------------------------------------------------------------

DATA_DIR="$HOME/.be-code"
if [ "$PURGE" = 1 ]; then
    if [ -d "$DATA_DIR" ]; then
        count=$(find "$DATA_DIR" -type f 2>/dev/null | wc -l | tr -d ' ')
        say ""
        say "--purge will delete $DATA_DIR ($count files):"
        say "  config.json, saved sessions, input history, checkpoints, custom commands"
        if confirm "permanently delete $DATA_DIR?"; then
            rm -rf "$DATA_DIR"
            say "deleted $DATA_DIR"
        else
            say "kept $DATA_DIR"
        fi
    else
        say "no $DATA_DIR to purge"
    fi
else
    [ -d "$DATA_DIR" ] && say "kept $DATA_DIR (config/sessions/history) — remove with: ./uninstall.sh --purge"
fi

say "BE-Code uninstall complete."
