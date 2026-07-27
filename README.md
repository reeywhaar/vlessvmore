# vlessvmore

A self-contained VLESS/Reality VPN server: [sing-box](https://github.com/SagerNet/sing-box)
plus a manager that owns the user list and exposes an HTTP API and a CLI.

Add a user with one command and get a subscription URL, a `vless://` link and a QR code
back. Optionally set traffic quotas and expiry dates and have them enforced
automatically. See who is using how much.

```console
$ docker exec vlessvmore vlessvmore user add alice
name               alice
state              enabled
quota              unlimited
expires            never
subscription       https://vpn.example.com/sub/QK7M2X…
install page       https://vpn.example.com/show/QK7M2X…

vless://8f1c…@vpn.example.com:8443?type=tcp&encryption=none&flow=xtls-rprx-vision&…#alice

  ▄▄▄▄▄▄▄  ▄▄ ▄ ▄▄▄▄ ▄▄▄▄▄▄▄
  █ ▄▄▄ █ ▀█▄▀▄█▀▄ ▀ █ ▄▄▄ █
  █ ███ █ █▄▀ ▄▀▄██▄ █ ███ █
  █▄▄▄▄▄█ ▄ ▀▄█ ▀▄▀▄ █▄▄▄▄▄█
   …
```

**Docs:** [step-by-step guide](GUIDE.md) · [CLI reference](CLI.md) · [HTTP API](API.md)

New here? [GUIDE.md](GUIDE.md) walks from a bare server to a working VPN on your phone,
including connecting with Hiddify.

## Why this exists

sing-box has no runtime user-management API — `experimental/v2rayapi` implements traffic
statistics only. Adding a user therefore means editing JSON and reloading. And official
sing-box builds omit the `with_v2ray_api` build tag entirely, so they cannot report
per-user traffic at all.

This image solves both: it ships a sing-box compiled **with** `with_v2ray_api`, and a
manager that generates sing-box's config from a user list you edit through an API.

## How it fits together

```
                            :8443  VLESS/Reality  ←── clients
                              │
  ┌───────────────────────────┴─────────────────────────────────┐
  │ container                                                   │
  │                                                             │
  │  config.json ──┐                                            │
  │  (yours, :ro)  ├──→ renders ──→ sing-box.json ──→ sing-box   │
  │  users.json ───┘      ▲                                     │
  │                       │                    v2ray_api gRPC   │
  │  manager ─────────────┘        ←──── per-user traffic ───────┤
  │    ├── :80              HTTP API (bearer token)              │
  │    └── unix socket      the CLI                              │
  └─────────────────────────────────────────────────────────────┘
```

Two config files exist and they are **different things**:

- **`config.json`** is yours: hostname, port, handshake target. Generated once by
  `vlessvmore init`, mounted read-only. It holds **no secrets**, so it is safe to commit
  or template.
- **`sing-box.json`** is generated, from `config.json` plus the identity and the current
  user list, on every start and every change. **Never edit it** — the next change
  overwrites it.

Everything secret and irreplaceable is in the data directory:

| file | what | why this format |
| --- | --- | --- |
| `identity.json` | the Reality keypair | generated on first start, never changes; **this is the file that must survive** |
| `users.json` | users, quotas, subscription tokens | small and rarely changes, so you can read or repair it in a text editor |
| `tokens.json` | API token hashes | same |
| `stats.db` | hourly traffic buckets | SQLite, the only data here that is high-volume and always read as an aggregate |

Mount that directory as a volume. Without it the keypair is regenerated whenever the
container is recreated, and every client stops connecting — `serve` logs an error if it
ever generates a keypair while users already exist, because that is exactly the mistake.

## Install

```sh
mkdir -p config data

# 1. Generate a config. Only the config goes to stdout; the summary and
#    next steps go to stderr.
docker run --rm ghcr.io/reeywhaar/vlessvmore \
  init --host vpn.example.com > config/config.json

# 2. Run it.
cp docker-compose.example.yml docker-compose.yml   # edit the hostname
docker compose up -d

# 3. Add a user. Prints a subscription URL, a link and a QR code.
docker exec vlessvmore vlessvmore user add alice
```

Then point DNS at the host with an **A record, not proxied**. Reality needs the TLS
handshake to reach this server directly; a CDN in front terminates TLS and the scheme
collapses.

[docker-compose.example.yml](docker-compose.example.yml) is wired for
caddy-docker-proxy, which is what gives the handshake hostname a real certificate — see
below. Without Caddy, drop its `networks` and `labels` blocks and publish the API on
loopback instead:

```yaml
    ports:
      - "8443:8443"
      - "127.0.0.1:8080:80"      # management API, never on 0.0.0.0
```

`init` has no `--force`: overwrite protection is your shell's job. Use
`set -o noclobber`, or write to a temp name first.

## The Reality handshake, and why Caddy helps

Reality forwards any connection that fails to authenticate to a **real** TLS server, so
a stranger sees a legitimate site rather than a proxy. That server must hold a valid
certificate for your SNI, or the disguise fails.

[caddy-docker-proxy](https://github.com/lucaslorentz/caddy-docker-proxy) makes this
almost free — it watches container labels and reconfigures Caddy live, certificates
included:

```yaml
    labels:
      caddy: vpn.example.com
      caddy.reverse_proxy: "{{upstreams 80}}"
      caddy.log.output: stdout
      caddy.log.format: json
```

with the Caddy container as the handshake target:

```sh
docker run --rm ghcr.io/reeywhaar/vlessvmore \
  init --host vpn.example.com --handshake caddy-caddy-1:443 > config/config.json
```

What each piece does:

- `caddy: vpn.example.com` — Caddy obtains and serves a genuine certificate for the
  hostname. **This is the load-bearing part**: that certificate is what Reality borrows.
- `--handshake caddy-caddy-1:443` — where sing-box sends non-authenticated traffic. Use
  your Caddy container's name on the shared network.
- `caddy.reverse_proxy: "{{upstreams 80}}"` — the hostname serves something real. A
  browser visiting it gets a plain `200 OK`, which is what an uninteresting web server
  looks like. Caddy dials the container over the shared network, so port 80 is never
  published to the host.

This does put the management API on that public hostname under `/api`, behind its bearer
token. To keep it off the internet entirely, drop the `caddy.reverse_proxy` label, use
`caddy.respond: '"OK" 200'` instead, and publish `127.0.0.1:8080:80`.

## Everyday use

```sh
docker exec vlessvmore vlessvmore user add alice        # unlimited, never expires
docker exec vlessvmore vlessvmore user ls
docker exec vlessvmore vlessvmore user sub alice        # subscription URL
docker exec vlessvmore vlessvmore user install alice    # setup page to send a person
docker exec vlessvmore vlessvmore user usage alice --days 30
docker exec vlessvmore vlessvmore status
```

Users are **unlimited and non-expiring by default**. Limits are opt-in, per user:

```sh
docker exec vlessvmore vlessvmore user add bob --quota 100GB --expires 30d
docker exec vlessvmore vlessvmore user set bob --quota 0        # back to unlimited
```

Hand out the **subscription URL**, not the raw link. Clients re-fetch it, so they pick up
a rotated key, a changed port or an exhausted quota on their own; a pasted `vless://`
link is frozen at the moment you copied it. The response also carries usage and expiry
headers, which is what makes a client display quota remaining in its own UI.

For a person rather than a program, send `user install` instead. It is a setup page on the
same token: install Hiddify, add the profile with one tap, connect — with screenshots, in
their language, for their phone, both guessed from the browser and switchable. It shows
them their own traffic and expiry too. Nothing to talk anyone through.

Full command reference, including quota and date formats, in **[CLI.md](CLI.md)**.
HTTP endpoints in **[API.md](API.md)**.

Where a quota or expiry *is* set, it is checked every 30 seconds — and expiry every
minute as well, so it fires for idle users too. Enforcement works by omission: an
over-quota or expired user is marked disabled, which removes them from the generated
config, which is what stops them connecting.

## Backup and moving hosts

**Do not just copy `data/` while the service runs** — `stats.db` is SQLite in WAL mode,
and a live file copy can capture a torn database. Use `export`, which is safe on a
running service and includes the Reality keypair:

```sh
docker exec vlessvmore vlessvmore export --all > backup.json
```

That one file is the whole deployment. To move hosts with **every client still working**:

```sh
# on the new host, after `docker compose up -d`
docker exec -i vlessvmore vlessvmore import --force < backup.json
docker exec vlessvmore vlessvmore reload
```

The keypair comes across, so clients keep connecting; anyone on a subscription URL needs
no action at all. `config.json` can be recreated with `init` — it holds nothing you
cannot retype.

A dump contains the server's private key and every user's UUID. Treat it as a secret.
Details in [CLI.md](CLI.md#export--import).

## config.json

```json
{
  "version": 1,
  "name": "Reey VPN",
  "host": "vpn.example.com",
  "port": 8443,
  "sni": "vpn.example.com",
  "handshake": { "server": "caddy-caddy-1", "server_port": 443 },
  "flow": "xtls-rprx-vision",
  "fingerprint": "chrome",
  "api_listen": ":80",
  "log_level": "info",
  "stats_interval": "30s"
}
```

| field | default | notes |
| --- | --- | --- |
| `name` | the user's own name | what clients display for this server |
| `host` | required | what clients dial |
| `port` | `8443` | the VLESS inbound |
| `sni` | `host` | Reality `server_name` |
| `handshake` | `<sni>:443` | the real TLS server traffic falls back to |
| `flow` | `xtls-rprx-vision` | `""` for plain VLESS without vision |
| `api_listen` | `:80` | management API bind address |
| `subscription_url_base` | `https://<host>` | origin clients fetch `/sub/<token>` from |
| `cors_origins` | unset | origins allowed to call `/api` from a browser; `["*"]` for any |
| `stats_interval` | `30s` | how often traffic is collected |
| `template` | unset | path to a sing-box template overriding the built-in one |

Setting `name` is worth doing: without it a client labels the profile with the user's own
name, so Alice's VPN is called "alice". It replaces the label in both places a client
reads — the `vless://` fragment and the subscription's `Profile-Title`.

`cors_origins` is off unless you set it, and worth understanding before you do. A CORS
preflight carries no `Authorization` header — browsers never send one — so every preflight
this server answers is answered to an unauthenticated stranger. Answering only for paths
that exist would tell that stranger which paths exist, which is precisely what the uniform
`404` above is for. So the `Origin` is the gate: an origin that is not listed gets the same
padded `404` as everything else, and only someone who already knows your dashboard's
hostname can learn anything. `["*"]` trades that away on purpose — it turns "whoever knows
where the dashboard lives can enumerate `/api`" into "anyone can". It is still no easier to
*use* the API without a token; it only stops being invisible.

**No key material appears here at all.** The Reality keypair is generated state, not
something you author, so it lives in `data/identity.json`; the public half is derived
from it on demand so the two cannot drift apart. `vlessvmore server show` prints it, and
`vlessvmore identity` manages it.

API tokens are not in this file either — they live in `data/tokens.json` as hashes. Mint
one with `vlessvmore token create`, which needs no existing credential because the CLI
reaches the daemon over its unix socket.

Unknown fields are rejected, so a misspelled `stats_intervall` fails at startup instead
of silently reverting to the default. Edits require a restart.

## Things worth knowing

**Reloads drop connections.** Changing users means rewriting sing-box's config and
sending `SIGHUP`, which recreates the instance and drops established TCP connections.
Clients reconnect in about a second. Changes made close together are coalesced into one
reload for exactly this reason.

**A bad config never reaches production.** Every generated config is validated with
`sing-box check` before being installed. On failure the running config is left alone and
the error goes back to whoever asked.

**Traffic is counted in whole hours.** A quota window starting mid-hour includes that
whole hour. History is kept 90 days.

**One interval of traffic can be lost.** Counters are drained on each poll, which makes
every reading a delta and removes any need to reason about counter resets. The cost is
that traffic between the last poll and a sing-box restart goes unrecorded.

**Verify with a sing-box client.** Reality behaviour differs subtly between cores; test
with something sing-box-based such as Hiddify rather than an Xray client.

**Every refusal looks the same.** Anything the HTTP API declines — a path that does not
exist, the wrong method on one that does, a missing or revoked bearer token, an unknown
subscription token — comes back as the same plain `404`, padded to a fixed ~60–150 ms so
the response time carries no information either. There is no `401`, no `405` and no
`WWW-Authenticate` header for anyone to walk the host with. The price is that a stale
token reads as `404` rather than as "renew me"; `vlessvmore token list` is where to look.
`GET /healthz` moved to the unix socket for the same reason — it answered strangers with
JSON no static site serves. The container's `HEALTHCHECK` already runs `vlessvmore
status`, which goes over the socket, so point any external HTTP check at `GET /` instead.

**Subscription URLs are capabilities.** Anyone holding one can fetch that user's
credential — no header, no password. They are exactly as sensitive as the link itself.
The setup page at `/show/<token>` and its assets at `/static/…?token=` ride the same
capability, and without it they are the same `404` as everything else. `user rotate-sub`
invalidates all of them without disconnecting the user, since their UUID is untouched.

**Rotating the server key is a hard cutover.** sing-box accepts exactly one private key
per inbound, so there is no overlap window: `identity set --regenerate` invalidates every
link at once. Clients on a subscription URL recover on their next poll, which is the main
reason to prefer subscriptions.

## Building from source

```sh
go build -o vlessvmore .
go test ./...
docker build -t vlessvmore:dev .
```

`ghcr.io/reeywhaar/vlessvmore:latest` is published by
[.github/workflows/publish.yml](.github/workflows/publish.yml) on every push to `main`,
for `linux/amd64` and `linux/arm64`, after `go vet` and the test suite pass. Only `latest`
is tagged, so the commit is embedded at build time instead — `vlessvmore version` reports
it.

The run reports to Telegram when it finishes, saying which half broke if it did. Set two
repository secrets to switch that on:

| secret | what |
| --- | --- |
| `TELEGRAM_TOKEN` | a bot token from [@BotFather](https://t.me/BotFather) |
| `TELEGRAM_TO` | the chat id to post to |

Both are optional. With either missing the notify steps skip themselves rather than
failing, so an unconfigured fork does not get a red run on every push.

The image builds sing-box itself, with only the build tags this deployment uses — see
`SINGBOX_TAGS` in the [Dockerfile](Dockerfile). Upstream's default set adds gvisor,
quic-go, tailscale, wireguard and the Anthropic and OpenAI SDKs, none of which the
generated config can reach; leaving them out takes sing-box from 55 MB to 26 MB and the
image from 118 MB to 78 MB. The build fails if `with_v2ray_api` is missing rather than
letting you discover it later from traffic stuck at zero. Confirm any image with:

```sh
docker run --rm --entrypoint sing-box vlessvmore:dev version
```

The sing-box stage pins an older Go on purpose — see the comment in the
[Dockerfile](Dockerfile). sing-box reaches into `crypto/tls` internals via
`//go:linkname`, and newer toolchains break that.

Both Go stages cross-compile from the build platform rather than running under emulation,
so a two-architecture build costs about the same as one. The build-tag check inspects the
binary instead of executing it, since cross-compiled output cannot run on the builder.

`private/` is a gitignored scratch directory for real configs, keys and deploy notes. Nothing sensitive belongs anywhere else in the tree, tests included: the vectors
here are the public ones from RFC 7748.

## Not implemented

Protocols other than VLESS, graceful (overlapping) key rotation, Prometheus metrics,
multi-node, rate limiting on the subscription endpoint.
