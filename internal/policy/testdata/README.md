# Policy fixtures

Each `*.json` is the output of `docker compose config --format json --no-env-resolution`
for the sibling `*.yaml`, with the throwaway project directory replaced by the `@ROOT@`
placeholder (tests substitute a real temp dir). `clean` must pass; every other fixture is
named `<rule>` or `<rule>__<variant>` after the single rule it must trigger.

Regenerate with `./regen.sh` (needs Docker Compose; it only parses, never touches the daemon).
`--no-env-resolution` keeps `env_file` entries in the model so the policy can check them.
