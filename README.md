# bwlimit

A simple TCP proxy that enforces real bandwidth limits AND protects the backend from connection-abuse tools like IDM (Internet Download Manager), FDM, aria2, etc.

## Why this exists

- **`tc` can't fix this problem.** Kernel-level shaping doesn't know about connection counts, it only shapes packets.
- **IDM and similar tools open 8-32 parallel connections per file.** With 5 simultaneous downloads, that's 40-160 concurrent TCP connections — enough to overwhelm a vless/xray backend's CPU and crash it.
- **This proxy throttles both bandwidth AND connection count**, so the backend stays healthy.

## Features

- Per-proxy upload and download rate limits
- **Max concurrent connections per proxy**
- **Max concurrent connections per client IP** (this is the big one for IDM)
- **Rate limit for new connections per second** (prevents connection floods)
- **Idle connection timeout** (cleans up half-dead connections)
- Live stats every 5 seconds
- Single binary, no runtime dependencies

## Build

```bash
go mod tidy
go build -o bwlimit main.go
```

## Config format

One JSON object per line (`config.json`):

```json
{"name": "ali", "listen": 9010, "target": "127.0.0.1:1080", "up": "30KB", "down": "300KB", "max_conns": 30, "max_conns_per_ip": 6, "new_conns_per_sec": 5, "idle_timeout_sec": 60}
```

### Fields

| Field | Required | Description |
|---|---|---|
| `name` | no | Label for logs (default: `proxy-<port>`) |
| `listen` | yes | Local port to listen on |
| `target` | yes | Target address `host:port` |
| `up` | yes | Upload rate limit (client → server), e.g. `30KB`, `1MB` |
| `down` | yes | Download rate limit (server → client), e.g. `300KB`, `2MB` |
| `burst` | no | Token bucket burst size (auto-computed if omitted) |
| `max_conns` | no | Max total concurrent connections (0 = unlimited) |
| `max_conns_per_ip` | no | Max concurrent connections per client IP (0 = unlimited) |
| `new_conns_per_sec` | no | Max new connections accepted per second (0 = unlimited) |
| `idle_timeout_sec` | no | Close connections with no traffic for this long (0 = disabled) |

## Recommended settings for a vless/xray backend

Preventing IDM abuse:

```json
{
  "name": "user1",
  "listen": 9010,
  "target": "127.0.0.1:1080",
  "up": "30KB",
  "down": "300KB",
  "max_conns_per_ip": 6,
  "new_conns_per_sec": 5,
  "idle_timeout_sec": 60
}
```

**Why these numbers:**

- `max_conns_per_ip: 6` — normal browsers open 6-8 connections per host. IDM wants 8-32. Set to 6 to allow normal browsing but block accelerators.
- `new_conns_per_sec: 5` — prevents a flood of 100 new connections in 1 second (typical IDM burst).
- `idle_timeout_sec: 60` — frees slots from dead connections that clients forgot to close.

## Usage

```bash
./bwlimit -config config.json
```

## Example stats output

```
[ali] conns: 12/245 (rejected: 38) | up: 28.50 KB/s | down: 295.20 KB/s | total: up=142.00 KB down=1.45 MB
```

`rejected: 38` shows how many connections were dropped due to limits — that's IDM being stopped.

## Systemd service

`/etc/systemd/system/bwlimit.service`:

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

```bash
sudo mkdir -p /etc/bwlimit
sudo cp config.json /etc/bwlimit/
sudo cp bwlimit /usr/local/bin/
sudo systemctl daemon-reload
sudo systemctl enable --now bwlimit
sudo journalctl -u bwlimit -f
```
