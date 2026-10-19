# Architecture

This is how hermitator works, and why it is built the way it is. The
[README](../README.md) has the overview; this document is for people
changing the code or deciding whether to trust it.

## The pieces

One process on the host, `hermitator serve`, contains:

| Component | Package | Job |
|---|---|---|
| Gateway | `internal/gateway` | One whatsmeow client per linked WhatsApp account. Decrypts media, sends replies, reactions and typing indicators, and runs the QR and phone-number pairing flows. |
| Orchestrator | `internal/orchestrator` | Routes messages to agents, runs turns, delivers output, and suspends idle sandboxes. |
| Sandbox runtime | `internal/sandbox` | runsc, OverlayFS, network namespaces and nftables. |
| Panel | `internal/web` | Server-rendered pages, server-sent events and the terminal WebSocket. |
| Store | `internal/store` | SQLite in WAL mode. |

Inside every sandbox, `hermitator-guest` runs as PID 1. It serves the job
spool, runs turns by driving Claude Code or pi, and is also the
`whatsapp-send` command agents use to send files.

Everything hermitator keeps lives under `/var/lib/hermitator`:

```
hermitator.db            users, agents, accounts, routes, turns, audit log
whatsapp.db              whatsmeow's device keys
images/universal         -> universal-20261017.1
images/universal-*/      rootfs/ (read-only lower layer), ENV
tools/<hash>/            guest binary and support files, mounted at /opt/hermitator
agents/<id>/
  config.json            OCI bundle config
  upper/ work/ merged/   OverlayFS: the agent's own layer, and the mounted root
  shared/                /shared inside: inbox/, outbox/, outbox/sent/
  spool/                 /run/hermitator inside: jobs/, out/
  checkpoint/            memory image while checkpointed
  guest.log              PID 1's output
whisper/                 speech model
```

## Sandboxes

### States

An agent's sandbox is **stopped** (no processes), **running**, or
**checkpointed** (memory image on disk, no processes). The database mirrors
the state for display; the truth is `runsc state` plus whether a checkpoint
image exists, and hermitator reconciles the two at startup.

**Wake** is idempotent. If the sandbox is not running, hermitator makes sure
the network namespace has its address (runsc strips it every time it takes
the interface over), mounts the OverlayFS root if needed, and then either
restores the checkpoint or boots cold. A restore reuses `config.json`
verbatim, because the checkpoint was taken against those exact mounts. If a
restore fails (after a gVisor upgrade, say), the checkpoint is discarded and
the sandbox boots cold: files survive, memory does not.

**Suspend** runs `runsc ps`. If PID 1 is the only process, it kills the
sandbox; its state is already on disk. Otherwise it checkpoints. Before
either, it removes credential files from the sandbox.

The **reaper** runs every 15 seconds and suspends running agents that have
been idle for their timeout (five minutes by default), unless a turn, a
terminal or a login is holding them awake.

### Why there is a spool instead of `runsc exec`

The obvious way to run a turn is `runsc exec <agent> claude -p ...`. It does
not work for hermitator, for two reasons found while building it:

1. When gVisor restores a checkpoint, it kills every process that was
   started through `runsc exec`, and their descendants: anything an agent
   started during a turn would die at the next wake.
2. A process holding a host file descriptor cannot be checkpointed cleanly.

So nothing long-lived is started through exec. `hermitator-guest init` is
PID 1, and it watches a spool directory that is bind-mounted from the host:

```
jobs/<id>.json    job (argv, env, stdin) written by the host via rename
jobs/<id>.kill    request to SIGTERM the job's process group
out/<id>.out      stdout, appended as the job runs
out/<id>.err      stderr
out/<id>.exit     exit status, written once output is complete
```

The guest polls `jobs/` every 100 ms and the host polls `out/` every 50 ms.
Both sides only create, append to and rename regular files, which gVisor's
gofer handles in its default shared mode for bind mounts without either side
caching stale data. A job's stdout and stderr are pipes that PID 1 copies into
the spool files, not the files themselves: a background process inheriting a
host file would block restore once the host deleted it.

The only thing still started with exec is the web terminal, which is
interactive and is expected to end when the agent sleeps.

### Filesystem

The root is OverlayFS: a shared, read-only image below, the agent's own
directory above. Agents can install anything; it lands in their upper
layer. Images are pinned per agent by their dated name, because changing an
upper layer's lower layer underneath it gives undefined results. New agents
get the newest image.

runsc runs with `--overlay2=none`, so writes go straight to the upper layer
as ordinary files instead of into gVisor's own in-sandbox overlay.

The guest tools are mounted read-only from a content-addressed directory.
A sandbox keeps the bundle it booted with until its next cold boot, so a
checkpoint is always restored against exactly the binary that was running.
Unreferenced bundles are deleted at startup.

### Network

Each agent has a network namespace with a veth pair into the `hermitator0`
bridge, and an address from `10.231.0.0/16`. Bridge ports are isolated from
each other. An nftables table drops everything from the bridge to the host
except replies, everything from the bridge to private, loopback, link-local
and multicast ranges, and all new inbound connections to agents. Outbound
traffic to the internet is masqueraded. The table is rewritten at every
start.

## Turns

### From message to reply

1. The gateway decrypts an incoming message and hands it to the orchestrator,
   preserving order per account. Status updates, channels, hermitator's own
   echoes and anything more than 15 minutes old are dropped.
2. Routing: a linked chat goes to its agent. An unlinked direct message from
   the owner goes to the account's fallback agent, if there is one. Anything
   else is ignored. "The owner" means the linked account itself, when it is
   the owner's own number, or one of the owner's own numbers writing to it,
   when it is a number dedicated to agents. Only the owner can run
   commands. `/link CODE` is accepted from anyone, because a valid code
   proves its sender can see the agent's page, and the sender's number is
   then remembered as the owner's. Wrong codes are throttled per sender.
   Direct chats are keyed by phone number even when WhatsApp addresses them
   by LID, so routes keep matching.
3. Each linked chat says which messages start a turn, separately for the
   owner and for everyone else: all, mentions (an @mention of the account, a
   reply to it, or `@agent-name`) or none. A message that does not start a
   turn is held in the database, with any attachment saved straight into
   the agent's inbox on the host. Holding wakes nothing and calls no model.
   The next turn in that chat takes the held messages as a block of labelled
   context ahead of its own, marks them read, and releases them. A chat
   holds at most 200.
4. The agent's actor collects triggering messages for 1.5 seconds, so a
   photo and the caption typed after it make one turn, then starts the turn
   for that chat. Messages from another chat wait for the next turn. A
   plain message from the same chat that arrives while Claude Code is
   working joins the running turn instead (it gets 👀): the host writes it
   to the spool's `steer/<turn>` directory, and the guest, which keeps
   Claude Code's stdin open until its first result, feeds it in as another
   user message. If Claude Code still answers it after that result, the
   earlier answer goes out on its own. Whatever reached the spool too late
   to be fed in starts the next turn, so nothing is lost. pi agents queue
   such messages for the next turn.
5. The turn sends read receipts (on agent numbers only), reacts ⏳, shows
   "typing…", wakes the sandbox, saves attachments
   into `/shared/inbox` (transcribing voice notes on the host), and submits a
   job running `hermitator-guest turn` with the request on stdin.
6. The guest runs the harness and translates its output into a small event
   stream: `started`, `text`, `tool`, `file`, `result`, `error`.
7. Files the agent queues with `whatsapp-send` are delivered while the turn is
   still running. When it ends, the reply is converted from Markdown to
   WhatsApp formatting and split into messages, and the reaction becomes ✅.

Agents talk to the host through `/shared` only. Besides the outbox,
guest commands such as `hermitator-guest schedule` drop requests into
`/shared/requests`, which the host answers in a file beside each one; the
host keeps `/shared/tasks.json` current for `hermitator-guest tasks`. All
of it is read through `os.Root`, like the rest of `/shared`.

Between turns, the reaper's tick also looks at awake agents: it delivers
what background jobs queued (to the chat the agent last talked in, at most
20 messages an hour), answers their requests, and leaves the agent awake
while a `hermitator-guest hold` file in `/shared/holds` is fresh. A hold
refreshes its file every 30 seconds and lapses after three minutes
without, or after twelve hours.

`hermitator-guest ask QUESTION OPTION...` sends a single-choice WhatsApp
poll the same way. A vote carries only SHA-256 hashes of the chosen
options, so the host keeps each poll it sends (`polls` table) and maps the
hashes back to names; the vote then arrives like any message addressed to
the agent: `[Answer to your poll "Deploy?": Yes]`.

### Scheduled tasks

The host keeps the timetable (`tasks` table; cron parsing in
`internal/schedule`), so an agent with tasks still sleeps until one is
due. A task is a prompt that reaches the agent as a message in the chat it
was set up from, in its usual conversation; the reply is headed with the
task's name. Agents create them with `hermitator-guest schedule`, and the
owner is told about each one. Times are in the owner's time zone, which
is also the agent's `TZ`. A task may run at most every five minutes, an
agent may have 25, and a task missed while hermitator was down runs once
when it is back. If its chat is no longer linked to the agent, it pauses.

Before any turn starts, the owner's optional daily and monthly spending
limits are checked against the API-equivalent cost of their turns; a turn
that would start past one does not, and crossing 80% sends a warning.

Each agent is served by one goroutine (its actor) that owns the queue, the
running turn and the holds keeping it awake, so none of that needs locks.
Turns themselves run on their own goroutine and report back to the actor.

### Harnesses

**Claude Code** runs as

```
claude -p --input-format stream-json --output-format stream-json --verbose \
  --dangerously-skip-permissions --session-id <uuid> | --resume <uuid> \
  --append-system-prompt <hermitator's instructions>
```

with `CLAUDE_CODE_OAUTH_TOKEN` in its environment and `ANTHROPIC_API_KEY`
removed (it would take precedence over the subscription). The user message,
including images as base64 blocks, is one line of JSON on stdin. Skipping
permission prompts is safe because nobody is there to answer them and the
sandbox is the boundary. `IS_SANDBOX=1` tells Claude Code that this is
deliberate.

**pi** runs as `pi --mode rpc --session-id <uuid> --provider P --model M`.
The prompt goes in as an RPC command; hermitator waits for `agent_settled`
and cancels any extension dialog, since nobody can answer it.

Both get hermitator's addition to the system prompt, which tells them they
are talking over WhatsApp, where files arrive, how to send one back and that
background processes should log to a file.

The agent's model and effort go on the command line of every turn
(`--model` and `--effort` for Claude Code, `--model` and `--thinking` for
pi), so changing them needs no restart and the session simply continues;
`/once` overrides them for a single turn. Each turn records the model the
harness reports it ran on.

Session IDs are chosen by hermitator, so they are known before the first
turn, and survive switching between WhatsApp and the web console. `/new`
clears it; switching harness clears it too, since the transcripts are not
compatible.

## Credentials

| Credential | Comes from | Reaches the sandbox as |
|---|---|---|
| Claude subscription | `claude setup-token`, pasted into the panel | `CLAUDE_CODE_OAUTH_TOKEN` |
| ChatGPT subscription | Device-code login, run inside a sandbox by pi's own code | `~/.pi/agent/auth.json`, synced back when pi refreshes it |
| DeepSeek, OpenRouter | Pasted into the panel | `DEEPSEEK_API_KEY`, `OPENROUTER_API_KEY` |
| GitHub | `gh auth token` or a fine-grained token, checked against GitHub when saved | `GH_TOKEN` and `GITHUB_TOKEN`, plus a git credential helper set through `GIT_CONFIG_*`, so git over https uses it without it touching the disk |
| The user's own secrets | Name and value in the panel | An environment variable of that name; the agent's instructions list the names |

They are stored encrypted with XChaCha20-Poly1305 under the master key, with
the owner and name as associated data. They are decrypted per turn and
travel inside the job (a spool file that the guest deletes when it claims
it), never on a command line. pi's credential file is removed from the
sandbox before it is suspended, so it is not left in the upper layer or a
checkpoint image. Environment variables live only in the turn's processes,
though background processes started during a turn inherit them.

### Keeping credentials out (the egress proxy)

A user can turn on **Keep them out of the sandbox** in Settings. Their
agents then hold placeholders, derived from the master key per user and
credential so they stay the same across turns, restarts and checkpoints,
and shaped like the real thing (`sk-ant-oat01-hermitator-…`). nftables
redirects those agents' HTTPS to public addresses to the egress proxy on
the gateway (`internal/egress`). The proxy reads the server name from the
TLS ClientHello. For a host that one of the user's credentials is for
(`api.anthropic.com` for the Claude token, GitHub's hosts for the GitHub
token, the hosts a user gives their own secret), it decrypts with a
certificate from hermitator's own CA, swaps placeholders for real values
in headers, Basic credentials and the query, and sends the request on;
a placeholder bound for another host is refused with a 403. Every other
connection is passed through without being decrypted.

The proxy tells agents apart by source address, so a bridge-family rule
pins each agent's veth to its own address: an agent cannot claim
another's and get its credentials swapped in. It also refuses private
destinations, since redirected traffic skips the forward chain.

Sandboxes trust the CA through `NODE_EXTRA_CA_CERTS` and a CA bundle
(`SSL_CERT_FILE` and friends) in the tools bundle. A sandbox only holds
placeholders if the configuration it runs with, the one a restore reuses,
trusts the CA; turning the proxy on stops idle agents so they boot
afresh, and busy ones keep real credentials until they do.

pi's ChatGPT login is an OAuth file that pi reads the account ID from (the
access token is a JWT) and refreshes itself when it nears expiry. A
sandbox holding placeholders gets an unsigned JWT that decodes the same
way, a refresh placeholder that is refused everywhere, and an expiry in
2100, so pi never refreshes; the host refreshes the real token when it
has under six hours left, before turns and as the proxy looks it up.
When pi's file comes back after a turn or a terminal session, an entry
still holding the placeholder is replaced by the stored real one, so
only a fresh login from the terminal changes what is stored.

Every credential value of eight characters or more, and the long strings in
pi's credential file, is replaced with `[secret removed]` in what the agent
sends to WhatsApp (replies, messages, captions), in the stored turn, and in
the live activity view. Files it sends are not inspected. Commit name and
email travel as `GIT_AUTHOR_*` and `GIT_COMMITTER_*`.

The ChatGPT login runs `pi-login.mjs` inside one of the user's sandboxes. It
calls pi's own implementation of OpenAI's device-code flow, so hermitator
does not carry a copy of a provider's auth code. The web page shows the code
and URL as server-sent events.

## Trust boundaries

The agent is assumed hostile. It is root in its sandbox, holds its owner's
model credentials, and can write anything into `/shared` and the spool,
including symlinks pointing at host paths. So:

- The host touches those directories only through `os.Root`, which refuses
  any path that resolves outside them. Outbox files must be regular files;
  hermitator reads them itself and hands the bytes to the gateway.
- Everything the guest prints is parsed as data. The worst an agent can do
  through it is lie about its own turn, or replace its own owner's pi
  credentials.
- The web terminal's environment, which holds credentials, goes to runsc in
  a temporary 0600 process spec file rather than in its arguments.

Users are separated by ownership checks on every request (agents, accounts
and routes all belong to one user). Admins manage users but cannot own
agents or accounts; to act on someone's agents they impersonate them, which
puts a banner on every page, keeps admin pages closed until they stop, and
records both identities in the audit log. Sessions are random 256-bit tokens
stored as SHA-256 hashes; passwords are argon2id; logins are throttled per
address; cross-origin form posts are rejected.

## Restarts

Sandboxes are separate processes in their own cgroups, and runsc keeps its
state in `/run/hermitator`, which systemd preserves across service restarts.
So upgrading hermitator does not touch running agents. At startup it marks
turns that were in flight as interrupted, reconciles each agent's state, and
the reaper picks up running agents as usual. Each chat whose turn was cut
short is told so once WhatsApp reconnects, and `/retry` sends the turn's
whole prompt again (turns keep it for exactly this), prefaced with a note
to check what was already done. Nothing is retried automatically, since a
turn may have pushed code or sent something before it stopped. After a
host reboot, every agent is stopped or checkpointed and wakes on its next
message.

## Shared limits

Each sandbox has its own memory limit, and all of them sit under one
cgroup, `/hermitator`, which hermitator caps at startup: memory at the
host's total less a fifth (at least 1 GiB, at most 4 GiB kept back), and
CPU weight at half the default. Agents that are each within their limit
can then still not exhaust the host between them; if they try, the OOM
killer chooses among sandboxes, never hermitator, Caddy or sshd.
