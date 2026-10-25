#!/usr/bin/env bash
# Build and install ghspot into ~/.local/bin (needs Go and a logged-in gh).
#   curl -fsSL https://raw.githubusercontent.com/jaderfeijo/ghspot/main/install.sh | bash
#   ... | bash -s -- --uninstall
# From a clone it builds that checkout; otherwise it builds github.com/jaderfeijo/ghspot at ITEM_VERSION's commit
# via `go install`.
set -euo pipefail

ITEM_ID=ghspot
ITEM_VERSION=0.1.0
OMARCHY_MIN=
OMARCHY_TESTED=
OMARCHY_MAX=
BASE="https://raw.githubusercontent.com/jaderfeijo/ghspot/main"
FILES=(install.sh)

# ---- installer contract v1 (https://github.com/jaderfeijo/omakit/blob/main/docs/contract.md) ----
# install.sh [--uninstall | --status | --check] [--dry-run] [--force]
MODE=install DRY=0 FORCE=0
for a in "$@"; do
  case $a in
    --uninstall) MODE=uninstall ;; --status) MODE=status ;; --check) MODE=check ;;
    --dry-run) DRY=1 ;; --force) FORCE=1 ;;
    *) echo "Unknown option: $a" >&2; exit 2 ;;
  esac
done
STATE="${STATE:-${XDG_STATE_HOME:-$HOME/.local/state}/$ITEM_ID}"   # installers may keep an older state dir
run() { if ((DRY)); then echo "[dry-run] $*"; else "$@"; fi; }
omarchy_version() { echo "${OMAKIT_OMARCHY_VERSION:-$(pacman -Q omarchy 2>/dev/null | awk '{print $2}' | sed 's/-[0-9]*$//')}"; }
# Refuse outside OMARCHY_MIN..OMARCHY_MAX (unless --force); warn above OMARCHY_TESTED.
guard() {
  local v bad=0; v=$(omarchy_version)
  if [[ -z $v ]]; then echo "Omarchy not found (pacman -Q omarchy)." >&2; ((FORCE)) || exit 1; return 0; fi
  [[ -n $OMARCHY_MIN ]] && (( $(vercmp "$v" "$OMARCHY_MIN") < 0 )) && bad=1
  [[ -n $OMARCHY_MAX ]] && (( $(vercmp "$v" "$OMARCHY_MAX") >= 0 )) && bad=1
  if ((bad)); then
    echo "Needs Omarchy $OMARCHY_MIN or newer${OMARCHY_MAX:+, older than $OMARCHY_MAX}; this is $v. (--force to try anyway)" >&2
    ((FORCE)) || exit 1
  elif [[ -n $OMARCHY_TESTED ]] && (( $(vercmp "$v" "$OMARCHY_TESTED") > 0 )); then
    echo "Note: not tested on Omarchy $v yet (newest tested: $OMARCHY_TESTED)." >&2
  fi
}
# SRC: the item's files. Taken from next to this script when they are all there (a checkout, or omakit's pinned
# copy); downloaded from BASE otherwise (curl | bash).
SRC="" SRC_TMP=""
get_files() {
  local f here="" ok=1
  [[ -n ${BASH_SOURCE[0]:-} && -f ${BASH_SOURCE[0]} ]] && here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
  [[ -n $here ]] && for f in "${FILES[@]}"; do [[ -f $here/$f ]] || ok=0; done
  if [[ -n $here && $ok == 1 ]]; then SRC=$here; return; fi
  SRC=$(mktemp -d); SRC_TMP=1; trap 'rm -rf "$SRC"' EXIT
  for f in "${FILES[@]}"; do
    curl -fsSL "$BASE/$f" -o "$SRC/$f" || { echo "Download failed: $f. Nothing was changed." >&2; exit 1; }
  done
}
record() { ((DRY)) || { mkdir -p "$STATE"; echo "$ITEM_VERSION" >"$STATE/version"; }; }
forget() { ((DRY)) || rm -rf "$STATE"; }
recorded() { [[ -f $STATE/version ]] && echo "installed $(cat "$STATE/version")"; }
# place SRC_FILE DEST MODE [sudo]: install a file, saying whether it was new, changed or unchanged.
place() {
  local s=$1 d=$2 m=$3 sudo=${4:-} how=Installed
  if [[ -f $d ]]; then cmp -s "$s" "$d" && how=Unchanged || how=Updated; fi
  [[ $how == Unchanged ]] || run $sudo install -Dm"$m" "$s" "$d"
  echo "$how  $d"
}
# same SRC_FILE DEST: true when DEST is byte-identical to the shipped file
same() { [[ -f $2 ]] && cmp -s "$1" "$2"; }
# ---- end of contract boilerplate ----

BIN="$HOME/.local/bin/ghspot"

case $MODE in
  status) if [[ -f $BIN ]]; then [[ -f $STATE/version ]] && recorded || echo "installed unknown"; else echo absent; fi ;;
  check) command -v go >/dev/null || { echo "Needs Go (mise use -g go, or pacman -S go)."; exit 3; }; exit 0 ;;
  uninstall) run rm -f "$BIN"; forget; echo "Removed ghspot." ;;
  install)
    guard
    command -v go >/dev/null || { echo "Needs Go (mise use -g go, or pacman -S go)." >&2; exit 1; }
    here=""; [[ -n ${BASH_SOURCE[0]:-} && -f ${BASH_SOURCE[0]} ]] && here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
    if [[ -n $here && -f $here/go.mod ]]; then
      echo "Building $here"
      ( cd "$here" && run go build -trimpath -o "$BIN" . )
    else
      echo "Building github.com/jaderfeijo/ghspot@main"
      run env GOBIN="$(dirname "$BIN")" go install github.com/jaderfeijo/ghspot@main
    fi
    command -v gh >/dev/null && gh auth status >/dev/null 2>&1 || echo "Note: ghspot needs the GitHub CLI logged in (gh auth login)."
    record; echo "Done. Run ghspot inside a git repo." ;;
esac
