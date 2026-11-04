## 1. Clone and test

pirn builds against sibling checkouts of the other heedle-ai repos, all public:

```bash
mkdir -p ~/src/heedle-ai && cd ~/src/heedle-ai
for r in weft draft shuttle bobbin reed pirn; do git clone https://github.com/heedle-ai/$r.git; done
cd pirn
go test -race ./...
```

The tests need no key: they use a stand-in for the Messages API, so they check the real SDK's requests (model, token limit, system prompt, prompt, key header) and how every error status is handled.

## 2. Ask the real Claude

With an [Anthropic API key](https://console.anthropic.com/settings/keys):

```bash
ANTHROPIC_API_KEY=sk-ant-... go test -run TestLive_ -v .
```

It gives Claude two sources, one saying the sky is blue and one saying it's green, and checks that Claude names the second.

## 3. Use it in a program

loom's worker and drawdown's built-in worker both run `ask` when `ANTHROPIC_API_KEY` is set where they run, and both take `-ask-model` (default `claude-sonnet-5-5`).

**Any platform**, with drawdown (see its [Getting Started](https://github.com/heedle-ai/drawdown/wiki/Getting-Started#ask-claude)): click **Example: ask** to have Claude judge the shipment sources.

**Linux**, with live pages: build a reed browser image ([reed → Getting Started](https://github.com/heedle-ai/reed/wiki/Getting-Started)), then start a loom worker with the image and your key:

```bash
cd ~/src/heedle-ai/loom
ANTHROPIC_API_KEY=sk-ant-... go run ./cmd/loom worker -store ~/heedle-store.git -browser-image <image ID>
```

and run:

```bash
go run ./cmd/loom run '(pipe
  (par (browse "https://news.ycombinator.com/")
       (browse "https://lobste.rs/"))
  (ask "What are today'"'"'s three most important engineering stories? Cite the source of each."))'
```

The two pages are fetched in parallel, each in a fresh browser that may reach only its own site; then Claude answers from their text. The run's commit records the pages and the answer, so running it again tomorrow and diffing the two commits shows what changed.

On macOS, run it through [heedle](https://github.com/heedle-ai/heedle/wiki/Getting-Started): `heedle up` downloads reed's published browser image, built for Linux arm64, into Podman's virtual machine. `ask` itself works on both.

## 4. Agent steps

An agent needs an image with a shell. Any content-pinned image with `/bin/sh` and a writable `/work` (or one root can create) works.

### The published agent image

`ghcr.io/heedle-ai/pirn-agent` is built from `images/agent/Containerfile` by `.github/workflows/image.yml`, for Linux amd64 and arm64 (the architecture of a Mac's Podman machine). heedle pins it by digest, so `heedle up` downloads it once and gives it to its worker.

It is Debian bookworm (`debian:bookworm-slim`, pinned by digest) with these packages, and nothing else is installed:

| Package | Why |
|---|---|
| `bash` | the shell most agent commands are written for; `/bin/sh` is Debian's dash |
| `coreutils` | `ls`, `cat`, `sort`, `tr`, `wc` and the rest of the basic file tools |
| `curl` | fetching over HTTP(S) where a step's network allows it |
| `ca-certificates` | so `curl`, `git` and Python can check HTTPS certificates |
| `git` | cloning and diffing, which agents use for code and for comparing files |
| `jq` | reading and reshaping JSON, the format most inputs and APIs use |
| `python3` | scripts for anything the shell tools can't do comfortably |

It runs as `agent` (uid 1000), not root, in `/work`, which that user owns. Every package added needs a row in this table: `TestInv_AgentImageIsPinnedAndUnprivileged` fails otherwise.

The workflow runs from the Actions tab (**Image → Run workflow**) or on a tag `image-v*`. Each architecture is built on its own runner, checked for the tools above, the user and `/work`, and must pass the agent container tests in real sessions before it is saved; only then are both pushed by digest, under one manifest list. The run's summary names the manifest list's digest, which is what heedle pins. The tag pushed with it (the `image-v*` tag, or `run-<run ID>`) is only a label.

To run those tests against an image of your own, give its ID as `PIRN_IMAGE` (the busybox one below is the default):

```bash
PIRN_IMAGE=<image ID> PIRN_REQUIRE_PODMAN=1 go test -count=1 -v -run '^(TestContainer_AgentInARealSession|TestContainer_AgentKeepsAwkwardFiles|TestContainer_AgentFollowsASkill)$' .
```

### Your own image

Build one with the tools your agents need, then give its ID to the worker, or after `--` with heedle (`heedle up -- -agent-image <image ID>`):

```bash
ANTHROPIC_API_KEY=sk-ant-... go run ./cmd/loom worker -store ~/heedle-store.git -agent-image <image ID>
go run ./cmd/loom run '(agent "Write out/primes.txt with the primes below 100, one per line.")'
```

pirn's own tests build a small one, `FROM scratch` with a static busybox (`apt-get install busybox-static` on Debian or Ubuntu), in `agent_test.go`.

## 5. In Go

```go
reg := draft.DefaultRegistry()
w := worker.New(c, "loom", worker.Options{})
err := pirn.Register(reg, w, pirn.Claude{APIKey: os.Getenv("ANTHROPIC_API_KEY")})
```

This is `ExampleRegister`, which the tests compile. `Register` adds the `ask` command to a draft registry and its activity to a worker. `TestInv_AskInAProgram` shows it inside a program, with Temporal's test environment.
