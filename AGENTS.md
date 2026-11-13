# Repository engineering practices

This file guides Codex and other coding agents working in this repository. The
Python tools deploy and control a Go service that writes laptop backlight sysfs
attributes on Talos nodes. Read README.md for supported hardware and operational
behavior. User instructions take precedence over this file.

## Before changing code

- Read the affected code, tests, and callers. Reuse established patterns and keep
  changes within the requested scope.
- For a nontrivial change, explain the proposed interface and affected state before
  implementation. Resolve material ambiguity with the user. Proceed on settled
  decisions and state reasonable reversible assumptions.
- Explain meaningful tradeoffs or risks plainly. Report verified outcomes, failed
  checks, and skipped checks accurately.

## Implementation and tests

- For a new behavior or bug fix, write the smallest meaningful failing test, make
  it pass, and then refactor. Work one behavior at a time. Cosmetic changes and
  documentation edits need checks appropriate to their effect.
- Test observable behavior through CLI, HTTP handlers, or controller interfaces.
  Cover malformed input, missing fields, numeric boundaries, unavailable nodes,
  interrupted operations, retries, and persisted state compatibility as relevant.
- Trace a changed value from its producer through all consumers. Search for sibling
  occurrences of a bug and fix the underlying cause across its lifecycle.
- Keep desired manifests derived from the complete inventory. Treat saved normal
  brightness and restoration journals as durable state. Preserve their meaning
  across restarts and upgrades, including journals created by earlier releases.
- Keep writes idempotent where the command permits it. Verify readback after a
  panel write. Serialize operations that share restoration state and preserve a
  recovery path after a partially failed operation.
- Treat deployment preview as read-only. Local and CI tests use fixtures and mocks.
  Apply manifests, patch Talos, reboot nodes, or change physical brightness only
  within the user's authorized operational scope.

## Python and Go quality

- Add Python parameter and return annotations to functions and methods. Represent
  JSON at external boundaries explicitly and validate before relying on its shape.
  Use safe JSON/YAML parsers. Avoid evaluation of untrusted code or unsafe loading.
- Public helpers and classes get concise docstrings explaining their contract or
  purpose. Prefer early returns, readable names, and focused functions. Avoid
  wrappers and abstractions that add no reusable behavior.
- Ruff defines Python formatting, import ordering, and lint rules. Mypy checks
  annotated Python code. Go uses gofmt, go vet, and the race-enabled test suite.
- Preserve machine-readable command output on stdout. Send diagnostics to stderr
  through logging or explicit CLI errors. Include useful operation context without
  exposing credentials or entire machine configurations.
- Keep direct Python dependency versions pinned and retain Go's module checksums.
  Verify package names and review release notes before dependency upgrades.

## Verification and review

- Install development dependencies from requirements-dev.txt and run `make check`
  before requesting review. Run `make build` for changes to the controller,
  packaging, compiler configuration, or image-cache export.
- Local checks and GitHub Actions must call the same Make targets. Keep cloud
  deployment and hardware operations out of the quality workflow.
- Before review, identify concrete failure modes in inputs, callers/docs, and
  restart/retry behavior. Verify the relevant tests or code paths. Record unresolved
  cases without claiming completion.
- Changes to credentials, host access, shutdown handling, or restoration state need
  independent review before production rollout. Report reviewer availability and
  unresolved findings explicitly. Review the actual source and consumers.
- Check branch divergence from the intended base before review. Rebase or rewrite
  history only when authorized by the user.

## Git and publication

- Use a descriptive feat/, fix/, or chore/ branch for requested commits. Commit
  messages follow `type(scope): short description` and explain the intent.
- Commit, push, publish, or create remote resources only when the user requests it.
- Keep secrets, private inventories, environment files, credentials, generated
  images, caches, and logs out of Git. Use documentation-reserved example IPs.
- State which checks ran and their outcomes. Explain operational consequences and
  any assumptions the user should verify. Shell commands intended for the user
  belong in fenced blocks, one command per block.
