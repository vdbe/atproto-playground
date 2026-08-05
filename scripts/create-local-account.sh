#!/bin/sh
set -eu

root_dir=$(cd -- "$(dirname -- "$0")/.." && pwd)
env_file="$root_dir/.env"

require_var() {
  if [ -z "$2" ]; then
    printf 'error: %s is required in %s or the exported environment\n' "$1" "$env_file" >&2
    exit 1
  fi
}

if [ ! -f "$env_file" ]; then
  printf 'error: root environment file not found: %s\n' "$env_file" >&2
  exit 1
fi
if ! command -v goat >/dev/null 2>&1; then
  printf 'error: goat is required; install it with: go install github.com/bluesky-social/goat@latest\n' >&2
  exit 1
fi

# Save any pre-existing values (environment takes precedence over .env)
has_PDS="${ACCOUNT_PDS+x}";       saved_PDS="${ACCOUNT_PDS-}"
has_HANDLE="${ACCOUNT_HANDLE+x}"; saved_HANDLE="${ACCOUNT_HANDLE-}"
has_EMAIL="${ACCOUNT_EMAIL+x}";   saved_EMAIL="${ACCOUNT_EMAIL-}"
has_PASS="${ACCOUNT_PASSWORD+x}"; saved_PASS="${ACCOUNT_PASSWORD-}"

set -a
# shellcheck disable=SC1090
. "$env_file"
set +a

# Restore environment values over .env values
if [ -n "$has_PDS" ];    then ACCOUNT_PDS="$saved_PDS";          export ACCOUNT_PDS; fi
if [ -n "$has_HANDLE" ]; then ACCOUNT_HANDLE="$saved_HANDLE";    export ACCOUNT_HANDLE; fi
if [ -n "$has_EMAIL" ];  then ACCOUNT_EMAIL="$saved_EMAIL";      export ACCOUNT_EMAIL; fi
if [ -n "$has_PASS" ];   then ACCOUNT_PASSWORD="$saved_PASS";    export ACCOUNT_PASSWORD; fi

# Validate
require_var ACCOUNT_PDS      "${ACCOUNT_PDS:-}"
require_var ACCOUNT_HANDLE   "${ACCOUNT_HANDLE:-}"
require_var ACCOUNT_EMAIL    "${ACCOUNT_EMAIL:-}"
require_var ACCOUNT_PASSWORD "${ACCOUNT_PASSWORD:-}"

if ! curl --fail --silent --show-error --max-time 5 "${ACCOUNT_PDS%/}/xrpc/_health" >/dev/null; then
  printf 'error: PDS is unavailable at %s\n' "$ACCOUNT_PDS" >&2
  exit 1
fi

if ! ATP_PDS_HOST="$ACCOUNT_PDS" \
  NEW_ACCOUNT_HANDLE="$ACCOUNT_HANDLE" \
  NEW_ACCOUNT_EMAIL="$ACCOUNT_EMAIL" \
  NEW_ACCOUNT_PASSWORD="$ACCOUNT_PASSWORD" \
  goat account create; then
  printf 'error: account creation failed; the account may already exist and was not modified\n' >&2
  exit 1
fi
