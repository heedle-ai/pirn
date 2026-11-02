## 1. Clone and test

pirn builds against sibling checkouts of the other heedle-ai repos. bobbin, reed and pirn are private, so let git use your GitHub CLI login:

```bash
gh auth login        # once; bobbin, reed and pirn are private
gh auth setup-git    # lets git clone them with your gh login
mkdir -p ~/src/heedle-ai && cd ~/src/heedle-ai
for r in weft draft shuttle reed pirn; do git clone https://github.com/heedle-ai/$r.git; done
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

On macOS, reed's browser image must be built inside Linux, which reed lists as **Planned**; `ask` itself works on both.

## 4. In Go

```go
reg := draft.DefaultRegistry()
w := worker.New(c, "loom", worker.Options{})
err := pirn.Register(reg, w, pirn.Claude{APIKey: os.Getenv("ANTHROPIC_API_KEY")})
```

This is `ExampleRegister`, which the tests compile. `Register` adds the `ask` command to a draft registry and its activity to a worker. `TestInv_AskInAProgram` shows it inside a program, with Temporal's test environment.
