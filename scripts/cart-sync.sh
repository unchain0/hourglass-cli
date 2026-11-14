#!/bin/sh
# Apply the cart spreadsheet to Hourglass. Intended for a timer: it is quiet
# when nothing changed and appends one JSON line per run to the log.
set -eu

CLI="${HOURGLASS_CLI:-$HOME/.local/bin/hourglass-cli}"
SHEET="${HOURGLASS_CART_SHEET:-$HOME/Documents/hourglass/programacao-carrinho-dezembro-2026.xlsx}"
LOG="${HOURGLASS_CART_LOG:-$HOME/.local/state/hourglass-cart-sync.log}"

[ -x "$CLI" ] || { echo "hourglass cli not found at $CLI" >&2; exit 1; }
[ -f "$SHEET" ] || exit 0

TOKENS_PATH="${TOKENS_PATH:-$HOME/.hourglass-rpa/auth-tokens.json}"
WEBAUTHN_TOKENS_PATH="${WEBAUTHN_TOKENS_PATH:-$TOKENS_PATH}"
WEBAUTHN_CREDENTIALS_PATH="${WEBAUTHN_CREDENTIALS_PATH:-$HOME/.hourglass-rpa/webauthn-credentials.json}"
export TOKENS_PATH WEBAUTHN_TOKENS_PATH WEBAUTHN_CREDENTIALS_PATH
export HOURGLASS_ALLOW_CART=1

mkdir -p "$(dirname "$LOG")"
printf '%s ' "$(date -Iseconds)" >>"$LOG"
"$CLI" schedule cart sync --file "$SHEET" --apply --prune >>"$LOG" 2>&1
