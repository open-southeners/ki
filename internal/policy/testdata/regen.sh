#!/usr/bin/env bash
# Regenerate testdata/*.json from testdata/*.yaml using real `docker compose config`.
# Needs Docker Compose v2+ (parses only; never talks to the daemon).
# The throwaway project dir is replaced by the literal @ROOT@ placeholder, and its
# parent by @ROOT@/.., so tests can substitute any real root at run time.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
for y in "$here"/*.yaml; do
  name=$(basename "$y" .yaml)
  work=$(mktemp -d)
  mkdir "$work/proj"
  cp "$y" "$work/proj/compose.yaml"
  touch "$work/proj/app.env"
  docker compose -f "$work/proj/compose.yaml" config --format json --no-env-resolution \
    | sed -e "s#$work/proj#@ROOT@#g" -e "s#$work#@ROOT@/..#g" > "$here/$name.json"
  rm -rf "$work"
done
