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
bridge, and an address from `10.88.0.0/16`. Bridge ports are isolated from
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
   someone else goes to the account's fallback agent, if there is one.
   Anything else is ignored. `/link` and the other commands are only
   accepted from the account's owner.
3. The agent's actor collects messages for 1.5 seconds, so a photo and the
   caption typed after it make one turn, then starts the turn. Messages that
   arrive mid-turn wait for the next one.
4. The turn reacts ⏳, shows "typing…", wakes the sandbox, saves attachments
   into `/shared/inbox` (transcribing voice notes on the host), and submits a
   job running `hermitator-guest turn` with the request on stdin.
5. The guest runs the harness and translates its output into a small event
   stream: `started`, `text`, `tool`, `file`, `result`, `error`.
6. Files the agent queues with `whatsapp-send` are delivered while the turn is
   still running. When it ends, the reply is converted from Markdown to
   WhatsApp formatting and split into messages, and the reaction becomes ✅.

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

They are stored encrypted with XChaCha20-Poly1305 under the master key, with
the owner and name as associated data. They are decrypted per turn and
travel inside the job (a spool file that the guest deletes when it claims
it), never on a command line. pi's credential file is removed from the
sandbox before it is suspended, so it is not left in the upper layer or a
checkpoint image.

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
the reaper picks up running agents as usual. After a host reboot, every agent
is stopped or checkpointed and wakes on its next message.
