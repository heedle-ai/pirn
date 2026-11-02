# pirn

Gives agent steps Claude: the `ask` command, which answers from a step's inputs and records the answer. Part of heedle-ai:
weft (algebra) → draft (language) → shuttle (execution) → bobbin (runtime) →
warp (object store) → drawdown (UI); reed gives browsing
agents a browser; pirn gives them Claude; loom runs them together.

## The first rule: reuse, never rewrite

Build on battle-tested tools (Git, Temporal, the Go standard library and
other long-proven software) through their public interfaces. Add on top of
them; never fork, patch, mutate or reimplement them. Prefer the real tool to
a reimplementation of it (for example the `git` CLI over a Go port of Git).
If a task seems to need a rewrite, stop and ask the owner first, explaining
why the existing tool can't do it.

## Rules for every change

1. **Keep the wiki current.** Any change that affects how a developer
   installs, builds, tests, uses or understands this repo updates
   `docs/wiki/` in the same commit or PR. That folder is the source of truth;
   `.github/workflows/wiki.yml` mirrors it to
   https://github.com/heedle-ai/pirn/wiki on every push to `main`. Never edit
   the wiki on GitHub directly: the next sync overwrites it. Code, README and
   wiki must never disagree.
2. **Wiki quality bar.** Every command is copy-pasteable and has been run.
   Every code snippet compiles and is checked by a test; prefer quoting a
   Go `Example` test with an `// Output:` comment. State required versions.
   No placeholders, TODOs or "coming soon" on published pages. Mark design
   that isn't built yet as **Planned**, plainly.
3. **Platforms: Linux (amd64, arm64) and macOS on Apple Silicon only.** Don't
   write Windows instructions, scripts, CI jobs or path handling.
4. **Bare minimum.** Add only what the task needs. No speculative packages,
   dependencies or abstractions.
5. **Tests pass before commit:** `go vet ./...` and `go test -race ./...`.
6. **Codify every mistake.** A mistake isn't fixed until it has a row in
   `docs/wiki/Invariants.md` (ID, invariant, guard, where it came from) and
   a guard that fails on the old code: a test, a fuzz seed, or a CI step
   (`ci:NAME`). `bash .github/invariants.sh` checks the page against the
   code in both directions and runs in CI. Name guard tests `TestLaw_*` or
   `TestInv_*`; every `Fuzz*` test is a guard. Keep inputs that ever broke a
   fuzz test in `testdata/fuzz/`.
7. **Show, don't tell.** Explain structure with string diagrams: SVGs in
   `docs/wiki/images/` that work in light and dark mode, using `;` for
   "then" and `⊗` for "side by side". Program diagrams are never drawn by
   hand; pirn's pages show programs without plan diagrams, and loom's
   `TestInv_WikiDiagramsMatchPlans` is the model if one is added.
8. **Get an independent review before significant pushes.** A fresh
   reviewer that hasn't seen the work tries to break it; every finding
   becomes an invariant row.

## Repo-specific

- pirn reimplements nothing. Anthropic's Go SDK
  (`github.com/anthropics/anthropic-sdk-go`) speaks the Messages API;
  Temporal retries; Git keeps the record. `TestInv_OnlyTheSDKTalksToClaude`
  keeps pirn's imports to an allowlist.
- An ask's inputs are its upstream outputs and nothing else. The prompt is a
  pure function of the instruction and those outputs (`Prompt`), so the record
  says exactly what an answer could have used.
- A reed page becomes its URL, title and text, never its screenshot.
- Each attempt is one request: the SDK's retries are off (Temporal retries)
  and so are its environment defaults (no `ANTHROPIC_*` variable may redirect
  or re-sign a request). The key comes from the worker and is never in an
  output, a plan or a log.
- Error classes are for Temporal: an invalid ask (`InvalidInput`) and a key,
  model or request the API refuses (`AskSetup`) are non-retryable; rate limits,
  overload and network errors are retryable. A cancelled ask reports the
  cancellation. A `Retry-After` from the API becomes Temporal's next retry
  delay.
- Source text can't forge the prompt's tags (`inert`).
- A skill is files in `/work/skill` plus its `SKILL.md` in the first message;
  pirn runs skills but doesn't keep them (treadle does).
- Agent steps run every command in a bobbin `Session` (never anything else),
  record every command, and are bounded (`MaxTurns`, `MaxCommands`,
  `CommandTimeout`, `ClaudeOutput`, `RecordedOutput`, `MaxOut*`,
  `MaxSkipped`, `MaxChanges`, `MaxResult`). Rate limits mid-step are
  retried in place so the session survives; anything else goes to Temporal. Container tests are
  `TestContainer_*`; CI's `containers` step runs them with
  `PIRN_REQUIRE_PODMAN=1`.
- Tests use a stand-in Messages API (`httptest`), so they need no key.
  `TestLive_*` asks the real Claude when `ANTHROPIC_API_KEY` is set; it is not
  an invariant guard, because CI has no key.
- `go.mod` replaces the heedle-ai modules with `../` checkouts until they
  are published; clone all repos as siblings.
