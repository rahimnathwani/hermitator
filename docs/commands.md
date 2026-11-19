# Commands and chats

Everything you send in a linked chat goes to its agent, except these, which
hermitator answers itself. `/help` lists them in the chat, grouped the same
way, and `/help COMMAND` shows one.

## The agent

| Command | |
|---|---|
| `/status` | What the agent is doing, its state and disk use |
| `/stop`, `/stop all` | Abort the current turn; `all` also withdraws the messages waiting for one |
| `/retry` | Run the last message again, after a failure or a restart |
| `/plan` | The agent's own task list for what it is doing, without waking it |
| `/usage` | Claude subscription use (5-hour and weekly, with pace) and spending |
| `/sleep` | Suspend it now (alias `/exit`) |
| `/awake 2h`, `/awake off` | Keep the sandbox from sleeping (up to 12 hours), so a background job runs on |

## Without the model

| Command | |
|---|---|
| `/sh COMMAND` | Run a shell command in the sandbox and get its output |
| `/get PATH` | Send a file from the sandbox to this chat now; a folder comes zipped |
| `/git`, `/git PATH` | Repositories with uncommitted or unpushed work; one repository's changes and recent commits |
| `/md`, `/md pdf`, `/md file` | The last reply again as a picture (a PDF when it is long), for tables and wide output; reply to an older one with `/md`, or react 🖼 to it, to pick it |
| `/remember FACT` | Add a dated line to the agent's memory file, which it reads at the start of every conversation |
| `/preview PORT`, `/previews`, `/previews off` | A link to a web server the agent runs; list them; turn them all off |

## Later

| Command | |
|---|---|
| `/in 2h …`, `/at 9am …`, `/every 0 9 * * 1-5: …` | Schedule a message to the agent, without asking the model to |
| `/tasks`, `/tasks pause N`, `/tasks resume N`, `/tasks run N`, `/tasks delete N` | Scheduled tasks; `run` starts one now, outside its schedule |
| `/timezone Europe/London` | The time zone schedules use (also in Settings) |

## Conversation

| Command | |
|---|---|
| `/new` | Start a fresh conversation in this chat; files are kept (alias `/clear`) |
| `/compact` | Summarise this conversation now, so the next messages re-read less |
| `/sessions`, `/resume N` | Past conversations, and going back to one (`/new` keeps them) |
| `/fork` | Branch the next message off this conversation (Claude Code) |
| `/history on`, `/history off` | Keep this chat's messages, everyone's, in a file the agent can search |

## Model

| Command | |
|---|---|
| `/model`, `/model opus`, `/model default` | Show or change the model |
| `/effort`, `/effort high`, `/effort default` | Show or change how hard it thinks |
| `/once opus max: …` | Use another model or effort for this one message |
| `/switch claude` or `/switch pi` | Change harness, same workspace |
| `/upgrade`, `/upgrade all` | Move the agent, or all of yours, to the newest image; files are kept, background processes end, and it asks first |

Model and effort changes apply from the next message, and the conversation
carries on. Claude Code takes `fable`, `opus`, `sonnet`, `haiku` or a full
model ID, with effort from `low` to `max`; pi takes its provider's models,
with thinking from `off` to `max`. The agent's Settings tab has the same
choices, and its Overview shows which model each turn actually ran on.

## This chat

| Command | |
|---|---|
| `/listen` | Show or change which messages wake the agent in this chat |
| `/link CODE`, `/link NAME`, `/unlink` | Bind this chat to an agent (by name, for you), or unbind it |
| `/agents` | Your agents, and which one this chat reaches |
| `/help`, `/help COMMAND` | The list, or the lines about one command |

A message that is only a slash word hermitator does not know, or that
opens with one of Claude Code's or pi's own commands (`/cost`, `/mcp`),
gets an explanation instead of a turn: the harness would have read it as
plain text. A command sent as a reply to a message still counts as one.
Only you can use commands: messages from your own numbers
(listed in Settings, and learnt from `/link`), or sent by the linked
account itself when it is your own number.

## The server

An admin can make a user a **host operator** (on the Users page), who can
then look after the server with no SSH, from a chat linked to an agent, a
direct chat with the agents' number, or their own chat with themselves
(never one other people read):

| Command | |
|---|---|
| `/host` | Version, uptime, load, memory, disk, agents awake and working, WhatsApp numbers connected |
| `/host doctor` | `hermitator doctor`'s checks |
| `/host logs`, `/host logs 50` | The last warnings and errors from the journal, with every stored secret removed |
| `/host image`, `/host image update` | The agent image and its updates; build one now if there are new releases |
| `/host restart` | Restart hermitator, after asking; sandboxes keep running, and it says when it is back |

Host operators also hear when a new agent image is built or a build
fails, and when the disk runs low. The commands are fixed: nothing typed
after `/host` reaches a shell.

## Which messages wake the agent

Each linked chat has two settings: which of **your** messages wake the agent,
and which of **other people's** do. Each is *always*, *by mentioning it* or
*never*. Mentioning means an @mention of the agent's number, a reply to one
of its messages, or typing `@` and the agent's name. When you `/link` a
direct chat, or a group with just you and the agent's number, your messages
always wake it and other people's never do. In a group with other people,
you wake it by mentioning it. Change this on the agent's Chats tab or with
`/listen` (for example `/listen others all`).

A message that does not wake the agent wakes nothing at all: no sandbox, no
model call. hermitator keeps it, up to the last 200 per chat, and hands it
to the agent as context with the next message that does. So in a group with
a friend you can talk among yourselves and then ask *"@shop-frontend, can
you do what Sam suggested?"*. Commands from you always work, and never wake
the agent either.

Messages sent in quick succession, a photo and then its caption say, become
one turn. A message you send while a Claude Code agent is working ("actually,
make it blue") reaches it in the middle of what it is doing, and gets 👀;
with pi, it waits for the next turn, and gets 📥.

Anyone who is not in a chat you linked is ignored, so strangers who find the
agent's number get nothing. An account can also have a **fallback agent**,
which answers your direct messages to the number without a `/link`. It
listens only to your numbers, never to strangers or groups.

## Cost

Each turn's cost appears under Recent turns on the agent's Overview. It is
what the model would have charged at API prices: on a Claude or ChatGPT
subscription you are not billed per token, but it is a fair measure of how
much of your allowance a turn used.
