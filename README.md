# lastseen

A dead-man switch. Things check in over HTTP on a schedule; when one stops,
you get a message in Telegram. Once when it goes quiet, once when it returns.

It exists because a machine cannot report its own death. Anything that watches
itself is blind to the failure that matters most, and silence from a healthy
system looks exactly like silence from a burnt one. So this runs somewhere
else and waits.

```
atsos   ──GET /ping/<secret>/atsos───┐
nas     ──GET /ping/<secret>/nas─────┼──> lastseen ──> Telegram
backup  ──GET /ping/<secret>/backup──┘
```

## What it does and does not do

It reports **absence**, nothing else. The ping carries no payload: there is no
way to attach "disk is 91% full" to it. If a name stops checking in for longer
than the grace period, you hear about it.

Each outage produces exactly **one** message. An alert that repeats every tick
is one you learn to ignore, and then it is worth nothing on the day it counts.

Nothing is ever sent to the watched machines. The only inbound surface is one
secret path per monitor, and every other request is a 404.

## Set it up

`init` generates the secret and works out the address, so nothing has to be
typed by hand and then retyped on every client:

```
$ lastseen init -monitors atsos,nas,backup
wrote /etc/lastseen.conf

atsos                    http://192.168.1.99:9000/ping/K7fQ…/atsos
nas                      http://192.168.1.99:9000/ping/K7fQ…/nas
backup                   http://192.168.1.99:9000/ping/K7fQ…/backup
(status page)            http://192.168.1.99:9000/status/K7fQ…

that address is private, so these urls only work from the same network.
pass -advertise to publish something reachable from outside.
```

The address comes from the route this host would take to the internet, which
is right for a watchdog on the same network as the things it watches and
wrong for anything beyond it. It cannot be worked out in general: the daemon
knows what it binds, not what sits in front of it, and a host can reach the
world by more than one path. When it is wrong, say so once:

```sh
lastseen init -monitors atsos,nas -advertise https://watch.example.com:9000
```

Print the lines again whenever you need them, and add a monitor without
disturbing the secret every existing client already uses:

```sh
lastseen urls
lastseen add nas2
```

`init` refuses to overwrite an existing config. Regenerating the secret would
orphan every client still using the old one, and they would go quiet without
anyone knowing why.

Then point each thing at its own url. Anything able to make an HTTP request
qualifies: a daemon, a cron job, the tail of a backup script.

```sh
curl -sf http://192.168.1.99:9000/ping/K7fQ…/nas
*/5 * * * * curl -sf http://192.168.1.99:9000/ping/K7fQ…/backup
```

Ask who is alive at any time:

```
$ curl -s http://192.168.1.99:9000/status/K7fQ…
atsos                    ok      last seen 41s ago (2026-08-21T09:00:00Z)
nas                      ok      last seen 1m12s ago (2026-08-21T08:58:49Z)
backup                   SILENT  last seen 2h4m ago (2026-08-21T06:56:12Z)
```

## Choosing the grace period

The watchdog does not count pings, it looks at the last one only. Missing
beats in the middle are invisible; what matters is how long the silence has
lasted. So the grace period is how you express "how many lost pings are still
an accident":

| check-in every | grace | tolerates |
|---|---|---|
| 1m | 3m | two lost pings |
| 5m | 15m | two lost pings |
| 1h | 3h | two lost hours |

Set it too tight and a flaky network wakes you at night for nothing, which
ends with the alert muted and the watchdog useless.

## Names and the secret

`-monitors` is a declared list, not something clients register into. A name
that has never checked in is still reported after one grace period, because a
client that died before its first ping is exactly the case worth catching. A
name that is not on the list gets a 404 and changes nothing.

One secret covers every monitor. Whoever holds it can ping on behalf of any
name, including a dead one, which would hold its alert down. That is fine when
the secret stays inside your own network and worth knowing before it does not.

Two more things to know before you paste the URL anywhere:

- Any HTTP method counts as a check-in. A link preview or a browser prefetch
  will silence an alert, so keep the URL out of chats and dashboards that
  fetch links.
- The secret is part of the path, so it lands in the access log of any reverse
  proxy you put in front and in the shell history of whoever runs the curl.

## Install

```sh
make build                                    # dist/lastseen, via Docker
install -m 0755 dist/lastseen /usr/local/bin/
install -m 0644 configs/lastseen.service /etc/systemd/system/

printf 'BOT_TOKEN=...\nCHAT_ID=...\n' > /etc/lastseen.env
chmod 600 /etc/lastseen.env

lastseen init -monitors atsos,nas            # writes /etc/lastseen.conf, 0600
systemctl daemon-reload && systemctl enable --now lastseen
```

Credentials come from the environment. systemd reads `EnvironmentFile` as root
before dropping privileges, so the file stays unreadable to the service user,
and the token never appears in the process arguments where `ps` would show it
to everyone.

## Build and test

No local Go toolchain is required; everything runs in Docker.

```sh
make docker-test   # go test ./...
make build         # dist/lastseen
make version       # what the next build will stamp
```

Versions come from `git describe`, stamped through `-ldflags`. `lastseen
version` reports it and so does the startup log.

## State

Last-seen and the alerted flag are persisted, so restarting the watchdog
neither forgets an outstanding alert nor sends a duplicate. A corrupt state
file is refused at start-up rather than treated as a fresh install, because
starting empty would re-announce outages you have already been told about.

Dropping a name from `-monitors` stops watching it; its old entry in the state
file is simply ignored.
