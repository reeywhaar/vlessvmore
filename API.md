# vlessvmore HTTP API

The management API. Everything the CLI does goes through these endpoints, so the two
cannot drift apart.

- Base path: `/api`
- Content type: `application/json` in both directions
- All timestamps are RFC3339 UTC
- All byte counts are integers

## Authentication

`Authorization: Bearer <secret>` on every endpoint except `GET /` and `GET /sub/{token}`.

```sh
TOKEN=$(docker exec vlessvmore vlessvmore token create web --raw)
curl -sH "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/api/users
```

Tokens come from `POST /api/tokens` and are compared against a stored SHA-256 hash. There
is no bootstrap credential to configure or forget to remove.

Requests over the container's unix socket (`/run/vlessvmore/manager.sock`) skip
authentication: reaching it already requires root inside the container, which is more
access than any token grants. That is how the first token gets minted, and how automation
can bootstrap without one:

```sh
TOKEN=$(docker exec vlessvmore vlessvmore token create ci --raw)
```

A missing or unrecognised token gets a plain `404`, byte-identical to the one an
unregistered path returns, and takes the same time to arrive. There is no `401` and no
`WWW-Authenticate` header. Revoked tokens, unknown tokens, wrong methods and paths that
never existed are all the same answer, on purpose — see
[Refusals](#refusals) below.

[`GET /backup`](#the-backup-port) is on a separate listener and is unauthenticated for the
same reason the socket is: it is not exposed. Do not publish that port.

## Errors

Non-2xx responses are `{"error": "<message>"}`.

| status | meaning |
| --- | --- |
| `400` | bad input: malformed JSON, unknown field, invalid UUID, negative quota |
| `404` | no such user or token — *or* no valid credential; see [Refusals](#refusals) |
| `409` | name or UUID already taken |
| `500` | something failed on our side: disk, database, config generation |

Note the absent rows. There is no `401` and no `405`: both would confirm that a path
exists.

Unknown JSON fields are rejected rather than ignored, so a typo fails loudly instead of
silently doing nothing.

## Mutations and reloads

Changing a user rewrites sing-box's config and reloads it. Because that is a second
step which can fail on its own, mutation responses wrap the result:

```json
{
  "result":   { "id": "u_0B4X…", "name": "alice", … },
  "reloaded": true
}
```

If the reload failed, `reloaded` is `false` and `reload_error` explains why. **The
status code is still 2xx**: the change is saved and will take effect on the next
successful reload. Only the reload did not happen.

Reloads within about a second of each other are coalesced into one, because each reload
drops established connections.

---

## Users

A `{id}` path parameter accepts either the internal id (`u_0B4X…`) or the display name,
matched case-insensitively. `user show alice` and `user show u_0B4X…` are the same call.

### `GET /api/users`

| query | effect |
| --- | --- |
| `include=usage` | add the `usage` object to each user |

```json
{
  "users": [
    {
      "id": "u_0B4X6TWQ8ZKM3N1PVJHR5DGYAC",
      "name": "alice",
      "uuid": "268e4039-6dd0-4d35-b279-b97639d9eed3",
      "enabled": true,
      "quota_bytes": 107374182400,
      "expires_at": "2026-08-25T02:46:57Z",
      "usage_reset_at": "2026-07-26T02:46:57Z",
      "note": "phone",
      "created_at": "2026-07-26T02:46:57Z",
      "updated_at": "2026-07-26T02:46:57Z"
    }
  ]
}
```

`quota_bytes: 0` means unlimited. `expires_at` absent means never. `disabled_reason` is
present only when enforcement turned the user off, and is `"quota"` or `"expired"`.

`sub_token` is the path segment of the user's subscription URL. `subscription_url` is
that URL assembled, and `install_url` is the illustrated setup page on the same token.
All three are credentials.

### `GET /api/users/{id}`

One user, always including `usage`.

```json
{
  "id": "u_0B4X…",
  "name": "alice",
  "usage": {
    "up": 1048576,
    "down": 4194304,
    "total": 5242880,
    "window_up": 1048576,
    "window_down": 4194304,
    "window_total": 5242880,
    "quota_bytes": 107374182400,
    "quota_remaining": 107368939520
  }
}
```

`total` is lifetime; `window_*` counts only since `usage_reset_at`, which is what the
quota is measured against. `quota_remaining` is `0` when unlimited.

### `POST /api/users`

```json
{
  "name": "alice",
  "uuid": "268e4039-…",
  "quota_bytes": 107374182400,
  "expires_at": "2026-12-31T00:00:00Z",
  "enabled": true,
  "note": "phone"
}
```

Only `name` is required. `uuid` is generated when omitted. `201` on success.

### `PATCH /api/users/{id}`

Accepts `name`, `uuid`, `enabled`, `quota_bytes`, `expires_at`, `note`. Absent fields
are left alone.

`expires_at` is three-valued, and the distinction matters:

| body | effect |
| --- | --- |
| field absent | expiry unchanged |
| `"expires_at": "2026-12-31T00:00:00Z"` | set it |
| `"expires_at": null` | remove it |

Renaming keeps the internal id, so **usage history survives a rename** — sing-box is
told the id, never the display name.

Changing `uuid` invalidates the user's existing client configuration.

### `DELETE /api/users/{id}`

Deletes the user and their usage history. Not reversible.

```json
{ "result": { "deleted": "u_0B4X…", "name": "alice" }, "reloaded": true }
```

### `POST /api/users/{id}/reset-usage`

Starts a new quota window at now, and re-enables the user if they were disabled *for
quota*. History is not deleted — the traffic still happened, it just stops counting
against the current quota. A user disabled by hand stays disabled.

### `GET /api/users/{id}/usage`

| query | default | notes |
| --- | --- | --- |
| `from` | 7 days ago | RFC3339, `YYYY-MM-DD`, or a unix timestamp |
| `to` | now | same |
| `bucket` | `hour` | `hour` or `day` |

```json
{
  "user_id": "u_0B4X…",
  "name": "alice",
  "from": "2026-07-19T00:00:00Z",
  "to": "2026-07-26T00:00:00Z",
  "bucket": "day",
  "series": [
    { "bucket": "2026-07-25T00:00:00Z", "up": 1048576, "down": 4194304 }
  ],
  "summary": { "up": 1048576, "down": 4194304, "total": 5242880, … }
}
```

Empty intervals are **omitted, not zero-filled** — a caller drawing a graph knows the
range it asked for. Traffic is recorded in whole UTC hours, so a range starting
mid-hour includes that whole hour.

### `GET /api/users/{id}/link`

The `vless://` URI, both user-facing URLs, and a QR bit matrix for each of the two things
worth scanning.

| query | effect |
| --- | --- |
| `qr=false` | omit both matrices |

```json
{
  "user_id": "u_0B4X…",
  "name": "alice",
  "link": "vless://268e4039-…@vpn.example.com:8443?type=tcp&…#alice",
  "subscription_url": "https://vpn.example.com/sub/QK7M2X…",
  "install_url": "https://vpn.example.com/show/QK7M2X…",
  "qr": {
    "size": 57,
    "rows": ["101110100…", "100000101…"],
    "quiet_zone": 4
  },
  "subscription_qr": {
    "size": 33,
    "rows": ["111111101…", "100000101…"],
    "quiet_zone": 4
  }
}
```

`qr` encodes `link`, `subscription_qr` encodes `subscription_url`. Prefer showing the
second: a scanned subscription re-fetches, so it survives a key rotation or a changed
port, while a scanned link is frozen at the moment it was drawn. `subscription_qr` is
absent for a user with no subscription token.

`rows` has `size` entries, each a `size`-character string of `'0'` (light) and `'1'`
(dark), top row first. The matrix is always square.

Modules rather than a PNG, so a client can render at any scale in any colours — SVG,
canvas, table cells — which a fixed-size raster would prevent. **Add `quiet_zone`
modules of light margin around it**: without that margin many scanners will not lock
on, and the failure looks like a broken code rather than a missing border.

Minimal renderer:

```js
const { size, rows, quiet_zone: q } = data.qr;
const side = size + q * 2;
const svg = [`<svg viewBox="0 0 ${side} ${side}" shape-rendering="crispEdges">`,
             `<rect width="${side}" height="${side}" fill="#fff"/>`];
rows.forEach((row, y) => [...row].forEach((bit, x) => {
  if (bit === "1") svg.push(`<rect x="${x + q}" y="${y + q}" width="1" height="1"/>`);
}));
svg.push("</svg>");
```

### `POST /api/users/{id}/rotate-sub`

Issues a new subscription token, invalidating the old URL immediately. Returns the user.

The UUID is untouched, so an already-configured client keeps connecting — this cuts off a
leaked subscription URL without disconnecting anyone. No reload happens, because sing-box's
config does not depend on the subscription token.

---

## Subscriptions

### `GET /sub/{token}`

**Unauthenticated.** The token in the path is the credential: subscription clients cannot
send an `Authorization` header, so a 160-bit capability URL is the only workable design. It
is exactly as sensitive as the credential it returns.

Outside `/api` on purpose, so it is obvious this route is public. An unknown token gets
the same `404` as any other refusal, so poking at it reveals nothing about whether this is
a subscription server at all.

| query | default | effect |
| --- | --- | --- |
| `format=base64` | ✓ | base64 of the newline-separated URIs — the de-facto standard |
| `format=uri` | | the raw `vless://` URI, unencoded (`plain` is a synonym) |

Response headers are the reason to prefer a subscription over a pasted link — they are
what makes a client display remaining traffic and an expiry date in its own UI:

```
Subscription-Userinfo: upload=1048576; download=4194304; total=107374182400; expire=1787011200
Profile-Update-Interval: 24
Profile-Title: base64:YWxpY2U=
Cache-Control: no-store, no-cache, must-revalidate
```

| field | meaning |
| --- | --- |
| `upload` / `download` | bytes since the user's current quota window began |
| `total` | the quota in bytes; **omitted entirely when the user is unlimited** |
| `expire` | expiry as unix seconds; **omitted entirely when the user never expires** |

`total` and `expire` are **left out rather than sent as `0`**. The convention says zero
means unlimited, but clients do not implement it reliably — Hiddify given `total=0`
invents a ceiling of its own (~85.9 GiB, the leading digits of `MaxInt64`) and shows a
limit the server never set. Omitting the field gives it nothing to misread. An unlimited,
non-expiring user therefore gets just `upload=…; download=…`.

`Profile-Title` is the server's configured `name`, falling back to the user's own name
when unset — the same label as the `vless://` fragment, so a client shows one thing
however the profile was added. `Profile-Update-Interval` is in hours. `Cache-Control` is `no-store` because a cached
credential would outlive its revocation.

A **disabled or expired user still gets a 200** with their link and honest
`Subscription-Userinfo`. The credential is already theirs and will not work — they are
absent from sing-box's config — but the headers are what let a client say "quota
exhausted" instead of showing a bare error.

There is no rate limiting on this endpoint. Put it behind a reverse proxy if that matters
to you.

---

## Server

### `GET /api/server`

Everything a client needs to connect, and nothing secret.

```json
{
  "name": "Reey VPN",
  "host": "vpn.example.com",
  "port": 8443,
  "sni": "vpn.example.com",
  "public_key": "Z0PwFQzd7TTduOXwDLQ7XePJXrtv6O7THdYg6aRloAo",
  "short_id": "6048316bbc9ca90e",
  "flow": "xtls-rprx-vision",
  "fingerprint": "chrome",
  "handshake": "caddy-caddy-1:443"
}
```

`name` is `config.json`'s label for this server — what a client displays for the profile,
via the `vless://` fragment and the subscription's `Profile-Title`. **Omitted when unset**,
which is not the same as empty: with no name configured, clients fall back to showing the
user's own name.

`public_key` is derived from the private key on each request and is never stored, so the
two halves cannot drift apart. **The private key is never returned by any endpoint** — it
lives in `data/identity.json` and is managed with the `vlessvmore identity` CLI, not over
HTTP.

There is no `PATCH /api/server`: `config.json` is read-only operator input. To change the
host or port, edit it and restart.

### `GET /api/status`

```json
{
  "sing_box": {
    "running": true,
    "pid": 28,
    "started_at": "2026-07-26T02:45:35Z",
    "config_path": "/var/lib/vlessvmore/sing-box.json",
    "active_users": 2,
    "last_reload": "2026-07-26T02:46:58Z"
  },
  "sing_box_version": "sing-box version 1.13.14\n\nEnvironment: …\nTags: …,with_v2ray_api",
  "users": 3,
  "active_users": 2,
  "tokens": 1,
  "data_dir": "/var/lib/vlessvmore"
}
```

`active_users` counts those in the generated config: enabled, unexpired, under quota.
`sing_box.last_error` appears if the most recent config generation failed.

Check that `sing_box_version` mentions `with_v2ray_api`. Without it there are no
per-user counters, so usage stays at zero and quotas never trigger.

### `POST /api/reload`

Regenerate and reload. Returns the `sing_box` status object. `500` if the generated
config was rejected — in which case the running proxy is untouched.

---

## Tokens

### `POST /api/tokens`

```json
{ "label": "web-ui" }
```

```json
{
  "token": { "id": "t_0B4X…", "label": "web-ui", "created_at": "2026-07-26T02:50:00Z" },
  "secret": "MHDJWEZ5NQ7K2P8RTX4VYB6CFA3GS1WD"
}
```

**`secret` is shown here and never again.** Only its hash is stored, so a leaked
`tokens.json` cannot be replayed.

### `GET /api/tokens`

Lists tokens with `id`, `label`, `created_at`, `last_used_at` and `revoked_at`. Never
returns a secret. `last_used_at` is persisted at most once a minute per token, so it can
lag by that much.

### `DELETE /api/tokens/{id}`

Accepts an id or a label. The token stops working immediately.

---

## Cross-origin requests

Off by default: with no `cors_origins` in config.json, no `Access-Control-*` header is ever
sent and a preflight is a `404` like anything else.

With it set, a preflight from a listed origin is answered:

```http
OPTIONS /api/users
Origin: https://dash.example.com
Access-Control-Request-Method: GET

204 No Content
Access-Control-Allow-Origin: https://dash.example.com
Access-Control-Allow-Methods: GET, POST, PATCH, DELETE
Access-Control-Allow-Headers: Authorization, Content-Type
Access-Control-Max-Age: 600
Vary: Origin
```

An origin that is not listed — and a request that sends no `Origin` at all — is passed
through untouched, which for `OPTIONS` means the usual refusal. That is deliberate: a
preflight has no `Authorization` header to check, so the `Origin` is the only thing
standing between an anonymous caller and a map of which paths exist. `["*"]` answers
everyone and gives that up.

### Writing `cors_origins`

Each entry is an **origin**, in exactly the form a browser puts in the `Origin` header:
scheme, host, and the port only when it is not the scheme's default. That is the
serialized origin of [RFC 6454 §6.2](https://www.rfc-editor.org/rfc/rfc6454#section-6.2),
the same value that comes back in `Access-Control-Allow-Origin`. No path, no trailing
slash, no wildcards inside a host.

| entry | matches | note |
| --- | --- | --- |
| `https://dash.example.com` | `https://dash.example.com` | the usual case |
| `https://dash.example.com:8443` | `https://dash.example.com:8443` | a non-default port is part of the origin |
| `http://localhost:5173` | `http://localhost:5173` | a dev server; `http` and `https` are different origins |
| `*` | anything | alone in the list, or beside others it makes them redundant |

Case and a trailing slash are cleaned up for you, as is a redundant default port —
`https://Dash.Example.com:443/` is stored as `https://dash.example.com`. That last one
matters: a browser on `https://dash.example.com` sends exactly that and never `:443`, so
without the fixup the entry would be valid, look right, and never match.

These are rejected at startup rather than silently never matching:

| entry | why |
| --- | --- |
| `dash.example.com` | no scheme; an `Origin` always has one |
| `https://dash.example.com/app` | a path is never part of an origin |
| `https://*.example.com` | subdomain wildcards are not a thing in CORS |

Sub-domains each need their own entry. If that list would be long, the honest options are
to front them with one origin or to use `*` and accept what it costs.

Credentials are not involved: the API authenticates with a bearer token, so
`Access-Control-Allow-Credentials` is never sent and `fetch` must not use
`credentials: "include"`. Send the token in the `Authorization` header as usual.

CORS says who may *ask*. It does not bypass the token — an allowed origin with no bearer
token still gets a `404`.

## Refusals

Every way this server says no produces the same answer:

```
HTTP/1.1 404 Not Found
Content-Type: text/plain; charset=utf-8

404 page not found
```

That covers a path that does not exist, a path that does but with the wrong method, a
request with no bearer token, one with a revoked or invalid token, and an unknown
subscription token. The body is Go's stdlib 404 verbatim.

Refusals on the TCP listener are also padded to a fixed delay of roughly 60–150 ms,
measured from when the request arrived rather than added to the work. Response time
therefore carries no information about *why* the request was refused — without it, "this
subscription token exists" is measurably slower to reject than "this path was never
registered", and enough samples recover the difference.

Refusals over the unix socket are not padded: the CLI hits it constantly and nobody can
reach it without already being root in the container.

The cost of this is that a legitimate caller with a stale token sees `404` rather than a
message telling them to renew it. Check `vlessvmore token list`.

## Unauthenticated endpoints

### `GET /healthz`

```json
{ "ok": true }
```

**Socket only.** Not served on the TCP listener, where it would answer strangers with JSON
no static site serves. The container's `HEALTHCHECK` runs `vlessvmore status`, which goes
over the socket. If you need an external HTTP health check, point it at `GET /`.

### `GET /show/{token}`

**Unauthenticated**, on the same subscription token as `/sub/{token}`. An illustrated
setup page: install Hiddify, add the profile with one tap, connect. It picks a language
from `Accept-Language` and a device from `User-Agent`, and renders every other
combination hidden so the switches work with no round trip and no JavaScript needed to
land on the right one.

| query | effect |
| --- | --- |
| `lang=ru` | force the language instead of reading `Accept-Language` |
| `device=android` | force the device instead of reading `User-Agent` |

Either one overrides both the request headers *and* whatever the reader chose on an
earlier visit — the point is to hand someone a link when you already know what they read
and what they carry. A value this build does not ship falls back to guessing rather than
erroring, so a typo still yields a usable page.

Also shows the user their own traffic, quota and expiry, which is the same data
`Subscription-Userinfo` already carries.

`no-store`, and `Referrer-Policy: no-referrer` so the token never leaves in a header when
someone taps through to an app store.

An unknown token gets the same refusal as everything else.

### `GET /static/{name}?token={token}`

**Unauthenticated**, same token again. The page's CSS, JavaScript and screenshots.

Names are content-addressed (`app.36fc37b9.css`), so responses are
`private, max-age=31536000, immutable` with an `ETag`. Without a valid token — or with the
un-hashed name — this is a `404`, so the assets are not a second way to find out this host
is more than a static site.

### `GET /`

`200 OK` with the plain text body `OK`, and nothing that identifies the software.

This exists because Reality's handshake target must be a hostname serving a real TLS
site. When a reverse proxy fronts this service on that hostname, a visitor has to see
something unremarkable. Matches only the exact root — unknown paths still `404`.

### `GET /sub/{token}`

See [Subscriptions](#subscriptions) above.

## The backup port

`GET /backup` lives on a **listener of its own** — `backup_listen`, `:3000` by default —
and is the only route that listener serves. It is not reachable on `api_listen` at all,
and `api_listen`'s routes are not reachable on it.

```sh
curl -O -J http://vlessvmore:3000/backup
```

`200` with `Content-Type: application/gzip`, an accurate `Content-Length`, and
`Content-Disposition: attachment; filename="vlessvmore-<YYYYMMDD_HHMMSS>.tgz"`. The
archive mirrors the two directories a deployment mounts:

| member | what |
| --- | --- |
| `config/config.json` | the mounted config file, byte for byte, when it is readable |
| `data/identity.json` | the Reality keypair |
| `data/users.json` | users, quotas, subscription tokens |
| `data/tokens.json` | API token hashes |
| `data/stats.db` | traffic history, self-contained, no `-wal` or `-shm` |
| `data/sing-box.json` | the rendered sing-box config |

Every member is a regular file with mode `0600`. There are no directory entries, so
extracting cannot re-chmod directories that already exist, and files the deployment has
not written yet are absent rather than empty. Errors are the usual `{"error": …}`, and
anything other than `GET /backup` is a `404`.

The archive is assembled in memory before the response starts, so a `200` is never a
partial backup. `stats.db` is produced with `VACUUM INTO`, so it is a consistent database
with its write-ahead log folded in — which is why this is safe on a running service while
`tar`-ing the data directory from outside is not.

**Unauthenticated, so the port must never be published.** Publishing it hands the Reality
private key and every user UUID to anyone who can reach the host. It is meant for a
sibling container on a private network; `"backup_listen": ""` turns it off entirely.

Restore is an extract, not an endpoint — there is no `POST /restore`. Stop the service
first, because writing over a live data directory corrupts it:

```sh
docker compose down
tar xzf vlessvmore-20260729_031500.tgz -C /srv/vlessvmore
docker compose up -d
```

See [Backup and moving hosts](README.md#backup-and-moving-hosts) for driving this from a
backup tool, and [CLI.md](CLI.md#export--import) for `export`/`import`, which remain the
right tool for moving to a *different* host.
