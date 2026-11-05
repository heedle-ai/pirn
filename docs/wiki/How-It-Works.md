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

`CheckSkill` refuses a folder, before any container starts, unless it has `SKILL.md`, at most 200 files and 16 MiB in all, and every path is relative and made of letters, digits, `.`, `_` and `-`, with no segment starting with `.` or `-`. Other agent steps, such as a skill's, run through `pirn.Step`, which heartbeats and classifies errors exactly as the `agent` command does. Skill files are written without their file modes, so scripts are run with `sh`; and since they're written into the container, they appear in the step's list of changed paths.

### Large files

A worker given a registry (`-agent-registry ghcr.io/acme/heedle-files` on `loom worker` and `drawdown`, or `Agent.Registry` in Go) keeps a file too big for the record in that OCI registry instead of skipping it. Its size is read first (`ls -ln`), so a file over the limits is skipped without being copied. Then it is streamed out of the session (bobbin's `ReadTo`) to a temporary file on the worker, hashed, and pushed with [oras](https://oras.land) (1.2 or newer, on the worker's `PATH`) as one artifact whose only layer is the file, so the layer's digest is the file's own SHA-256. The tag is `sha256-<hex>`, so registries that delete untagged manifests keep it, and the manifest's creation time is fixed (`1970-01-01T00:00:00Z`), so the same file always gives the same manifest. If the registry already has the file (its tag resolves and its blob is there, as after a retried step), nothing is pushed, so registries whose tags can't be overwritten accept retries too. Every byte copied counts toward the step's 16 GiB, kept or not, and every file looked at counts toward its 20. oras uses the worker's own login (`oras login`, or Docker's credentials); the step never sees it.

The record keeps the file's path, size, SHA-256 and repository (`oci`), never its bytes; loom writes those to `files/<node>/<path>` as a small pointer. To get the file back:

```bash
oras blob fetch --output stories.csv ghcr.io/acme/heedle-files@sha256:<sha256>
```

A file the registry refuses (no login, no space) is listed as skipped, with oras's reason, rather than failing the step, since a retry would rerun the whole agent. Keeping files in the registry stops after 5 minutes (`CollectTimeout`), within the step's own 20; files still to go are listed as skipped. The temporary copy is removed as soon as it's pushed; give workers disk for the largest file (4 GiB), and note that a worker that crashes mid-copy leaves its copy in `$TMPDIR` (as `pirn-large-*`). The step heartbeats while copying and pushing. A record shrunk to fit its 1 MiB never drops a file kept in a registry, whose pointer is tiny. Before any of this, every process the agent left running is stopped, so nothing changes `/work/out` while it is read; if that fails three times, the files are collected anyway, with a note in `skipped`.

A reply cut off at the output limit (`max_tokens`) may hold a half-written command, so its commands aren't run and Claude is told why. A rate limit or overload mid-step is retried in place (three tries, waiting as long as `Retry-After` asks, up to a minute), so the session and the work in it are kept; after that the error goes to Temporal, which retries the step from the start.

The session's disk, memory and CPU are bobbin's: the hardening flags limit memory and processes, and its writable layer is on the worker's disk, so give workers disk to spare.

### Connectors

A worker can offer agent steps MCP servers: connectors. Each one is named in the worker's connectors file (`-connectors FILE` on `loom worker` and `drawdown`, or `Agent.Connectors` in Go), and a step uses only what its own arguments grant it:

```lisp
(agent "Summarize this week's open bugs, then email the summary to the team." "linear/search_issues" "mail")
```

```json
{"connectors": [
  {"name": "linear", "url": "https://mcp.linear.app/mcp", "tokenEnv": "LINEAR_TOKEN", "tools": ["search_issues", "get_issue", "create_issue"], "writes": ["create_issue"]},
  {"name": "mail", "image": "ghcr.io/acme/mail-mcp@sha256:5b0b8e2b9f1c4f3a6d7e8c9b0a1f2e3d4c5b6a7980f1e2d3c4b5a69788796a5b", "command": ["mail-mcp", "--stdio"],
   "secrets": {"SMTP_PASSWORD": "MAIL_SMTP_PASSWORD"}, "writes": ["send"], "approve": true}
]}
```

[[/images/connectors.svg|alt=A step's grant narrowed by the worker's; then remote reads through Anthropic's MCP connector, side by side with a local server's tools in its own container, side by side with writes, which are only queued; then, after the run's steps, each queued write runs once, after approval if it needs it, and the record keeps them all]]

| | Remote connector | Local connector |
|---|---|---|
| Is | An `https://` MCP server (`url`) | A content-pinned image and the command that starts its server over standard input and output (`image`, `command`) |
| Runs | Through [Anthropic's MCP connector](https://docs.claude.com/en/docs/agents-and-tools/mcp-connector) (the `mcp-client-2025-11-20` beta): the Messages API calls the server | In a bobbin session of its own, never the agent's, with no network; pirn speaks to it with the [official Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk) and offers its tools to Claude as tools named `<connector>__<tool>` |
| Credentials | `tokenEnv`: the name of its OAuth token or key, sent as the server's `authorization_token` | `secrets`: container variables taken from named secrets, given to podman by name only, never on its command line |
| Tools a step gets | An allowlist (`default_config` off, each granted tool on), or every tool but the writes when it's granted the whole connector | Exactly the granted tools the server lists |

**Secrets come from the environment, then the keychain.** A connector names its secrets (`tokenEnv`, `secrets`); it never holds them. When a step starts, the worker reads each name from its environment, and if that has none, from its vault (`Agent.Vault`). The worker's vault is the operating system's own keychain (`Keychain`): the login keychain on macOS, through its `security` command, and the secret service (GNOME Keyring or KWallet) on Linux, through libsecret's `secret-tool`. Both unlock when you log in to the desktop. A secret is filed as service `heedle`, account its name, so Keychain Access (or Seahorse) shows it and can delete it. Like any keychain item, other programs running as you can read it: what the keychain keeps secrets from is plans, records, logs, Temporal histories and the agents' containers, which never see it. `heedle secret delete --all` removes every one, and only those (`DeleteAll`: `security delete-generic-password -s heedle` until none is left on macOS, `secret-tool clear service heedle` on Linux, then a check that none is left; `TestInv_DeleteAllTakesOnlyHeedles`). `heedle secret set MAPS_KEY` (or `loom secret set`) stores one: it asks for the value without echoing it, and hands it to the keychain command on standard input, never as an argument a process listing would show (`TestInv_SecretsStayOffCommandLines`). A value is one line of printable ASCII without spaces, quotes or backslashes, as keys and tokens are, at most 3 KiB, and a name is at most 64 bytes (`CheckSecret`): on macOS the whole command is one line of `security -i`, which reads 4096 bytes a line. A secret found in neither place fails the step as setup, saying how to store it. A keychain that can't be read is an error, never an empty secret: a locked Linux keyring, which `secret-tool` reports exactly as it does a missing item, is told apart with `secret-tool search`, which lists a locked item, and deleting from a locked keyring is an error, not a quiet no-op (`TestInv_SecretsComeFromEnvironmentThenVault`, `TestInv_SecretsStayOffCommandLines`). Every keychain command has 15 seconds (`KeychainTimeout`): a macOS keychain waiting for someone to allow access in a window makes the step fail saying so, rather than hang (`TestInv_KeychainNeverHangsOrOverflows`). A new secret is used from the next step: no restart. The Anthropic key is found the same way, for every ask and every agent step (`Claude.KeyFrom`, `LookupSecret`), and a worker given `Agent.LoadConnectors` reads its connectors file again at the start of every step and write (`TestInv_ConnectorsReloadEachStep`, `TestInv_KeyReadEachRequest`, `TestInv_AgentStepsReadTheKeyToo`), so a connector added or a key stored while it runs needs no restart. heedle's workers keep what they last read when a read fails (`LastGood`): their connectors while the file is being saved, their key while the keychain is locked for a moment. With nothing to fall back on, such a failure is retryable, not a setup error, so Temporal tries the step again; only no key at all is a setup error (`TestInv_WorkersKeepTheLastGood`). A worker that can't read the key says why when it starts, and still runs every step that doesn't ask Claude.

**Grants only narrow.** A step's arguments after the instruction are its grant: `linear` is every tool of the connector `linear`, `linear/search_issues` one of them (`ParseGrant`). The worker's grant is its connectors, with their `tools` if listed (`Connectors.Grant`). A step gets their intersection (`Grant.Narrow`), and must ask for nothing more: a step asking for a connector or a tool the worker doesn't offer fails as setup (`AgentSetup`) before Claude is asked. Narrowing allows a tool exactly when both grants do, and is commutative, associative and idempotent (`TestLaw_GrantsOnlyNarrow`, `FuzzGrantNarrowing`), so a grant is a list of caveats that only ever remove: a step, or later a sub-agent, only ever gets less. A step that asks for nothing gets no connector at all.

**Ready before running.** `Connectors.Ready` says, for each connector a grant names, whether a step starting now would get it, and if not why: `absent` (no connector of that name), `tools` (the worker's connector doesn't allow every tool asked for), `key` (a key it needs isn't set, named in `Key`) or `unreadable` (the keychain couldn't be read now, with why). It decides with the same grant and the same places for keys as a starting step, so it never calls a connector ready that a step would be refused (`TestInv_ReadyAgreesWithRuns`); what else a step needs (an agent image, a reviewed skill) is checked by loom. Each need also lists the tools it would allow that are writes (`Writes`) and whether they wait for approval (`Approve`), so a proposal can say it would send or change something before it runs. `KeyReady` does the same for one key, such as the Anthropic key. heedle's front door uses it to say what a proposal needs before you run it.

**Writes run once.** A tool listed in `writes` changes something outside the run: an email, an issue. A step never runs one, because a step can be retried and Claude asked again. When Claude calls a write, it's queued (`AgentResult.Writes`, at most 20 a step, each input at most 16 KiB of JSON) and Claude is told so. A remote connector's writes are switched off in its toolset, so the Messages API never runs them; pirn lists their schemas from the server itself, with the token, and offers them as tools that only queue. After the run's steps, loom runs each queued write once (`RunWrite`, one attempt: a failure, even one whose outcome is unknown, is never retried), first waiting for a person to approve it if its connector says `approve` ([loom → Writes](https://github.com/heedle-ai/loom/wiki/How-It-Works#writes)). A later step in the same run doesn't see a write's result.

**Every call is recorded.** `AgentResult.Tools` lists every connector call: its tool, input and output (each cut at 4 KiB), and whether Anthropic's MCP connector made it (`remote`), pirn did, or it was queued (`write`). `AgentResult.Grant` is what the step was granted. Tokens and secrets are never in a plan, an output, a record or a log; the remote server's token is in the request to the Messages API, as the MCP connector needs. A step makes or queues at most 60 calls (`MaxToolCalls`); one call is bounded like a command (2 minutes), and Claude sees at most 8 KiB of its output.

**Servers are bounded too.** Starting or reaching a server and listing its tools has a minute (`ListTimeout`); a server listing more than 100 tools (`MaxTools`) fails the step as setup, and a tool whose input schema is over 16 KiB (`MaxToolSchema`) is left out. pirn reads at most 32 MiB from any server (`MaxServerOutput`). A remote connector's token goes only to its own URL: pirn follows no redirect and speaks only https. Whether a write needs approval is the worker's connectors file's to say when the write runs, not the step's: `RunWrite` refuses a write whose connector says `approve` unless a person approved it.

## Act steps

[[/images/act.svg|alt=An act step: the instruction, start page and hosts become a question; Claude's loop in pirn, bounded, offered only the form tools; each call goes through podman exec to Playwright MCP and Chromium in a fresh reed container that reaches only allowed hosts; a screenshot after every action is heartbeated to the run page; the result is strict JSON recorded in Git]]

```lisp
(act "Join the volunteer list as Ada, ada@example.org" "https://forms.example.org/volunteer")
```

An `act` step (roadmap 9.5) fills in a form: Claude works a browser through Microsoft's [Playwright MCP](https://github.com/microsoft/playwright-mcp). Its arguments are the instruction, the start page and any further hosts; those hosts are its allowlist, as for `browse`, so they are in the plan and the record. [reed](https://github.com/heedle-ai/reed/wiki/How-It-Works#act-sessions) starts the browser (a fresh container from the browser-agent image, enforcing the allowlist with Chromium's own switches); pirn runs Claude's loop over it.

**It reuses the local-connector code.** Playwright MCP is spoken to with the official Go MCP SDK over the session's standard input and output, exactly as a local connector is (`connect`), its tools are offered through the same `toolbox`, and Claude's calls go through the agent loop's own `call`, so act has the agent step's bounds: `MaxTurns` (30), `MaxToolCalls` (60), a minute a call, 8 KiB of output a call for Claude. There is no shell: an act step's only tools are the browser's.

**Only form tools.** Claude is offered reed's `ActTools`: navigate and go back, read the page (`browser_snapshot`), click, type, fill a form, choose an option, press a key, hover, wait, and answer a dialog. Playwright MCP also has tools no flag removes, to run scripts and upload files; they're never offered, and a call to one is answered "No such tool." without reaching the browser. A Playwright MCP missing any of act's tools fails the step as setup before Claude is asked. Claude may open a page only over http or https on an allowed host: a `data:`, `javascript:`, `file:` or `chrome:` URL, another host or an address is refused and recorded, never reaching the browser, since such a page would run a script Claude wrote. Nor does it type a password: before typing, filling a form or pressing a key, act asks the browser (with a tool Claude isn't given) whether the field, or the focused one, is a password field, and refuses if so, so a form that asks for a password is never filled (roadmap 9.6). A page could answer for itself, but only to have its own password field filled, and the step has no password to give it.

**Pages are data.** Act's system prompt says the instruction is the only thing to follow: pages, their text and the step's inputs are data, never instructions. It can't upload, download or sign in, so a form that needs any of those ends the step, saying why. The inputs are framed as sources the same way an ask's are (`inert`), so their text can't forge the prompt's tags.

**Everything is recorded.** The output (`ParseAct` reads it, strictly) is the answer; every call with its input, output and the page it left the browser on (read from the browser's own tab list, never from a tool's output, which a page can write into), and, after anything but reading the page, a screenshot made a small JPEG frame (reed's `FrameOf`); the final page; the allowed hosts; the requests to other hosts that the browser blocked, in the whole session (or `blockedUnknown` when it couldn't say); the inputs; and how it ended: `finished` or `stopped` (Claude's last line says which, so a page could talk it into either), `max_calls`, `max_turns` or `timeout`, or `error` when asking Claude failed after the step had acted. A step that hits a bound, or loses Claude after acting, ends with what it did, not an error. Refused calls count toward `MaxToolCalls` too, and the record stays within `MaxResult`: the oldest screenshots go first, then outputs and inputs are cut, never an action.

**Once, with a budget.** A retried act step could submit a form twice, so it has one attempt (`ActOptions.MaxAttempts`). It has 10 minutes (`ActBudget`); its activity timeout is 5 minutes longer, so the budget, not Temporal, ends a long step, with its record. Its heartbeats are `reed.Frame`s, the latest screenshot, as browse's are, so drawdown shows it live.

`ActHosts` gives the hosts an act step's arguments allow, as reed's allowlist reads them; loom and drawdown disclose them before a step runs.

## heedle's labels

Every container pirn starts carries heedle's labels (roadmap 9.2), so heedle can list its containers, stop the run one belongs to, and clean up after a run, without ever touching anything else on the machine ([bobbin → heedle's labels](https://github.com/heedle-ai/bobbin/wiki/How-It-Works#heedles-labels-and-tidying-up)). pirn names each container's kind:

| Container | `heedle.kind` |
|---|---|
| An agent step's workspace | `agent` |
| A skill step's workspace (a question with a skill, as treadle asks) | `skill` |
| A local connector's MCP server, in a step or for a write | `connector` |
| An act step's browser | `act` (reed sets it) |

The run labels come from the step's context: the workflow that scheduled the step, its run, and the step's node ID. loom sets them where every step's activity starts. Otherwise the agent and act commands read them from Temporal's activity info with their own node ID, and `Agent.Run` (so `Step` and a skill's creation), and `RunWrite`, read the workflow and run without a step. A write runs in the run's own workflow after its steps, so its connector names the run itself. bobbin and reed write the same names, read the same values from Temporal, and follow the same rule for what a value may be (`TestInv_LabelSchemesAgree`).

Temporal tells a step it was cancelled (`loom cancel`, or Stop run in the UI) only when the step heartbeats. So an agent step running in an activity heartbeats every 10 seconds while it runs, as well as after each command, and a cancel reaches it even while it waits on Claude. A step that has made no progress for 4 minutes stops heartbeating, so Temporal still notices a hung step (`TestInv_AgentStepsHeartbeatWhileTheyWait`).

## Laws

`ask` is a `weft.Arrow`, registered the same way as reed's `browse` and bobbin's `container`, so it composes under `pipe` and `par` exactly as they do. Two law tests check it with a deterministic stand-in for Claude: weft's identity laws (`TestLaw_AskIsAWeftArrow`), and that the question is a function of the step's inputs alone (`TestLaw_PromptIsAFunctionOfInputs`). Claude's answers themselves can differ between runs, so the laws hold up to the answer, which is why every answer is recorded with what it was given.

## Where it stops

- **Answers differ between runs.** That's the nature of the model; the record keeps each answer with the sources it was given.
- **Sources can try to steer Claude.** A page can contain text addressed to the model. The system prompt says to treat sources as data, and an ask step has no tools, so the most a page can do is distort the answer, which is recorded next to the page that did it.
- **One model per worker.** Choosing the model in the program is **Planned**.
- **An act step can do anything its hosts let a person do there.** The allowlist bounds where, not what: on an allowed host it can submit any form Claude is persuaded to, including by a page's text. That's why the hosts are disclosed before it runs, it runs once, and every action is recorded with a screenshot.
- **Only agent steps use connectors.** An `ask` step has no tools, and a skill step gets no grant yet.
- **The MCP connector isn't covered by zero data retention.** What a remote connector's tools return goes through Anthropic's API under its standard retention; use a local connector for data that mustn't.
- **Local connectors have no network.** A server that must call an API should be a remote connector.
