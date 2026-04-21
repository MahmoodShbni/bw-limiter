# bwlimit

A simple TCP proxy that enforces real bandwidth limits on a per-connection basis.

Unlike `tc`, which relies on kernel queueing and TCP's own reaction to congestion, this proxy controls throughput in the application layer with a token bucket — giving you precise, predictable limits.

## Features

- Per-connection upload and download limits
- Independent rate limits per direction
- Live stats every 5 seconds
- Human-friendly units (KB, MB)
- Single static binary, no dependencies at runtime

## Build

```bash
go mod tidy
go build -o bwlimit main.go
```

Or grab a prebuilt binary from the Releases page.

## Usage

```bash
./bwlimit -listen 8080 -target 127.0.0.1:80 -up 30KB -down 300KB
```

Now any client connecting to port 8080 will be forwarded to `127.0.0.1:80` with a 30KB/s upload cap and 300KB/s download cap.

## Flags

| Flag | Description | Default |
|---|---|---|
| `-listen` | Local port to listen on | `8080` |
| `-target` | Target address `host:port` | `127.0.0.1:80` |
| `-up` | Upload rate limit (client → server) | `30KB` |
| `-down` | Download rate limit (server → client) | `300KB` |
| `-burst` | Token bucket burst size (KB) | `32` |

## Examples

**Limit a local web server:**
```bash
./bwlimit -listen 8080 -target 127.0.0.1:80 -up 100KB -down 500KB
```

**Limit multiple ports (run multiple instances):**
```bash
./bwlimit -listen 8080 -target 127.0.0.1:80 -up 30KB -down 300KB &
./bwlimit -listen 8443 -target 127.0.0.1:443 -up 30KB -down 300KB &
```

**Transparent redirect with iptables:**
```bash
# Move real service to port 8000, run bwlimit on 8080, redirect 80 → 8080
./bwlimit -listen 8080 -target 127.0.0.1:8000 -up 30KB -down 300KB &
sudo iptables -t nat -A PREROUTING -p tcp --dport 80 -j REDIRECT --to-port 8080
```

## Systemd service

`/etc/systemd/system/bwlimit.service`:

```ini
[Unit]
Description=Bandwidth Limiter Proxy
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/bwlimit -listen 8080 -target 127.0.0.1:80 -up 30KB -down 300KB
Restart=always
User=root

[Install]
WantedBy=multi-user.target
```

```bash
sudo cp bwlimit /usr/local/bin/
sudo systemctl daemon-reload
sudo systemctl enable --now bwlimit
sudo journalctl -u bwlimit -f
```
