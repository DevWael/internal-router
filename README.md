# internal-router

One container stack that gives every service on a Tailscale network its own HTTPS name under a
private domain, such as `gym.internal` or `photos.internal`. You add a name in a web page or with
one CLI command, and the router answers DNS for it, issues a certificate for it, and forwards
requests to the VM or container behind it.

## The problem

A home lab or small team network on Tailscale usually ends up with services spread over several
machines: apps on a NAS, a VM or two, containers on a dev box. Getting to each one is clumsy:

- **Addresses are machine-level.** MagicDNS gives one name per machine
  (`nas.tailnet-name.ts.net`), not one per service, so you still type ports and remember which
  box runs what.
- **HTTPS is per machine too.** Tailscale's certificates cover the machine name. Two services on
  one machine can't each have their own HTTPS name, and a service that moves to another machine
  changes its address.
- **Many apps need HTTPS.** Passkeys, service workers, installable web apps and browser
  notifications don't work over plain `http://` on a LAN address.
- **The workarounds sprawl.** A DNS server on one machine, nginx on another, a script that
  re-issues certificates when a site file changes. It works until one of those machines is
  rebuilt or turned off, and then every name breaks at once.

internal-router puts all of that in one place with one job: names under your private domain go to
the router, and the router sends each one where you told it to.

## How it works

```
browser on the tailnet
   │  gym.internal?            Tailscale split DNS sends *.internal to the router
   ▼
router (Tailscale node "router")
   ├─ internal-router   DNS: every *.internal name → the router's tailnet IP
   │                    web UI, CLI and JSON API for the list of names
   ├─ Caddy             HTTPS on :443, one certificate per name, signed by your CA
   │                    forwards gym.internal → http://<target>
   └─ tailscale         joins the tailnet and reaches the targets
```

- The stack is three containers sharing one network namespace: `tailscale`, `caddy` and
  `internal-router`.
- `internal-router` keeps the names in `data/routes.json` and pushes the full config to Caddy's
  admin API, which only listens on `localhost` inside the stack. Caddy is never reconfigured by
  hand.
- Certificates come from Caddy's internal CA. Give it your own root (`ca/ca.crt`, `ca/ca.key`) and
  every device that already trusts that root trusts every name. Without one, Caddy creates its own
  root on first start in `caddy/data`.
- Requests reach the target with the original `Host` header (`gym.internal`), plus the usual
  `X-Forwarded-For`, `X-Forwarded-Proto` and `X-Forwarded-Host` headers.
- WebSockets and long uploads pass through.
- At idle the stack uses about 60-100 MB of RAM.

Wildcard certificates are not an option: browsers reject a `*.internal` certificate because the
wildcard would sit directly under a top-level domain. So each name gets its own certificate, issued
the first time it's requested.

## Requirements

- Docker with Compose, on a Linux host with `/dev/net/tun`.
- A Tailscale network where you can edit DNS settings.
- Optional: an existing root CA certificate and key that your devices trust.

## Setup

1. Make a folder for the stack, copy `compose.yaml` and `resolv.conf` into it, and create the
   folders it mounts:

   ```bash
   mkdir -p internal-router/{ca,data,run,tailscale,caddy/data,caddy/config}
   cd internal-router
   ```

2. Put your root CA in `ca/ca.crt` and `ca/ca.key`. To make a new one instead:

   ```bash
   openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 3650 \
     -subj "/CN=internal-router CA" -keyout ca/ca.key -out ca/ca.crt
   ```

   Caddy normally signs names with an intermediate it creates from your root. If your root has
   `pathlen:0` it can't sign an intermediate, so also set `CA_INTERMEDIATE_CERT=/ca/ca.crt` and
   `CA_INTERMEDIATE_KEY=/ca/ca.key` and Caddy signs with the root directly. Check with
   `openssl x509 -in ca/ca.crt -noout -ext basicConstraints`.

   Then install `ca/ca.crt` as a trusted root on each device that will open the names. If you'd
   rather let Caddy make its root, remove `CA_ROOT_CERT` and `CA_ROOT_KEY` from `compose.yaml`
   and install `caddy/data/caddy/pki/authorities/local/root.crt` after the first start.

3. The `router` container runs as UID 65532 and writes `data/routes.json`:

   ```bash
   sudo chown 65532:65532 data
   ```

4. Start it with a Tailscale auth key, or without one and open the login link from
   `docker compose logs tailscale`:

   ```bash
   TS_AUTHKEY=tskey-auth-... docker compose up -d
   ```

   Without a key, the Tailscale container gives up on a login link after 60 seconds and makes a
   new one, so approve the newest link. Once it has logged in, the login is kept in `tailscale/`.

5. In the Tailscale admin console, open **DNS**, add a nameserver with the router's tailnet IP,
   turn on **Restrict to domain** and enter `internal`. Devices on the tailnet now send every
   `*.internal` lookup to the router.

6. Open `https://router.internal`.

## Adding names

In the web UI at `https://router.internal`, enter a name, pick a target and save. The target list
shows the machines on your tailnet (read from the router's Tailscale through the socket in `run/`):
choose one, then the protocol and port. Pick **Custom URL…** to type any other address, such as a
LAN IP. Saving an existing name replaces its target. The button in the top right switches between
light and dark mode, and the browser remembers the choice.

From the CLI inside the stack:

```bash
docker compose exec router internal-router add gym http://100.64.0.12:3030
docker compose exec router internal-router add nas https://192.168.1.10 --insecure-tls
docker compose exec router internal-router list
docker compose exec router internal-router rm nas
```

The same binary works from any machine on the tailnet that trusts the CA:

```bash
go install github.com/DevWael/internal-router@latest
internal-router --server https://router.internal list
```

Or with `curl`:

```bash
curl -X PUT https://router.internal/api/routes/gym -d '{"target":"http://100.64.0.12:3030"}'
curl https://router.internal/api/routes
curl -X DELETE https://router.internal/api/routes/gym
```

Rules for names and targets:

- A name is one label: lowercase letters, digits and dashes. `router` is taken by the UI.
- A target is `http://host[:port]` or `https://host[:port]`, with no path.
- The host can be a tailnet IP, a MagicDNS name (`box.tailnet-name.ts.net`, resolved through
  `100.100.100.100` by the mounted `resolv.conf`), or a LAN address the router's host can reach.
- `--insecure-tls` (the checkbox in the UI) skips certificate checks on an HTTPS target. Use it
  only for targets with a self-signed certificate.

## Running on TrueNAS SCALE

`truenas.yaml` is the same stack with absolute paths, for **Apps → Discover Apps → ⋮ → Install via
YAML**. Replace `/mnt/POOL/apps/internal-router` with your dataset first. It adds:

- `x-portals`, which gives the app a **Web UI** button. TrueNAS reads it only when the app is
  created, so editing the YAML later does not change the button.
- `user: "568:568"` on the router container. Make the `data` folder owned by `568:568` (the `apps`
  user).

TrueNAS has no setting for a custom app's icon. You can set one by adding an `icon:` line under
`metadata:` in `/mnt/.ix-apps/app_configs/<app>/metadata.yaml` and running
`midclt call -j app.metadata.generate`, using
`https://cdn.jsdelivr.net/gh/DevWael/internal-router@main/web/icon.svg`. This is not a supported
TrueNAS feature, and re-creating the app removes it.

## Security notes

- Every device on your tailnet can open the UI and change names. There is no login on top of
  Tailscale. Use Tailscale ACLs to limit who can reach the router on ports 443 and 53.
- Write requests that carry an `Origin` header other than `https://router.internal` are refused,
  so another web page can't change names through your browser.
- `ca/ca.key` can sign a certificate for any name your devices will trust. Keep it readable only
  by root, and back it up somewhere private.
- The Tailscale container needs `NET_ADMIN` and `/dev/net/tun` so the stack can reach other
  tailnet nodes. The other two containers have no extra privileges.
- The router container reads the Tailscale socket in `run/` (mounted read-only) to list machines.
  It only calls the status endpoint, but the socket itself is the local Tailscale API, so keep
  `run/` out of reach of anything else.

## Settings

| Variable | Default | Meaning |
| --- | --- | --- |
| `ROUTER_DOMAIN` | `internal` | The private domain |
| `ROUTER_NAME` | `router` | Name of the UI, so the UI is at `router.internal` |
| `CA_ROOT_CERT`, `CA_ROOT_KEY` | empty | Root CA files, as paths inside the Caddy container |
| `CA_INTERMEDIATE_CERT`, `CA_INTERMEDIATE_KEY` | empty | Signing CA files. Set them to the root's files when the root has `pathlen:0` |
| `ROUTER_DATA` | `/data` | Where `routes.json` is kept |
| `ROUTER_DNS_LISTEN` | `:53` | DNS listen address |
| `ROUTER_DNS_INTERFACE` | `tailscale0` | Interface whose addresses DNS answers with |
| `ROUTER_DNS_IPV4`, `ROUTER_DNS_IPV6` | empty | Fixed addresses to answer with instead |
| `ROUTER_UI_LISTEN` | `127.0.0.1:8080` | UI and API, reached through Caddy |
| `CADDY_ADMIN` | `http://localhost:2019` | Caddy admin API |
| `ROUTER_URL` | `http://127.0.0.1:8080` | Router the CLI talks to (`--server` overrides it) |
| `ROUTER_TAILSCALE_SOCKET` | `/var/run/tailscale/tailscaled.sock` | Tailscale socket used for the machine list. Empty turns the list off |

## Development

```bash
go test ./...
sh test/run.sh
```

`test/run.sh` builds the image, starts it with Caddy, a throwaway CA and a test backend, and checks
HTTPS routing, the UI, the API, DNS answers, restarts and removal.
