# Intentional differences from the Python implementation

Behavior the Go port changes on purpose. The goldens still pin Python's
observable output; entries here are improvements no scenario can see (or,
after the cutover, golden updates recorded with `-impl=go -update`).

## sandbox

- The learned grant store (`sandbox-learned.json`) is updated under an
  exclusive lock (`sandbox-learned.json.lock` next to it, 10 s timeout):
  concurrent `sandbox allow` / `allow-domain` / `forget*` runs and auto
  grants no longer lose each other's entries. The store's byte format is
  unchanged.
