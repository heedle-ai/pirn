- **Reuse, never rewrite.** Anthropic's Go SDK talks to Claude; pirn only builds the prompt and records the answer. Ask before adding anything that reimplements part of the SDK.
- **A mistake isn't fixed until it has a row on [Invariants](Invariants)** and a guard that fails on the old code. CI checks the page against the tests in both directions.
- **Update this wiki in the same change.** The pages live in `docs/wiki/` and publish on every push to `main`.
- **Never record the key.** Nothing in an answer, a plan or a log may contain it.
- Supported platforms are Linux (amd64, arm64) and macOS on Apple Silicon.

## Before opening a pull request

```bash
gofmt -l .        # prints nothing
go vet ./...
go test -race ./...
bash .github/invariants.sh
```

With `ANTHROPIC_API_KEY` set, `go test -run TestLive_ -v .` also asks the real Claude.
