# Step-by-step guide

Getting from a bare server to a working VPN on your phone. Roughly 15 minutes.

- [1. What you need](#1-what-you-need)
- [2. Point a domain at the server](#2-point-a-domain-at-the-server)
- [3. Set up Caddy](#3-set-up-caddy)
- [4. Install vlessvmore](#4-install-vlessvmore)
- [5. Check it started](#5-check-it-started)
- [6. Add a user](#6-add-a-user)
- [7. Connect with Hiddify](#7-connect-with-hiddify)
- [8. Confirm it works](#8-confirm-it-works)
- [Day-to-day](#day-to-day)
- [If something is wrong](#if-something-is-wrong)

## 1. What you need

- A server with a public IP, outside whatever network you want to get around. Any small
  VPS is plenty — this is not CPU-heavy.
- A domain name you can add a DNS record to.
- Docker and the Compose plugin on the server.
- Root or sudo on the server.

Throughout, replace `vpn.example.com` with your own hostname.

## 2. Point a domain at the server

Add a single **A record**:

```
vpn.example.com.  A  203.0.113.10
```

**It must not be proxied.** If your DNS provider has an orange cloud, a "proxied"
toggle, or CDN in front of records, turn it off for this one. Reality works by making
the TLS handshake reach your server directly; a CDN terminates TLS and the whole design
stops functioning.

Check it resolves to the right address before continuing:

```sh
dig +short vpn.example.com
```

## 3. Set up Caddy

Reality needs a **real TLS server** to hide behind. When a connection arrives that
isn't one of your users, sing-box forwards it to that server, so a stranger poking at
your port sees an ordinary website rather than a proxy. That server has to hold a valid
certificate for your hostname.

[caddy-docker-proxy](https://github.com/lucaslorentz/caddy-docker-proxy) handles this
with no configuration files: it watches Docker labels and gets certificates
automatically.

Skip to step 4 if you already run it — just note your Caddy container's name
(`docker ps | grep caddy`).

```sh
docker network create caddy

mkdir -p /srv/caddy && cd /srv/caddy
cat > docker-compose.yml <<'EOF'
services:
  caddy:
    image: lucaslorentz/caddy-docker-proxy:ci-alpine
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - caddy-data:/data
    networks:
      - caddy

volumes:
  caddy-data:

networks:
  caddy:
    external: true
EOF

docker compose up -d
```

Find the container's name — you need it in the next step:

```sh
docker ps --format '{{.Names}}' | grep caddy
# caddy-caddy-1
```

## 4. Install vlessvmore

```sh
mkdir -p /srv/vlessvmore/config /srv/vlessvmore/data
cd /srv/vlessvmore
```

Generate the config. Use **your** hostname, and **your** Caddy container name from the
previous step:

```sh
docker run --rm ghcr.io/reeywhaar/vlessvmore \
  init --host vpn.example.com --handshake caddy-caddy-1:443 \
  --name "My VPN" \
  > config/config.json
```

`--name` is what your users will see the profile called in their app. Leave it out and
they see their own username instead, which reads oddly.

The config goes to the file; a summary and next steps print to your terminal. Have a
look at what it wrote:

```sh
cat config/config.json
```

There are no secrets in it — just where the server lives and how it behaves. The Reality
keypair is generated on first start and stored in `data/`.

Now the compose file:

```sh
cat > docker-compose.yml <<'EOF'
services:
  vlessvmore:
    image: ghcr.io/reeywhaar/vlessvmore:latest
    container_name: vlessvmore
    restart: unless-stopped
    ports:
      - "8443:8443"
    volumes:
      - ./config:/etc/vlessvmore:ro
      - ./data:/var/lib/vlessvmore
    networks:
      - caddy
    labels:
      caddy: vpn.example.com
      caddy.reverse_proxy: "{{upstreams 80}}"
    healthcheck:
      test: ["CMD", "vlessvmore", "status"]
      interval: 30s
      timeout: 5s
      start_period: 10s

networks:
  caddy:
    external: true
EOF
```

Edit `vpn.example.com` in the labels to your hostname, then start it:

```sh
docker compose up -d
```

> **The `./data` mount matters.** It holds the Reality keypair. Without it, every
> container recreation generates a new one and all your clients stop connecting.

## 5. Check it started

```sh
docker exec vlessvmore vlessvmore status
```

```
sing-box         running (pid 28)
uptime           12s
users            0 (0 active)
data dir         /var/lib/vlessvmore
```

Confirm Caddy got a certificate and is serving your hostname:

```sh
curl -sI https://vpn.example.com | head -1
# HTTP/2 200
```

A `200` means Caddy has a real certificate and is proxying to vlessvmore. That plain
response is deliberate — anyone who visits sees an unremarkable web server.

If this fails, wait a few seconds and retry; certificate issuance takes a moment on the
first request. Check `docker logs caddy-caddy-1` if it persists.

## 6. Add a user

```sh
docker exec vlessvmore vlessvmore user add alice
```

```
name               alice
id                 u_06FSRMCA5J6KT1AGTZE5WV128W
state              enabled
quota              unlimited
expires            never
subscription       https://vpn.example.com/sub/QK7M2X...

vless://268e4039-...@vpn.example.com:8443?type=tcp&encryption=none&...#alice

█▀▀▀▀▀█ ▄▀▄ ▄▄▀ █▀▀▀▀▀█
█ ███ █ ▀▄█▀▄██ █ ███ █
█▄▄▄▄▄█ ▄ ▀▄█ ▀ █▄▄▄▄▄█
 …
```

You get three things. The one to use is the **subscription URL**:

| | what it is | when to use it |
| --- | --- | --- |
| **subscription URL** | a link the client re-checks periodically | **this one**, normally |
| `vless://…` link | a static credential | one-off, or a client with no subscription support |
| QR code | the subscription URL, scannable | phones |

Prefer the subscription URL because the client keeps itself current: if you later change
the port, rotate the server key, or the user hits their quota, the client finds out on
its own. A pasted `vless://` link is frozen at the moment you copied it.

Users are unlimited and non-expiring by default. If you want limits, they are per user
and opt-in:

```sh
docker exec vlessvmore vlessvmore user add bob --quota 100GB --expires 30d
```

## 7. Connect with Hiddify

[Hiddify](https://hiddify.com/) is a good choice because it's built on sing-box, the
same core as the server, so Reality behaves identically at both ends. Available for
Android, iOS, Windows, macOS and Linux.

Other sing-box-based clients work the same way. Avoid Xray-based clients for the first
test — Reality differs subtly between cores, and you want to rule that out.

**On a phone — scan the code**

1. Install Hiddify from your app store.
2. Run `docker exec vlessvmore vlessvmore user show alice` on the server. Make the
   terminal window large enough that the QR code is not squashed.
3. In Hiddify: **+** (top right) → **Scan QR code**.
4. Point it at your terminal.
5. Tap the big power button to connect.

**On a desktop — paste the URL**

1. Get the URL:
   ```sh
   docker exec vlessvmore vlessvmore user sub alice
   ```
2. Copy it.
3. In Hiddify: **+** → **Add from clipboard**.
4. Connect.

**Manually, if you prefer**

If you'd rather type the values, `vlessvmore server show` prints everything a client
needs: host, port, SNI, public key, short id, flow and fingerprint. Choose **VLESS**,
transport **TCP**, security **Reality**.

## 8. Confirm it works

In Hiddify, the profile should show a connected state and — because the server sends
usage headers with every subscription fetch — the traffic used, plus a quota and expiry
date if you set them.

Check your traffic is actually going through the server:

```sh
curl -s https://api.ipify.org
```

That should print your **server's** IP, not your own.

Then confirm the server saw it. Traffic is collected every 30 seconds, so wait a moment:

```sh
docker exec vlessvmore vlessvmore user usage alice --bucket hour
```

Non-zero numbers mean the whole path works end to end: client, Reality handshake,
sing-box, and the stats collector.

## Day-to-day

```sh
# Who is using what
docker exec vlessvmore vlessvmore user ls

# Add someone with a 30-day, 100 GB allowance
docker exec vlessvmore vlessvmore user add carol --quota 100GB --expires 30d

# Revoke access, keeping the account and its history
docker exec vlessvmore vlessvmore user set alice --disable

# Someone hit their quota and you want to reset the window
docker exec vlessvmore vlessvmore user reset-usage alice

# A subscription URL leaked — issue a new one. The client keeps working;
# only the URL changes.
docker exec vlessvmore vlessvmore user rotate-sub alice

# Back up. Do this before you need it.
docker exec vlessvmore vlessvmore export --all > vlessvmore-backup.json
```

Full command reference in [CLI.md](CLI.md). Keep that backup somewhere safe — it
contains the server keypair and every user's credential.

## If something is wrong

**`curl https://vpn.example.com` fails**

Caddy has not issued a certificate. Check DNS points at this server, that ports 80 and
443 are open, and that the `caddy:` label matches your hostname exactly.

```sh
docker logs caddy-caddy-1 --tail 50
```

**The client says connected, but no traffic flows**

Check port 8443 is reachable from outside — a cloud firewall or security group blocking
it is the usual cause:

```sh
# from your laptop, not the server
nc -vz vpn.example.com 8443
```

**`REALITY: processed invalid connection` in the logs**

The client's public key doesn't match the server's. Almost always a stale link: get a
fresh one with `user sub alice` and re-import. If you rotated the server key, everyone
needs to re-fetch.

```sh
docker logs vlessvmore --tail 50
```

**Usage stays at zero**

Confirm the image has the right build tag. Without `with_v2ray_api` there are no
per-user counters at all, so usage and quotas silently do nothing:

```sh
docker exec vlessvmore vlessvmore version
```

The `Tags:` line must include `with_v2ray_api`.

**Everyone stopped connecting after a restart**

The `./data` mount is probably missing or was replaced, so a new keypair was generated.
Check the logs:

```sh
docker logs vlessvmore | grep -i identity
```

If you see `generated a NEW Reality identity even though users already exist`, that is
what happened. Restore from a backup:

```sh
docker exec -i vlessvmore vlessvmore import --force < vlessvmore-backup.json
docker compose restart
```

With no backup, the keypair is gone: everyone needs a new link
(`vlessvmore user sub <name>` for each). The user list itself is intact.

**Something else**

`docker exec vlessvmore vlessvmore status` shows the last config generation error, if
any. `docker logs vlessvmore` has both the manager's log and sing-box's own output.
