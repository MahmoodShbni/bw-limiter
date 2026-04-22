package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// ================= CONFIG =================

type Config struct {
	Name     string `json:"name"`
	Listen   int    `json:"listen"`
	Target   string `json:"target"`
	Up       string `json:"up"`
	Down     string `json:"down"`
	MaxConn  int    `json:"max_conn"`
}

func parseRate(s string) int {
	var v int
	m := 1

	switch {
	case len(s) > 2 && s[len(s)-2:] == "KB":
		m = 1024
		fmt.Sscanf(s, "%dKB", &v)
	case len(s) > 2 && s[len(s)-2:] == "MB":
		m = 1024 * 1024
		fmt.Sscanf(s, "%dMB", &v)
	default:
		fmt.Sscanf(s, "%d", &v)
	}

	return v * m
}

// ================= USER =================

type User struct {
	cfg Config

	activeConn int64

	upLimiter   *rate.Limiter
	downLimiter *rate.Limiter

	upBytes   uint64
	downBytes uint64
}

func NewUser(c Config) *User {
	return &User{
		cfg: c,
		upLimiter: rate.NewLimiter(
			rate.Limit(parseRate(c.Up)),
			8*1024,
		),
		downLimiter: rate.NewLimiter(
			rate.Limit(parseRate(c.Down)),
			8*1024,
		),
	}
}

// ================= CONNECTION LIMIT =================

func (u *User) allowConn() bool {
	if u.cfg.MaxConn <= 0 {
		return true
	}
	return atomic.LoadInt64(&u.activeConn) < int64(u.cfg.MaxConn)
}

// ================= PIPE =================

func pipe(ctx context.Context, src, dst net.Conn, lim *rate.Limiter, counter *uint64) {
	buf := make([]byte, 4*1024)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := src.Read(buf)
		if n > 0 {

			// 🔥 pacing BEFORE write (important)
			if err := lim.WaitN(ctx, n); err != nil {
				return
			}

			w := 0
			for w < n {
				x, err := dst.Write(buf[w:n])
				if err != nil {
					return
				}
				w += x
			}

			atomic.AddUint64(counter, uint64(n))
		}

		if err != nil {
			return
		}
	}
}

// ================= HANDLE =================

func (u *User) handle(c net.Conn) {
	if !u.allowConn() {
		log.Printf("[%s] REJECT maxConn reached", u.cfg.Name)
		c.Close()
		return
	}

	atomic.AddInt64(&u.activeConn, 1)
	defer atomic.AddInt64(&u.activeConn, -1)

	t, err := net.DialTimeout("tcp", u.cfg.Target, 10*time.Second)
	if err != nil {
		c.Close()
		return
	}

	_ = c.(*net.TCPConn).SetNoDelay(true)
	_ = t.(*net.TCPConn).SetNoDelay(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go pipe(ctx, c, t, u.upLimiter, &u.upBytes)
	pipe(ctx, t, c, u.downLimiter, &u.downBytes)
}

// ================= SERVER =================

func (u *User) start() {
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", u.cfg.Listen))
	if err != nil {
		log.Fatal(err)
	}
	defer l.Close()

	log.Printf("[%s] listen :%d -> %s | up=%s down=%s maxConn=%d",
		u.cfg.Name,
		u.cfg.Listen,
		u.cfg.Target,
		u.cfg.Up,
		u.cfg.Down,
		u.cfg.MaxConn,
	)

	go u.stats()

	for {
		c, err := l.Accept()
		if err != nil {
			continue
		}
		go u.handle(c)
	}
}

// ================= STATS =================

func (u *User) stats() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()

	var lastUp, lastDown uint64

	for range t.C {
		up := atomic.LoadUint64(&u.upBytes)
		down := atomic.LoadUint64(&u.downBytes)
		conn := atomic.LoadInt64(&u.activeConn)

		upRate := (up - lastUp) / 2
		downRate := (down - lastDown) / 2

		lastUp = up
		lastDown = down

		log.Printf("[%s] conn=%d up=%dKB/s down=%dKB/s",
			u.cfg.Name,
			conn,
			upRate/1024,
			downRate/1024,
		)
	}
}

// ================= MAIN =================

func main() {
	cfgFile := flag.String("config", "config.json", "")
	flag.Parse()

	data, err := os.ReadFile(*cfgFile)
	if err != nil {
		log.Fatal(err)
	}

	var cfgs []Config
	if err := json.Unmarshal(data, &cfgs); err != nil {
		log.Fatal(err)
	}

	for _, c := range cfgs {
		u := NewUser(c)
		go u.start()
	}

	select {}
}
