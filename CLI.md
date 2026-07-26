# vlessvmore CLI

Complete command reference. See [GUIDE.md](GUIDE.md) for a step-by-step walkthrough,
[README.md](README.md) for the design, and [API.md](API.md) for the HTTP API.

## Running it

Inside the container, every command talks to the daemon over its unix socket, so no
token is needed:

```sh
docker exec vlessvmore vlessvmore user ls
```

Three names work as the entrypoint, because the image symlinks the first two to the
binary and it dispatches on `argv[0]`:

```sh
docker exec vlessvmore vlessvmore user add alice   # canonical
docker exec vlessvmore user add alice              # shorter
docker exec vlessvmore token ls
```

`init`, `render`, `export` and `import` need no daemon — they work directly on files, so
they can run in a throwaway container or against a stopped deployment.

## Commands at a glance

```sh
# Users
vlessvmore user add <name>          # create a user; prints their link and a QR code
vlessvmore user ls                  # table of everyone, with usage against quota
vlessvmore user show <name|id>      # one user in full, with their link and QR code
vlessvmore user set <name|id>       # change name, quota, expiry, note, enabled state
vlessvmore user rm <name|id>        # delete the user and their usage history (asks first)
vlessvmore user link <name|id>      # print just the vless:// URI, for piping
vlessvmore user sub <name|id>       # print the subscription URL — prefer this
vlessvmore user rotate-sub <name|id>    # invalidate a leaked subscription URL
vlessvmore user usage <name|id>     # traffic history, hourly or daily
vlessvmore user reset-usage <name|id>   # start a new quota window; un-disables a capped user

# API tokens
vlessvmore token create <label>     # mint a token; the secret is shown once only
vlessvmore token ls                 # labels, creation and last-used times, never secrets
vlessvmore token rm <id|label>      # revoke immediately

# Server identity
vlessvmore identity show            # the Reality public key clients need
vlessvmore identity set             # adopt or regenerate the keypair

# Service
vlessvmore status                   # is sing-box up, how many users, last reload
vlessvmore server show              # host, SNI and the Reality public key clients need
vlessvmore reload                   # regenerate the sing-box config and apply it
vlessvmore render                   # print the generated config without installing it
vlessvmore version                  # versions, including sing-box's build tags

# Setup, backup and moving hosts
vlessvmore init --host <hostname>   # generate a config.json on stdout
vlessvmore serve                    # run the daemon (the container's default command)
vlessvmore export [--all]           # dump users (and with --all, usage and tokens)
vlessvmore import                   # restore a dump into an empty data directory
```

## Conventions

**Addressing.** Anywhere `<name|id>` appears, either works: `user show alice` and
`user show u_0B4X6TWQ8ZKM3N1PVJHR5DGYAC` are the same call. Names match
case-insensitively.

**`--json`.** Most read commands take it, for scripting.

**Sizes.** `--quota` accepts `100GB`, `1.5T`, `500M`, `2048K` or a raw byte count. **`GB`
means 1024³ here**, not 10⁹ — that is what people mean when they type it, and quietly
giving 7% less than asked for would be the worse surprise. `KiB`/`MiB`/`GiB` are accepted
as synonyms. `0` means unlimited.

**Dates.** `--expires` accepts `2026-12-31` (midnight UTC that day), `2026-12-31 18:30`,
a full RFC3339 timestamp, or a relative duration: `30d`, `2w`, `1y`, `48h`.

**QR codes.** `user add` and `user show` draw a scannable code when writing to a
terminal, and stay quiet when piped or redirected. `--qr` / `--qr=false` overrides that
guess either way. The code encodes the **subscription URL**, not the static link, so a
scanned profile keeps working through a key rotation or a port change.

`user link` and `user sub` never draw one by default, so
`LINK=$(vlessvmore user link alice)` captures exactly the URI; pass `--qr` if you want
the code as well.

**Exit codes.** `0` on success, `1` on failure. A saved change whose sing-box reload
failed is still a success — the warning goes to stderr, and the change applies on the
next successful reload.

**stdout vs stderr.** Commands whose output is data — `init`, `render`, `export`,
`user link` — put only that data on stdout. Reports, warnings and hints go to stderr, so
redirects capture clean output.

---

## Users

### `user add <name>`

```sh
vlessvmore user add alice                               # unlimited, never expires
vlessvmore user add alice --quota 100GB --expires 30d   # limits are opt-in
vlessvmore user add bob --uuid 268e4039-6dd0-4d35-b279-b97639d9eed3 --note "laptop"
vlessvmore user add carol --disabled
```

| flag | default | notes |
| --- | --- | --- |
| `--quota` | unlimited | traffic ceiling, e.g. `100GB` |
| `--expires` | never | date, timestamp or duration |
| `--uuid` | generated | supply one to migrate an existing client |
| `--note` | — | free-form, shown in `user ls` |
| `--disabled` | off | create without enabling |
| `--qr` | auto | force the QR code on or off |
| `--json` | — | machine-readable output |

Prints the user's details, their subscription URL, their `vless://` link, and a QR code of
the subscription URL. Fails with an error if the name or UUID is already taken.

### `user ls`

```console
$ vlessvmore user ls
NAME   STATE             USED     QUOTA      EXPIRES     NOTE
alice  enabled           1.2 GB   100 GB     2026-08-25
bob    disabled:expired  0 B      unlimited  2020-01-01  laptop
carol  disabled          0 B      unlimited  never
```

`USED` counts the current quota window, not lifetime. `STATE` distinguishes a user you
disabled by hand (`disabled`) from one enforcement disabled (`disabled:quota`,
`disabled:expired`).

### `user show <name|id>`

Full detail, including lifetime usage and the current quota window, followed by the link
and a QR code.

### `user set <name|id>`

Only the flags you pass are changed.

```sh
vlessvmore user set alice --quota 200GB       # raise the cap
vlessvmore user set alice --quota 0           # make unlimited
vlessvmore user set alice --expires 90d       # extend
vlessvmore user set alice --expires ""        # remove the expiry entirely
vlessvmore user set alice --name alice2       # rename; usage history is kept
vlessvmore user set alice --disable           # revoke access, keep the account
vlessvmore user set alice --enable            # restore it
vlessvmore user set alice --note ""           # clear the note
vlessvmore user set alice --uuid <new-uuid>   # rotate the credential
```

Renaming is safe: sing-box is told the internal id, never the display name, so a rename
keeps the user's traffic history attached. Changing `--uuid` invalidates whatever
configuration that client already has.

`--enable` also clears `disabled_reason`, so a user re-enabled after a quota trip does
not keep reading as "disabled: quota".

### `user rm <name|id>`

Deletes the user **and their usage history**. Asks for confirmation; `--yes` skips it. A
non-interactive stdin answers no, so a script that forgets `--yes` fails safe rather
than hanging.

### `user sub <name|id>`

Prints the subscription URL and nothing else.

```sh
vlessvmore user sub alice
vlessvmore user sub alice --qr
```

**This is the thing to hand out.** A client that subscribes re-fetches periodically, so it
picks up a rotated server key, a changed port, or an exhausted quota on its own. The
response also carries traffic and expiry headers, which is what lets a client show quota
remaining in its own UI.

The URL is a capability: it needs no password, so anyone holding it can fetch the
credential. Treat it exactly as carefully as the link itself.

### `user rotate-sub <name|id>`

Issues a new subscription URL. The old one stops working immediately.

The user's UUID is untouched, so an already-configured client keeps connecting — this cuts
off a leaked URL without disconnecting anyone. They will need the new URL to keep
receiving updates.

### `user link <name|id>`

Prints the static `vless://` URI and nothing else.

```sh
vlessvmore user link alice                       # just the URI
vlessvmore user link alice --qr                  # URI plus a QR code
vlessvmore user link alice | qrencode -t ansiutf8    # or use your own renderer
```

Use this for a client with no subscription support, or a one-off. It is frozen at the
moment you copy it: a later key rotation or port change will not reach it.

### `user usage <name|id>`

```console
$ vlessvmore user usage alice --days 3
WHEN        UP       DOWN     TOTAL
2026-07-24  120 MB   980 MB   1.1 GB
2026-07-25  45 MB    310 MB   355 MB

since quota reset  1.4 GB
quota              100 GB (98.6 GB remaining)
lifetime           1.4 GB
```

| flag | default |
| --- | --- |
| `--days` | `7` |
| `--bucket` | `day` (or `hour`) |

Intervals with no traffic are omitted rather than shown as zero. Traffic is recorded in
whole UTC hours.

### `user reset-usage <name|id>`

Starts a new quota window at now, and re-enables the user if they were disabled **for
quota**. History is not deleted — the traffic still happened, it just stops counting
against the current quota. A user you disabled by hand stays disabled.

## Quotas and expiry

Quotas are checked every 30 seconds against collected traffic; expiry is checked every
minute as well, so it fires for an idle user who is generating no traffic at all.

Enforcement works by omission. A user over quota or past their expiry is marked
disabled, which removes them from the generated sing-box config — and *that* is what
stops them connecting. `disabled_reason` records which limit it was.

## API tokens

```sh
vlessvmore token create web-ui          # prints the secret, with details on stderr
TOKEN=$(vlessvmore token create ci --raw)   # prints only the secret
vlessvmore token ls
vlessvmore token rm web-ui              # by label or id
```

The secret is shown once at creation and never again; only its SHA-256 hash is stored,
so a leaked `tokens.json` cannot be replayed. Use the token with the HTTP API:

```sh
curl -sH "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/api/users | jq
```

There is no bootstrap credential to configure. Automation gets a token the same way you
do — `vlessvmore token create` over the container's unix socket, which needs no token
itself.

## Service

### `status`

```console
$ vlessvmore status
sing-box         running (pid 28)
uptime           4h12m
users            3 (2 active)
api tokens       1
data dir         /var/lib/vlessvmore
rendered config  /var/lib/vlessvmore/sing-box.json
last reload      2026-07-26T02:46:58Z
```

`active` means present in the generated config: enabled, unexpired, under quota. A
`last error` line appears if the most recent config generation failed.

### `server show`

Host, port, SNI, handshake target, Reality **public** key, short id, flow and
fingerprint — everything a client needs. The private key is never printed; the public
key is derived from it on demand.

### `reload`

Regenerates the sing-box config and applies it. Fails, leaving the running proxy
untouched, if the result would be invalid.

Reloading drops established connections and clients reconnect within about a second.
This is inherent to sing-box having no runtime user API, which is why reloads requested
close together are coalesced into one.

### `render`

Prints the sing-box config that the current `config.json` and user list would produce,
without installing it. Needs no daemon.

```sh
vlessvmore render | sing-box check -c /dev/stdin
```

### `version`

Versions for both binaries, including sing-box's build tags. Warns if `with_v2ray_api`
is missing — without it there are no per-user counters, so usage stays at zero and
quotas never fire.

## Server identity

The Reality keypair identifies this server to its clients. It is generated on first start
and stored in `identity.json` in the data directory, **not** in config.json — it is
produced, not chosen, and it must never change.

Back up the data directory and the keypair comes with it. Lose it and every client needs
a new link, which is why `serve` logs an error if it ever has to generate a keypair while
users already exist.

### `identity show`

```console
$ vlessvmore identity show
public key  Z0PwFQzd7TTduOXwDLQ7XePJXrtv6O7THdYg6aRloAo
short id    6048316bbc9ca90e
created     2026-07-26T02:45:35Z
```

`--show-private-key` includes the private half. Off by default so a routine `show` does
not put it into shell history, scrollback and CI logs.

### `identity set`

```sh
# adopt another deployment's identity, so its clients keep working
vlessvmore identity set --private-key <key> --short-id <id>

# rotate to a fresh one
vlessvmore identity set --regenerate
```

Asks for confirmation, since it invalidates links; `--yes` skips that. Run
`vlessvmore reload` or restart afterwards.

Supplying only `--private-key` keeps the current short id, so changing one thing does not
quietly change two.

**Rotation is a hard cutover.** sing-box accepts exactly one private key per inbound, so
there is no overlap window — every existing link stops working at once. Clients on a
subscription URL recover on their next poll, which is the main reason to prefer
subscriptions over pasted links.

## Setup and transfer

### `init --host <hostname>`

Generates a `config.json` on **stdout**; the summary and next steps go to stderr.

```sh
vlessvmore init --host vpn.example.com > config/config.json
vlessvmore init --host vpn.example.com --handshake caddy-caddy-1:443 \
  --name "Reey VPN" > config/config.json
```

`--name` is what clients display for the profile. Without it they fall back to the user's
own name, so Alice's VPN ends up called "alice".

| flag | default | notes |
| --- | --- | --- |
| `--host` | required | the hostname clients dial |
| `--sni` | `--host` | Reality `server_name` |
| `--handshake` | `<sni>:443` | the real TLS server traffic falls back to |
| `--port` | `8443` | the VLESS inbound |
| `--flow` | `xtls-rprx-vision` | `""` for plain VLESS |
| `--fingerprint` | `chrome` | uTLS hint for clients |
| `--name` | the user's own name | what clients display for this server |
| `--subscription-url-base` | `https://<host>` | origin clients fetch subscriptions from |

There is no `--force`: overwrite protection is your shell's job. Use `set -o noclobber`,
or write to a temp name first.

The output contains **no secrets** — no keypair, no token — so it is safe to commit or
template. The Reality keypair is generated on first start and stored in the data
directory; see `identity` below.

### `export` / `import`

```sh
vlessvmore export --all > backup.json    # users, usage and tokens
vlessvmore export > users.json           # users only
vlessvmore import < backup.json
vlessvmore import --file backup.json --force
```

| `export` flag | effect |
| --- | --- |
| `--usage` | include traffic history |
| `--tokens` | include API tokens (they belong to the old host) |
| `--all` | both |
| `--exclude-identity` | leave the Reality keypair out |

The **Reality keypair is included by default**, because a restore without it produces a
working server that none of the restored users can reach — a silent, baffling failure.
That is also why a dump is a secret: it holds the server's private key and every user's
UUID.

Safe to run on a live service, and the supported way to back up: copying `data/` by hand
can capture a torn `stats.db`, since it is SQLite in WAL mode.

`import` refuses a data directory that already has users unless given `--force`. Sections
absent from a dump are left alone — importing a dump with no identity keeps the
destination's own key, and a users-only export does not wipe usage history. Run `reload`
afterwards to apply the imported users.

### `serve`

Runs sing-box under a supervisor, serves the API, and collects traffic. This is the
container's default command; you should not normally need to run it by hand.

| flag | default |
| --- | --- |
| `--config` | `/etc/vlessvmore/config.json` |
| `--data-dir` | `/var/lib/vlessvmore` |
| `--listen-socket` | `/run/vlessvmore/manager.sock` |

## Environment

| variable | overrides |
| --- | --- |
| `VLESSVMORE_CONFIG` | path to `config.json` |
| `VLESSVMORE_DATA_DIR` | data directory |
| `VLESSVMORE_SOCKET` | CLI socket path |
| `VLESSVMORE_SINGBOX_BIN` | the `sing-box` executable |

Flags win over environment variables, which win over the defaults.
