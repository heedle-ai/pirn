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

A source's text can't forge that structure: a `<source>`, `<instruction>` or `<skill>` tag inside it, opening or closing, has its `<` written as `&lt;`.

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

## Agent steps

`(agent "instruction")` is the step for work, not just answers. Claude gets the [bash tool](https://docs.claude.com/en/docs/agents-and-tools/tool-use/bash-tool) and runs commands in a fresh [bobbin session](https://github.com/heedle-ai/bobbin/wiki/How-It-Works): one container for this step alone, with no network, no capabilities and nothing mounted from the machine.

```lisp
(pipe (par (browse "https://news.ycombinator.com/") (browse "https://lobste.rs/"))
      (agent "Make out/stories.csv with every story's title, site and points, sorted by points."))
```

1. The session starts from the worker's agent image (`-agent-image`, a content-pinned image ID with `/bin/sh`). Each upstream output is written to `/work/in/<node>.txt` (a page as its title, URL and text).
2. Claude is told what's in `/work/in` and given the instruction. Each `bash` call runs in a new shell in `/work` (files persist; `cd` and variables don't). Its output, exit code and up to 64 KiB of output go back to Claude.
3. When Claude replies without a command, or after 30 turns, the step collects the files in `/work/out` and lists everything that changed in the container (`podman diff`). Then the container is removed.

The step's output is the result as JSON (`pirn.AgentResult`): Claude's reply, model and tokens like `ask`, plus every command with its exit code, time and output, the files from `/work/out`, the changes, and which input became which file. A later step that reads it (an `ask`, say) sees only the reply: the commands and files are in the record and on the step's page, not in the next prompt. Everything is bounded:

| Limit | Value |
|---|---|
| Turns | 30 (`MaxTurns`), then `stopReason` is `max_turns`; commands asked for on the last turn aren't run, since Claude would never see their results |
| Commands | 60 a step (`MaxCommands`); later ones are answered "Not run" |
| One command | 2 minutes (`CommandTimeout`); a command past it is stopped with every other process in the session, so it can't change files later |
| Output Claude sees | 8 KiB of each command's output (`ClaudeOutput`), so a noisy command can't fill its context; a silent command is "(no output)" |
| Each command's output kept in the record | 4 KiB of standard output and of standard error, marked when cut |
| Files kept from `/work/out` | 20 files, any file name. In the record: 256 KiB each, 512 KiB in all. With a registry (below), a larger file goes there instead: 4 GiB each, 16 GiB a step. The rest, and unreadable files, are listed as skipped (20 named, then a count) |
| Changes listed | 500 |
| The whole result | under 1 MiB (`MaxResult`), well inside Temporal's 2 MB payload limit: if escaping makes it larger, recorded output is cut further, then files are dropped |

### Skills

An agent step can use a skill: a folder whose `SKILL.md` says how to do a kind of work, with any scripts or references it needs, in the format of [Anthropic's Agent Skills](https://docs.claude.com/en/docs/agents-and-tools/agent-skills/overview). [treadle](https://github.com/heedle-ai/treadle) keeps skills and turns each into a command; pirn only runs one. `Question.Skill` holds the folder's files by path: they are written to `/work/skill` before Claude starts, and `SKILL.md` itself is in Claude's first message, inside `<skill>` tags, so Claude starts by reading it. The system prompt says a skill tells Claude how to work, while the step's inputs stay data.

`CheckSkill` refuses a folder, before any container starts, unless it has `SKILL.md`, at most 200 files and 4 MiB in all, and every path is relative and made of letters, digits, `.`, `_` and `-`, with no segment starting with `.` or `-`. Other agent steps, such as a skill's, run through `pirn.Step`, which heartbeats and classifies errors exactly as the `agent` command does.

### Large files

A worker given a registry (`-agent-registry ghcr.io/acme/heedle-files` on `loom worker` and `drawdown`, or `Agent.Registry` in Go) keeps a file too big for the record in that OCI registry instead of skipping it. Its size is read first (`ls -ln`), so a file over the limits is skipped without being copied. Then it is streamed out of the session (bobbin's `ReadTo`) to a temporary file on the worker, hashed, and pushed with [oras](https://oras.land) (1.2 or newer, on the worker's `PATH`) as one artifact whose only layer is the file, so the layer's digest is the file's own SHA-256. The tag is `sha256-<hex>`, so registries that delete untagged manifests keep it, and the manifest's creation time is fixed (`1970-01-01T00:00:00Z`), so the same file always gives the same manifest. If the registry already has the file (its tag resolves and its blob is there, as after a retried step), nothing is pushed, so registries whose tags can't be overwritten accept retries too. Every byte copied counts toward the step's 16 GiB, kept or not, and every file looked at counts toward its 20. oras uses the worker's own login (`oras login`, or Docker's credentials); the step never sees it.

The record keeps the file's path, size, SHA-256 and repository (`oci`), never its bytes; loom writes those to `files/<node>/<path>` as a small pointer. To get the file back:

```bash
oras blob fetch --output stories.csv ghcr.io/acme/heedle-files@sha256:<sha256>
```

A file the registry refuses (no login, no space) is listed as skipped, with oras's reason, rather than failing the step, since a retry would rerun the whole agent. Keeping files in the registry stops after 5 minutes (`CollectTimeout`), within the step's own 20; files still to go are listed as skipped. The temporary copy is removed as soon as it's pushed; give workers disk for the largest file (4 GiB), and note that a worker that crashes mid-copy leaves its copy in `$TMPDIR` (as `pirn-large-*`). The step heartbeats while copying and pushing. A record shrunk to fit its 1 MiB never drops a file kept in a registry, whose pointer is tiny. Before any of this, every process the agent left running is stopped, so nothing changes `/work/out` while it is read; if that fails three times, the files are collected anyway, with a note in `skipped`.

A reply cut off at the output limit (`max_tokens`) may hold a half-written command, so its commands aren't run and Claude is told why. A rate limit or overload mid-step is retried in place (three tries, waiting as long as `Retry-After` asks, up to a minute), so the session and the work in it are kept; after that the error goes to Temporal, which retries the step from the start.

The session's disk, memory and CPU are bobbin's: the hardening flags limit memory and processes, and its writable layer is on the worker's disk, so give workers disk to spare.

## Laws

`ask` is a `weft.Arrow`, registered the same way as reed's `browse` and bobbin's `container`, so it composes under `pipe` and `par` exactly as they do. Two law tests check it with a deterministic stand-in for Claude: weft's identity laws (`TestLaw_AskIsAWeftArrow`), and that the question is a function of the step's inputs alone (`TestLaw_PromptIsAFunctionOfInputs`). Claude's answers themselves can differ between runs, so the laws hold up to the answer, which is why every answer is recorded with what it was given.

## Where it stops

- **Answers differ between runs.** That's the nature of the model; the record keeps each answer with the sources it was given.
- **Sources can try to steer Claude.** A page can contain text addressed to the model. The system prompt says to treat sources as data, and an ask step has no tools, so the most a page can do is distort the answer, which is recorded next to the page that did it.
- **One model per worker.** Choosing the model in the program is **Planned**.
