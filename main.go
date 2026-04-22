package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// ================= CONFIG =================

type ProxyConfig struct {
	Name   string `json:"name"`
	Listen int    `json:"listen"`
	Target string `json:"target"`
	Up     string `json:"up"`
	Down   string `json:"down"`
}

func parseRate(s string) int {
	s = strings.ToUpper(strings.TrimSpace(s))
	m := 1

	switch {
	case strings.HasSuffix(s, "KB"):
		m = 1024
		s = strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "MB"):
		m = 1024 * 1024
		s = strings.TrimSuffix(s, "MB")
	}

	var v int
	fmt.Sscanf(s, "%d", &v)
	return v * m
}

func load(path string) ([]ProxyConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []ProxyConfig
	sc := bufio.NewScanner(f)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var c ProxyConfig
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, sc.Err()
}

// ================= PROXY =================

type Proxy struct {
	name   string
	listen int
	target string

	upRate   int
	downRate int

	upBytes   uint64
	downBytes uint64
	connCount int64
}

func NewProxy(c ProxyConfig) *Proxy {
	return &Proxy{
		name:     c.Name,
		listen:   c.Listen,
		target:   c.Target,
		upRate:   parseRate(c.Up),
		downRate: parseRate(c.Down),
	}
}

// ================= CORE STREAM =================

func pipe(ctx context.Context, src net.Conn, dst net.Conn, limiter *rate.Limiter, counter *uint64) {
	defer src.Close()
	defer dst.Close()

	buf := make([]byte, 4*1024)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// read
		n, err := src.Read(buf)
		if n > 0 {

			// 🔥 pacing BEFORE write (fix collapse)
			if limiter != nil {
				if err := limiter.WaitN(ctx, n); err != nil {
					return
				}
			}

			written := 0
			for written < n {
				w, err := dst.Write(buf[written:n])
				if err != nil {
					return
				}
				written += w
			}

			atomic.AddUint64(counter, uint64(n))
		}

		if err != nil {
			return
		}
	}
}

// ================= HANDLER =================

func (p *Proxy) handle(client net.Conn) {
	atomic.AddInt64(&p.connCount, 1)
	defer atomic.AddInt64(&p.connCount, -1)

	target, err := net.DialTimeout("tcp", p.target, 10*time.Second)
	if err != nil {
		client.Close()
		return
	}

	// 🔥 IMPORTANT TCP tuning (fix IDM + VLESS jitter)
	if c, ok := client.(*net.TCPConn); ok {
		_ = c.SetNoDelay(true)
		_ = c.SetReadBuffer(256 * 1024)
		_ = c.SetWriteBuffer(256 * 1024)
	}
	if t, ok := target.(*net.TCPConn); ok {
		_ = t.SetNoDelay(true)
		_ = t.SetReadBuffer(256 * 1024)
		_ = t.SetWriteBuffer(256 * 1024)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 🔥 per-connection limiter (IMPORTANT FIX)
	upLimiter := rate.NewLimiter(rate.Limit(p.upRate), 8*1024)
	downLimiter := rate.NewLimiter(rate.Limit(p.downRate), 8*1024)

	go pipe(ctx, client, target, upLimiter, &p.upBytes)
	pipe(ctx, target, client, downLimiter, &p.downBytes)
}

// ================= START =================

func (p *Proxy) start() {
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", p.listen))
	if err != nil {
		log.Fatal(err)
	}
	defer l.Close()

	log.Printf("[%s] listening :%d -> %s", p.name, p.listen, p.target)

	for {
		c, err := l.Accept()
		if err != nil {
			continue
		}
		go p.handle(c)
	}
}

// ================= MAIN =================

func main() {
	cfgPath := flag.String("config", "config.json", "")
	flag.Parse()

	cfgs, err := load(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}

	for _, c := range cfgs {
		p := NewProxy(c)
		go p.start()
	}

	select {}
}
