#!/usr/bin/env sh
# Compatibility shim between treefmt and golangci-lint.
# Strips arguments after and including `--`

args=""
while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do
  args="$args $1"
  shift
done

# shellcheck disable=SC2086
exec golangci-lint $args
