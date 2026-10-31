# Other apps on the production server

hermitator's server (`ssh hermitator`) also runs two of the owner's apps, deployed from their own repos. They
depend on things hermitator could break without noticing; keep these true.

| App | Repo | Site (Caddy file) | Containers (Podman, host network) | Ports | Data |
|---|---|---|---|---|---|
| **Timothy**, philosophy tutor | `~/src/personal/philosophy-tutor` | `tutor.ocaho.com`, `turn.ocaho.com`, `ntfy.ocaho.com` (`/etc/caddy/conf.d/tutor.caddy`) | compose project `tutor`: `livekit`, `tutor`, `ntfy` | 127.0.0.1:8800 (app), 127.0.0.1:2586 (ntfy), 7880/tcp (LiveKit API), 7881/tcp, 7882/udp, 3478/udp, 5349/tcp (media, TURN: public) | `/srv/tutor` |
| **Alex**, math circle | `~/src/personal/math-circle-tutor` | `mathcircle.ocaho.com` (`/etc/caddy/conf.d/mathcircle.caddy`) | compose project `mathcircle`: `mathcircle` (uses Timothy's LiveKit and ntfy) | 127.0.0.1:8810 | `/srv/mathcircle` |

## Don't

- **Touch their Caddy files**, or rewrite `/etc/caddy/Caddyfile` without its `import /etc/caddy/conf.d/*.caddy`.
  `install.sh` already keeps the import. **Reload Caddy, never restart it** (a restart drops every site's
  connections, including live video calls).
- **Take their ports**, above. LiveKit's media ports must stay reachable from the internet: no nftables rule may
  drop inbound UDP 7882/3478 or TCP 7881/5349, and nothing may set the forward chain's policy to drop.
- **Install Docker, ufw or firewalld**, or prune Podman (`podman system prune`, `podman rm -a`): the apps' containers
  and images live there, and `podman-restart.service` brings them back after a reboot.
- **Reboot the server** without asking the owner: a session with kids may be running. To check:
  `ls -t /srv/tutor/sessions /srv/mathcircle/sessions | head`.
