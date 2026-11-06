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

Every request Claude answers, an ask's or an agent or act step's turn, is noted the moment it answers on the `Meter` the context carries (`WithMeter`, `MeterFrom`): its model and its input and output tokens (`Call`). The step's output records the same counts only if the step succeeds; the meter sees them whatever becomes of the step, so loom can count the calls no record holds, such as an attempt that failed (roadmap 10.4.1, [loom → Costs](https://github.com/heedle-ai/loom/wiki/How-It-Works)). A request Claude refused used nothing and isn't noted. Without a meter nothing is counted.

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

**Only form tools.** Claude is offered reed's `ActTools`: navigate and go back, read the page (`browser_snapshot`), click, type, fill a form, choose an option, press a key, hover, wait, and answer a dialog. Playwright MCP also has tools no flag removes, to run scripts and upload files; they're never offered, and a call to one is answered "No such tool." without reaching the browser. A Playwright MCP missing any of act's tools fails the step as setup before Claude is asked. Claude may open a page only over http or https on an allowed host: a `data:`, `javascript:`, `file:` or `chrome:` URL, another host or an address is refused and recorded, never reaching the browser, since such a page would run a script Claude wrote. Nor does it type a password: before typing, filling a form or pressing a key, act asks the browser (with a tool Claude isn't given) whether the field Playwright would type into (a label's control), or the focused one (followed into shadow roots and frames), is a password field, and refuses if so, or if the browser can't say, as for focus inside a frame from another host. Text that would move to another field as it is typed (a Tab, or a newline typed key by key) is refused too, and a refused call's text isn't recorded. So a form with a password field is never filled (roadmap 9.6, review 40). A page could answer for itself, but only to have its own password field filled, and the step has no password to give it; a form that asks for a password in an ordinary field relies on act's instructions to stop.

**Pages are data.** Act's system prompt says the instruction is the only thing to follow: pages, their text and the step's inputs are data, never instructions. It can't upload or download, so a form that needs either ends the step, saying why; for a sign-in it asks for you ([Take the wheel](#take-the-wheel)). The inputs are framed as sources the same way an ask's are (`inert`), so their text can't forge the prompt's tags.

**Everything is recorded.** The output (`ParseAct` reads it, strictly) is the answer; every call with its input, output and the page it left the browser on (read from the browser's own tab list, never from a tool's output, which a page can write into), and, after anything but reading the page, a screenshot made a small JPEG frame (reed's `FrameOf`), password fields always masked; the final page; the allowed hosts; the requests to other hosts that the browser blocked, in the whole session (or `blockedUnknown` when it couldn't say); the inputs; and how it ended: `finished` or `stopped` (Claude's last line says which, so a page could talk it into either), `max_calls`, `max_turns` or `timeout`, or `error` when asking Claude failed after the step had acted. A step that hits a bound, or loses Claude after acting, ends with what it did, not an error. Refused calls count toward `MaxToolCalls` too, and the record stays within `MaxResult`: the oldest screenshots go first, then outputs and inputs are cut, never an action.

**It stops before it sends** (roadmap 9.7).

[[/images/act-pause.svg|alt=An act step fills a form, calls confirm with a summary, pauses with the browser open while its heartbeat shows the summary and a screenshot; a person decides; a yes lets the next action send, if it is a click on the page the person saw, opening reed's hold just for it; a no or no answer in 30 minutes ends the step with nothing sent]]

Until a person confirms, the browser sends no form: reed's hold aborts every request but GET, HEAD and OPTIONS ([reed → Act sessions](https://github.com/heedle-ai/reed/wiki/How-It-Works#act-sessions)). Claude is offered one more tool, `confirm(summary)`, and told to call it right before each click that sends a form, naming what will be sent. The call doesn't reach the browser: act takes a screenshot, gives the send a random ID, heartbeats it (`ActBeat`: the frame and the pending send) and asks `Act.Confirm`, which loom answers from the person's decision on the run page or with `loom forms`. While it waits, the browser stays open and the step's budget stands still. A yes lets the next action send, and only it: if it's a click or key press, and the page's outline (each field with its value, as Playwright MCP's snapshot gives it) is still the one the person was shown, act runs reed's `ReleaseJS` (a tool Claude isn't given) just before it and `CloseJS` just after. Any other action, or a page that changed, is refused, opens nothing, and cancels the yes; Claude must ask again. A confirm with no summary is refused and asks no one. A no, no answer within 30 minutes (`ConfirmTimeout`), a worker with no way to ask, or more than five sends (`MaxConfirmations`) ends the step with `ended: "cancelled"` and nothing more run. Each form that posts needs its own confirmation, since the browser can't tell a form's last page from its first. Every confirm is a call in the record, with its screenshot, and `held` lists what the hold stopped. A program can let a step send without asking with `:send-without-asking` (`(act "…" "https://forms.example.org/x" :send-without-asking)`): then the hold is lifted and Claude isn't offered `confirm`. Besides `:uses` and `:replay` ([Replay without Claude](#replay-without-claude)), any other option is invalid.

**Your details** (roadmap 9.8). A person's name, email, phone, address and date of birth (`ProfileFields`) are kept in the keychain, one item each (`StoreProfile`, `ProfileKey`; base64url-encoded, since the keychain keeps one word of printable ASCII), and edited on drawdown's Connectors page. A step may use only the details its program names: `(act "Send the contact form as me" "https://forms.example.org/contact" :uses "name email")`. Only those are looked up (`Act.Profile`, a worker's `ProfileFrom(vault)`), before the browser starts; one that isn't set stops the step there, saying which. The browser gets each under its placeholder (`⟨name⟩`, `⟨email⟩`) through reed's `UseProfile`, and Claude is told the placeholders, never the values: it types `⟨email⟩` as a field's value and the browser types the address, and wherever the value would show in what the browser answers, Claude sees the placeholder. Playwright MCP hides only a value's exact copy, and a page can show one encoded or reformatted, so act hides them itself too, in every tool result before Claude sees it, in every beat and in the record: each value, ignoring case, as itself; URL-, form-, JSON-, HTML- and base64-encoded; and as its letters and digits with up to three other characters between each (spaced out, punctuated, a phone number split up), each replaced by its placeholder. So the recorded inputs keep the placeholder, the record names the details it could use (`uses`), and every frame of such a step is taken with reed's `ShotJSFor`, which masks them; if that fails, there's no frame rather than an unmasked one. No connector may name a detail's keychain item (`IsProfileName`), so a detail only ever reaches an act step that names it; `heedle secret delete --all` forgets details with every other heedle secret. A placeholder works as a text field's whole value; a dropdown, or a detail split across fields, is chosen by Claude like any other value.

**Once, with a budget.** A retried act step could submit a form twice, so it has one attempt (`ActOptions.MaxAttempts`). It has 10 minutes (`ActBudget`); its activity timeout is that plus five waits for a person (`MaxConfirmations` × `ConfirmTimeout`) and 5 minutes more, so the budget, not Temporal, ends a long step, with its record. Its heartbeats are `ActBeat`s: a `reed.Frame` (the latest screenshot, as browse's are, so drawdown shows it live) and the send waiting for a person, if any.

`ActHosts` gives the hosts an act step's arguments allow, as reed's allowlist reads them; loom and drawdown disclose them before a step runs.

### Replay without Claude

[[/images/replay.svg|alt=An act step finds the last successful run's script, checks its question is the same, then before each recorded action reads the page and checks its outline is the one recorded; if so it does the action by role and name through the allowlist, password check and hold, so a send still asks you; on the first page that differs Claude carries on, told where and why; the record keeps every action, where it handed over, and its own script]]

A form that went through once is done again without Claude (roadmap 9.9). Every act step's record keeps its actions as a **script** (`script`): each action that ran, other than reading the page, with each element it targeted as a Playwright locator by role and name (`getByRole("textbox", { name: "Email", exact: true })`, and which of the page's elements with that role and name it is), never the page's ref, which changes between runs; the page's outline before it (Playwright MCP's snapshot, refs and focus left out: its SHA-256, and its text, up to 8 KiB, for saying where a page differs); and each confirmation with its summary. The person's details appear only as their placeholders: the script is hidden like the rest of the record. A step whose element couldn't be named by role and name keeps no script, since one with a gap would replay as if it were all of it. `scriptInputs` is the SHA-256 of the step's question (instruction, start page, hosts, inputs, details and options).

The next run of the same program looks for it (`Act.Script`; a worker's is loom's `Scripts`, which finds the last successful run of the same program, [loom → Replay](https://github.com/heedle-ai/loom/wiki/How-It-Works#replay-without-claude)). If the question is the same, it **replays** the script: before each action it reads the page, and does the action only if the outline is the one recorded, finding each element again by role and name. The action goes through the same path as any of Claude's: the allowlist, the password check, reed's hold and every confirmation, so a replayed send still asks the person, a no ends the step, and a yes opens the hold for the one click, exactly as for Claude. A replay never acts on a page asking for a card: one whose snapshot (which Playwright reads from every frame, another origin's too, and a page's script can't change) names a field for a card number, a credit or debit card, a card security code, CVC or CVV, or where the browser finds, in the page or a frame it can see, a field whose autocomplete says `cc-…` or named for a card number or security code. In a step that sends without asking, the replay stops there for the person (`ended: "stopped"`) rather than hand the page to Claude, and on such a page Claude may put nothing into a request: every action is refused but reading the page, going back, or opening the step's start page exactly (any other URL could carry a card's details, in its query, fragment, path or user info), since a page can send on any input: a step that sends without asking never fills in, chooses on, clicks on or opens anything from a page asking for a card. What a page does by itself, on its own events, is the page's. A step that replays the whole script asks Claude nothing: its tokens are 0, its turns 0, and its answer says which run's script it replayed.

The check is of the whole page, before each action, so it stops at the first action on a page that differs, which for a renamed field is the first action on its page, not necessarily the one that fills it; the record names the element that's gone when it's the one that action is on (the page has no `getByRole("textbox", { name: "Email", exact: true })` any more), else the first line that differs. There it **hands the rest to Claude**, telling it where it stopped and why, what was already done in the browser, and the page as it is now, all as data in which no tag can open or close, and Claude carries on. A send the person doesn't confirm ends a replay there as it ends Claude's, with nothing more done, and the record says where. A question that differs (an earlier step answered differently, so the inputs aren't the same), a run whose step didn't finish or kept no script, or a lookup that failed, asks Claude from the start. The record says what happened (`replay`): the run whose script it was and its record, how many of its actions were replayed, and, when it handed over, at which action and why; each replayed call is marked `replayed`. Its own script is the replayed actions and Claude's, for the next run. `:replay "off"` in the program (`(act "…" "https://forms.example.org/x" :replay "off")`) has Claude do it all, every time.

### Take the wheel

[[/images/wheel.svg|alt=Take the wheel: Claude acts until it can't go on, calls ask_person with a reason, and the step pauses with its budget; heedle tells you it needs you; you drive the live view, your clicks and keys going to the browser as DevTools input only while it's paused for you and only from this machine, while the hold lets what you send through as sent by you; the frames you see while driving are never stored, and every frame masks password fields and your details; what you typed reads as typed by you everywhere; hand back closes the hold and gives Claude a fresh outline and a note that a person acted; Claude carries on on the page you left it]]

When an act step meets a sign-in, a code, a CAPTCHA, a check that it's human, or a choice it can't make, Claude calls `ask_person(reason)` (roadmap 9.10): it never tries to get around a CAPTCHA itself. The step pauses, its budget standing still, its beat saying why (`ActBeat.Wheel`: a random ID, the step, Claude's reason and the page), so loom's watcher and the run page tell you heedle needs you. A step asks at most `MaxHandOvers` (3) times, with a reason; with no one to ask here (`Act.Wheels` nil) Claude is told to stop and say what you need to do; with no answer in 30 minutes (`ConfirmTimeout`) Claude is told no one came.

While it waits, and only then, you **drive** its browser: the step is in the process's `Wheels`, which the UI sharing the worker's process (drawdown's, so `heedle up`'s) drives. `Wheel.Click` and `Wheel.Key` send one click or one key at a time as DevTools input events through Playwright's own mouse and keyboard (reed's `ClickJS` and `KeyJS`), and `Wheel.Frame` is the browser as it is now, for you to see: a fresh frame whenever the last is more than half a second old, so a page that moves on after a click shows without another key. The page's URL is hidden as the record's are (below) wherever it shows, in the beat and the live view too. Your input never goes through Temporal: no signal, query or heartbeat carries it, and the frames you see while driving are never beats or records. Once you hand back, or the step stops waiting, the wheel takes no more input (`ErrNotWaiting`).

**Sends while you drive** are yours: each click of yours, Enter (except in a textarea or contenteditable) and Space (except in a field that takes text) opens reed's hold for `PersonWindow` (3 seconds) from it (a character typed opens nothing, so a page sending on each key is held), and what the browser sends then goes through and the record lists it (`sentByYou`). What the page sends on its own later in the pause is held, as it would be with Claude driving. When you hand back, the hold closes at once (`PersonDoneJS`); if that can't run (a dialog left open), Claude waits out `PersonWindow` before acting again, so the hold is on by then anyway.

**What you type is never recorded.** The wheel keeps what you type, in the worker's memory only: the value of each field you type into, read before and after each key (so a typo fixed with an arrow key or a click is hidden as the whole value), or, for a field that can't be read, the keys typed; and at hand-back the values of every field you typed into and every password field (reed's `TypedJS`). Longest first, each is hidden as `⟨typed by you⟩` wherever it would show from then on: in every page outline, tool result and screenshot's URL Claude is given, in every beat, and in the record. A run of four characters or more is hidden everywhere, as a detail is, encoded or spelled out; a shorter one where it's a field's whole value. Every frame, recorded or live, masks password fields and every field you typed into, and what you typed, if four characters or more and not in a password field when read, wherever the page shows it as text; the matching runs where the page's own script can't see it and writes nothing to the page, and its boxes are painted over the screenshot here (reed's `ShotJSFor` and `ShotOf`). A dialog the page opens can't be answered from the live view: hand back and Claude answers it.

**Hand back** (`Wheel.HandBack`) closes the hold and answers Claude's call: a person acted in this browser, what you sent yourself, and the page as you left it, as data no tag can open or close in; Claude carries on from there. A step a person acted in keeps no script ([Replay without Claude](#replay-without-claude)): a replay can't redo what they did, and a script with that gap would replay as if it were all of it, so the next run of the program is Claude's again.

## heedle's labels

Every container pirn starts carries heedle's labels (roadmap 9.2), so heedle can list its containers, stop the run one belongs to, and clean up after a run, without ever touching anything else on the machine ([bobbin → heedle's labels](https://github.com/heedle-ai/bobbin/wiki/How-It-Works#heedles-labels-and-tidying-up)). pirn names each container's kind:

| Container | `heedle.kind` |
|---|---|
| An agent step's workspace | `agent` |
| A skill step's workspace (a question with a skill, as treadle asks) | `skill` |
| A local connector's MCP server, in a step or for a write | `connector` |
| An act step's browser | `act` (reed sets it) |

The run labels come from the step's context: the workflow that scheduled the step, its run, and the step's node ID. loom sets them where every step's activity starts. Otherwise the agent and act commands read them from Temporal's activity info with their own node ID, and `Agent.Run` (so `Step` and a skill's creation), and `RunWrite`, read the workflow and run without a step. A write runs in the run's own workflow after its steps, so its connector names the run itself. bobbin and reed write the same names, read the same values from Temporal, and follow the same rule for what a value may be (`TestInv_LabelSchemesAgree`).

Each also carries `heedle.home`, the home whose worker started it (roadmap 9.2.1), when the worker's context carries one: loom's worker puts it on the context all its activities start from, so pirn passes nothing and an agent's workspace, a skill's creation, a connector's server and an act step's browser all say it. pirn gives an act visit the home from bobbin's context, so reed labels the browser the same way (`TestInv_PirnContainersSayTheirHome`).

Temporal tells a step it was cancelled (`loom cancel`, or Stop run in the UI) only when the step heartbeats. So an agent step running in an activity heartbeats every 10 seconds while it runs, as well as after each command, and a cancel reaches it even while it waits on Claude. A step that has made no progress for 4 minutes stops heartbeating, so Temporal still notices a hung step (`TestInv_AgentStepsHeartbeatWhileTheyWait`).

## Laws

`ask` is a `weft.Arrow`, registered the same way as reed's `browse` and bobbin's `container`, so it composes under `pipe` and `par` exactly as they do. Two law tests check it with a deterministic stand-in for Claude: weft's identity laws (`TestLaw_AskIsAWeftArrow`), and that the question is a function of the step's inputs alone (`TestLaw_PromptIsAFunctionOfInputs`). Claude's answers themselves can differ between runs, so the laws hold up to the answer, which is why every answer is recorded with what it was given. An act step's replay is a fixed point of recording: on an unchanged page, replaying a script does exactly its actions, asks Claude nothing, and records the same script (`TestLaw_ReplayReproducesItsScript`, loom's Soundness law 22).

## Where it stops

- **Answers differ between runs.** That's the nature of the model; the record keeps each answer with the sources it was given.
- **Sources can try to steer Claude.** A page can contain text addressed to the model. The system prompt says to treat sources as data, and an ask step has no tools, so the most a page can do is distort the answer, which is recorded next to the page that did it.
- **One model per worker.** Choosing the model in the program is **Planned**.
- **An act step can do anything its hosts let a person do there.** The allowlist bounds where, not what: on an allowed host it can submit any form Claude is persuaded to, including by a page's text. That's why the hosts are disclosed before it runs, it runs once, and every action is recorded with a screenshot.
- **A page asking for a card is known by its fields.** A checkout that charges a saved card has no card field, and a security code asked for alone, with no card number anywhere on the page, isn't recognised: a step that sends without asking could pay there on a link or a page load. Leave `:send-without-asking` off for a site that keeps your card: each send then waits for your yes. And on a page that isn't asking for a card, a step that sends without asking can open any page on its hosts, so if Claude had a card's details (from the step's inputs) and a site charged by a plain link, it could pay: never give such a step a card's details.
- **A replay trusts the page it sees.** It checks the outline (what Playwright MCP's snapshot shows: roles, names, text and field values) before every action, not what the site does with a form: a site that changed only what happens behind an unchanged form is replayed as before. Every send still waits for the person, and the record keeps each action with its screenshot.
- **Only agent steps use connectors.** An `ask` step has no tools, and a skill step gets no grant yet.
- **The MCP connector isn't covered by zero data retention.** What a remote connector's tools return goes through Anthropic's API under its standard retention; use a local connector for data that mustn't.
- **Local connectors have no network.** A server that must call an API should be a remote connector.
