**pirn** gives agent steps Claude. Each `(ask "…")` step sends its upstream outputs to Claude as sources, with the instruction, and records what Claude answered: the text, the model, why it stopped and how many tokens it used.

```lisp
(pipe
  (par (browse "https://news.ycombinator.com/")
       (browse "https://lobste.rs/"))
  (ask "What are today's three most important engineering stories? Cite the source of each."))
```

The pages are fetched in parallel, each in a fresh browser ([reed](https://github.com/heedle-ai/reed/wiki)); then Claude answers from exactly what was fetched. The run's commit holds both: what the pages said and what Claude concluded. Run it again tomorrow and `git diff` shows what changed.

[[/images/ask.svg|alt=An ask step: upstream outputs side by side become sources (a page is its URL, title and text, never its screenshot); then the prompt; then one call to Claude through Anthropic's Go SDK with the worker's key; then the answer, recorded in Git without the key]]

## Why

A model's answer can't be replayed: ask again and it may differ. So, as with a web page, the answer itself is the evidence, and it's recorded with what it was given. Because each step sees only its upstream outputs, the record says exactly which sources an answer could have used.

pirn reimplements nothing: [Anthropic's Go SDK](https://github.com/anthropics/anthropic-sdk-go) speaks the Messages API, Temporal retries, and Git keeps the record.

## Where pirn fits

| Repo | Role |
|---|---|
| [weft](https://github.com/heedle-ai/weft/wiki) | Composition algebra |
| [draft](https://github.com/heedle-ai/draft/wiki) | Language: s-expressions that compile to shuttle plans |
| [shuttle](https://github.com/heedle-ai/shuttle/wiki) | Durable execution on Temporal |
| [bobbin](https://github.com/heedle-ai/bobbin/wiki) | A fresh Podman container per agent step |
| [reed](https://github.com/heedle-ai/reed/wiki) | A fresh browser per browsing step |
| **pirn** | Claude for agent steps (this repo) |
| [warp](https://github.com/heedle-ai/warp/wiki) | Content-addressed store |
| [drawdown](https://github.com/heedle-ai/drawdown/wiki) | Web UI: one tab per agent |
| [loom](https://github.com/heedle-ai/loom/wiki) | Runs them together |

## Pages

- [Getting Started](Getting-Started): run the tests, ask the real Claude, run the pipeline above
- [How It Works](How-It-Works): sources, the prompt, the record, errors
- [Invariants](Invariants): every guarantee, the test that guards it, and where it came from
- [Contributing](Contributing)
