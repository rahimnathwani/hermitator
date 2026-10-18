# Ideas

Things worth building that are not built yet, with what is known about
each. Not a promise or an order.

## Plugins and extensions for every agent

Claude Code plugins and pi extensions are installed per agent today: each
lives in that agent's home directory, so five agents means five installs,
and five updates. Two ways to share them:

- **A per-user list.** The user names plugins and extensions once, in
  Settings; hermitator installs any that are missing into each agent at
  boot, as a spool job, and the agent can still add its own. Needs care
  with versions (pin, or follow the marketplace) and with plugins that ask
  for configuration.
- **Baked into the image.** Install them in `image/Containerfile`, for
  everyone on the server. Simple, and upgrades come with image upgrades,
  but per-agent settings can still shadow them the same way an agent's own
  Claude Code does, and every user gets the same set.

## Keep other people's messages out of view

A chat's "others" trigger can be always, when mentioned, or never; held
messages still reach the agent as context with the next turn. A fourth
choice, ignore, would drop them entirely, for groups where the agent
should only ever see its owner.
