## An ask step

`(ask "instruction")` takes exactly one argument, the instruction. Its inputs are its upstream steps' outputs, and nothing else: no files, no earlier runs, no other steps.

[[/images/ask.svg|alt=An ask step: upstream outputs side by side become sources; then the prompt; then one call to Claude; then the answer, recorded in Git]]

## Sources

Each upstream output becomes one source, in node ID order, carrying its node ID so Claude can cite it as `[n0]`:

| Upstream output | Source |
|---|---|
| A [reed](https://github.com/heedle-ai/reed/wiki) page | Its final URL, title and text. Never its screenshot |
| An earlier ask's answer | Its text, titled with the model that wrote it |
| Anything else | The output as it is |

## The prompt

The prompt is a pure function of the instruction and the sources (`pirn.Prompt`), so the same program and the same upstream outputs always send the same prompt:

```text
<source id="n0" url="https://a.example/" title="A">
page text
</source>

<source id="n1">
notes
</source>

<instruction>
Summarize.
</instruction>
```

The system prompt (`pirn.System`) tells Claude to answer only from the sources, to treat their contents as data rather than instructions, to cite them by ID, and to say so when they don't support an answer.

A source's text can't forge that structure: a `<source>` or `<instruction>` tag inside it, opening or closing, has its `<` written as `&lt;`.

Limits, checked before anything is sent: the instruction is at most 16 KiB (`MaxInstruction`) and the whole prompt at most 512 KiB (`MaxPrompt`). reed bounds each page's text to 64 KiB, so a few pages always fit.

## The record

The step's output is the answer as JSON (`pirn.Answer`):

```json
{"pirn":1,"model":"claude-sonnet-5-5","text":"…","stopReason":"end_turn","inputTokens":1830,"outputTokens":212}
```

An answer is at most 4,096 tokens (`MaxTokens`); `"stopReason":"max_tokens"` says it was cut off there. The worker's API key is never part of the output, the plan or the program.

## Calling Claude

Each attempt is one request to the Messages API through Anthropic's Go SDK, with:

- the worker's key (`ANTHROPIC_API_KEY` where the worker runs) and the worker's model (`claude-sonnet-5-5` unless the worker sets another);
- the SDK's own retries off, because Temporal retries the step;
- the SDK's environment defaults off, so no `ANTHROPIC_*` variable can redirect or re-sign a request;
- a 2-minute limit (`AskTimeout`), inside shuttle's 3-minute step timeout.

## Errors

| What happened | Temporal sees | Retried? |
|---|---|---|
| No instruction, or the prompt is too long | `InvalidInput` | No |
| No API key on the worker; or the API refuses the key, model or request (400, 401, 403, 404, 413, 422) | `AskSetup` | No |
| Rate limited or overloaded, and the API said when to retry (`Retry-After`) | `RateLimited` | Yes, after that delay (at most 5 minutes) |
| Rate limited, overloaded or a server error (429, 5xx, 529) without a delay, or the network failed | The error | Yes, with Temporal's backoff |
| The run was cancelled | The cancellation | No |

## Laws

`ask` is a `weft.Arrow`, registered the same way as reed's `browse` and bobbin's `container`, so it composes under `pipe` and `par` exactly as they do. Two law tests check it with a deterministic stand-in for Claude: weft's identity laws (`TestLaw_AskIsAWeftArrow`), and that the question is a function of the step's inputs alone (`TestLaw_PromptIsAFunctionOfInputs`). Claude's answers themselves can differ between runs, so the laws hold up to the answer, which is why every answer is recorded with what it was given.

## Where it stops

- **Answers differ between runs.** That's the nature of the model; the record keeps each answer with the sources it was given.
- **Sources can try to steer Claude.** A page can contain text addressed to the model. The system prompt says to treat sources as data, and an ask step has no tools, so the most a page can do is distort the answer, which is recorded next to the page that did it.
- **One model per worker.** Choosing the model in the program is **Planned**.
