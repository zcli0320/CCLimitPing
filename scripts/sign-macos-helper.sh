#!/bin/bash
set -euo pipefail

app_dir="${1:?usage: sign-macos-helper.sh APP}"
identity_file="${CODEX_HELPER_SIGNING_IDENTITY_FILE:-$HOME/Library/Application Support/CodexHelper-Signing/identity}"
identity="${CODEX_HELPER_SIGNING_IDENTITY:-}"
if [[ -z "$identity" && ! -e "$identity_file" ]] &&
   { [[ -n "${CODEX_HELPER_SIGNING_IDENTITY_FILE+x}" || -L "$identity_file" ]] ||
     [[ -e "$(dirname "$identity_file")/certificate.pem" ]]; }; then
  printf 'Configured signing identity file is missing: %s\n' "$identity_file" >&2
  exit 1
fi
if [[ -z "$identity" && -e "$identity_file" ]]; then
  identity="$(cat "$identity_file")"
  if [[ ! "$identity" =~ ^[[:xdigit:]]{40}$ ]]; then
    printf 'Invalid signing fingerprint in %s\n' "$identity_file" >&2
    exit 1
  fi
fi
if [[ -z "$identity" ]]; then
  printf 'No persistent signing identity configured; using ad-hoc signing (privacy grants may reset).\n' >&2
  identity="-"
elif [[ "$identity" == "-" ]]; then
  printf 'Use an unconfigured identity file for an explicit ad-hoc development build.\n' >&2
  exit 1
fi

# A configured identity must succeed: never retry a failed signature as ad-hoc.
for binary in codex-helper codex-runtime-bridge codex-accounts; do
  codesign --force --timestamp=none --sign "$identity" "$app_dir/Contents/MacOS/$binary"
done
codesign --force --timestamp=none --sign "$identity" "$app_dir"
codesign --verify --deep --strict "$app_dir"
