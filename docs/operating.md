# Running a server

What a host operator needs after the install in the README. How these
work inside is in [architecture.md](architecture.md).

## Agent images

The image pins exact versions of Claude Code, pi and Playwright, and
their auto-updaters are off, so a new release changes nothing in a
running agent until its owner chooses it. The server keeps the image
itself current: once a day it looks for releases that have been out for a
week, and when there are any, builds a new image with them (never older
than the pins in `image/Containerfile`), removes old images no agent uses,
and tells the host operators on WhatsApp what moved. New agents get it;
existing ones offer an upgrade, from `/upgrade` in their chat, one at a
time on their Settings tab or all at once from the agents page, and keep
all their files. `HERMITATOR_IMAGE_UPDATES=off` turns this off. From a
laptop, `make image-bump` still moves the repository's pins (review and
commit the diff) and `make image` deploys and builds them.

Before any agent uses a new image, the build checks it offline: Claude
Code and pi each run a turn the way hermitator drives them, against an
API address nothing listens on, and `claude setup-token` must still show
its sign-in link. A release that breaks any of that fails the build, the
current image stays current, and the host operators hear why.
`hermitator check-image TAG` runs the same check by hand.

Agents are told not to update Claude Code or pi themselves: a copy they
install lands in their own layer and hides every later image version. If
one does anyway, its Settings tab says so and can remove the copy.

## Plugins, skills and MCP servers

Claude Code plugins, pi extensions, skills and MCP servers need no image
change. They live in each agent's home directory and survive sleep and
image upgrades: install them from the agent's Terminal tab
(`claude plugin install …`, `pi install …`), or ask the agent to. They
take effect from the next message.

## Other apps on the same server

hermitator's Caddy site is its own file, `/etc/caddy/conf.d/hermitator.caddy`,
which the main Caddyfile imports along with anything else in that directory.
To serve another app from the same server, give it a file of its own there,
such as `/etc/caddy/conf.d/myapp.caddy`, and `systemctl reload caddy`;
reinstalling hermitator leaves it alone.
Two such apps run on the production server today; what they need from
hermitator is in [neighbours.md](neighbours.md).

## Disk

Agents write to the server's own disk, with no quota. hermitator watches
it every two minutes instead:

| | Default | When it is crossed |
|---|---|---|
| **One agent's files** (`HERMITATOR_AGENT_DISK_GB`) | 50 GiB | The agent is stopped and its owner told; its turns wait until it is back under, and `/sh` or its Terminal tab still wake it to clean up |
| **Free space** (`HERMITATOR_DISK_RESERVE_GB`) | the smaller of a tenth of the disk and 20 GiB | Every running agent is stopped and all turns wait until a quarter more is free; the host operators get the largest agents |

Waiting messages are kept, never dropped, and each chat is told once why
its message waits. `off` turns either limit off.

## Backups

```sh
hermitator backup /var/backups/hermitator
```

writes a new folder there with a consistent copy of both databases (users,
agents, chats, tasks and sealed credentials; WhatsApp's device keys) and
the egress CA, safe to run while the service is up. It leaves out the
master key, so keep `HERMITATOR_MASTER_KEY` somewhere else: without it the
credentials in a backup cannot be read. Agents' own files, under
`/var/lib/hermitator/agents`, are separate and much larger; copy them with
restic or borg, ideally while the agents sleep. Images, the tools bundle
and whisper's model are rebuilt by `deploy/install.sh`.

A daily run from cron or a systemd timer, with the folder then copied off
the server, covers a lost disk. To restore, stop the service, put the
files back in `/var/lib/hermitator` and start it.
