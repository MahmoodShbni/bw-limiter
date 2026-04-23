# bwlimit

TCP proxy with per-proxy bandwidth limits AND connection limits. Multi-proxy from a single JSON config.

The point is **protecting the backend**. Rate limiting alone doesn't help when 500 connections pile up and swamp the target. This tool also caps concurrent connections, per-IP connections, and the rate of accepting new connections.

## Build

```bash
go mod tidy
go build -o bwlimit main.go
```

## Config format

`config.json` is JSONL — one JSON object per line.

```json
{"name": "sadegh", "listen": 9010, "target": "127.0.0.1:8010", "up": "5KB", "down": "10KB", "max_conns": 3, "max_conns_per_ip": 1, "new_conns_per_sec": 1, "idle_timeout_sec": 30}
```

### Fields

| Field | Required | Description |
|---|---|---|
| `name` | no | Label in logs (default: `proxy-<port>`) |
| `listen` | yes | Local listen port |
| `target` | yes | Backend address `host:port` |
| `up` | yes | Upload rate limit (client → target), e.g. `30KB`, `1MB` |
| `down` | yes | Download rate limit (target → client), e.g. `300KB`, `2MB` |
| `burst` | no | Token bucket burst (default: rate-based, min 64KB) |
| `max_conns` | no | Max concurrent connections total (0 = no limit) |
| `max_conns_per_ip` | no | Max concurrent connections per client IP (0 = no limit) |
| `new_conns_per_sec` | no | Max rate of accepting new connections (0 = no limit) |
| `idle_timeout_sec` | no | Kill connection after N seconds of no traffic (0 = disabled) |

## Why connection limits matter

A single client using IDM or a download accelerator can open 30+ parallel connections. If your rate limit is 100KB/s per connection, that's 3MB/s total from one user. Worse — each connection makes the backend (vless, shadowsocks, etc.) open its own upstream connection, multiplying load.

**Recommended starting point for most cases:**

```json
{"max_conns": 10, "max_conns_per_ip": 2, "new_conns_per_sec": 5, "idle_timeout_sec": 60}
```

- Caps total load on backend at 10 simultaneous streams
- One user can't monopolize with more than 2 connections
- Connection storms are smoothed out to 5/sec (with a small burst)
- Dead connections are reaped after a minute

When limits are hit, new connections are **rejected immediately** (not queued) — the client just sees a connection refused and will retry. This is better than queueing which can cause timeouts.

## Usage

```bash
./bwlimit -config config.json
```

Flags:

| Flag | Description | Default |
|---|---|---|
| `-config` | Path to JSONL config | `config.json` |
| `-stats-interval` | How often to print stats | `5s` |

## Example log output

```
loaded 2 proxy entries from config.json
[sadegh] listening on :9010 -> 127.0.0.1:8010
[sadegh]   rates:  up=5.00 KB/s  down=10.00 KB/s  burst=64.00 KB
[sadegh]   limits: max_conns=3  per_ip=1  new_per_sec=1  idle=30s
------ stats ------
[sadegh] active:3 total:47 rejected:212 | up:4.80 KB/s down:9.92 KB/s | totals: up=142.00 KB down=284.00 KB
```

The `rejected` counter tells you whether your limits are being hit. If rejected grows fast, consider raising `max_conns` or `new_conns_per_sec`.

## Systemd service

```ini
[Unit]
Description=Bandwidth Limiter Proxy
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/bwlimit -config /etc/bwlimit/config.json
Restart=always
User=root

[Install]
WantedBy=multi-user.target
```
