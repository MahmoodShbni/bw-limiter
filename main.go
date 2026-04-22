package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// ============================================================
// Config
// ============================================================

type ProxyConfig struct {
	Name   string `json:"name"`
	Listen int    `json:"listen"`
	Target string `json:"target"`
	Up     string `json:"up"`
	Down   string `json:"down"`
	Burst  string `json:"burst,omitempty"`
}

func loadConfigs(path string) ([]ProxyConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cfgs []ProxyConfig
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)

	lineNum := 0
	for sc.Scan() {
		lineNum++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		var c ProxyConfig
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("line %d: %v", lineNum, err)
		}
		cfgs = append(cfgs, c)
	}
	return cfgs, sc.Err()
}

// ============================================================
// Stats
// ============================================================

type Stats struct {
	Name          string
	Up            uint64
	Down          uint64
	Conn          int64
	TotalConn     uint64
}

var (
	statsMap = make(map[string]*Stats)
	mu       sync.Mutex
)

func getStats(name string) *Stats {
	mu.Lock()
	defer mu.Unlock()

	if s, ok := statsMap[name]; ok {
		return s
	}
	s := &Stats{Name: name}
	statsMap[name] = s
	return s
}

// ============================================================
// Proxy
// ============================================================

type Proxy struct {
	name     string
	listen   int
	target   string
	stats    *Stats

	upLimiter   *rate.Limiter
	downLimiter *rate.Limiter
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

func NewProxy(c ProxyConfig) *Proxy {
	up := parseRate(c.Up)
	down := parseRate(c.Down)

	// 🔥 مهم: burst خیلی کوچک برای جلوگیری از spike
	burst := 4 * 1024

	return &Proxy{
		name:   c.Name,
		listen: c.Listen,
		target: c.Target,
		stats:  getStats(c.Name),

		upLimiter:   rate.NewLimiter(rate.Limit(up), burst),
		downLimiter: rate.NewLimiter(rate.Limit(down), burst),
	}
}

// ============================================================
// RATE LIMITED READER (FIX اصلی)
// ============================================================

const chunkSize = 4 * 1024

type rlReader struct {
	r       io.Reader
	limiter *rate.Limiter
	counter *uint64
	ctx     context.Context
}

func (r *rlReader) Read(p []byte) (int, error) {

	if len(p) > chunkSize {
		p = p[:chunkSize]
	}

	// ✅ مهم‌ترین تغییر: قبل از read throttle
	if err := r.limiter.WaitN(r.ctx, len(p)); err != nil {
		return 0, err
	}

	n, err := r.r.Read(p)
	if n > 0 {
		atomic.AddUint64(r.counter, uint64(n))
	}
	return n, err
}

// ============================================================
// Handler
// ============================================================

func (p *Proxy) handle(c net.Conn) {
	defer c.Close()

	atomic.AddInt64(&p.stats.Conn, 1)
	atomic.AddUint64(&p.stats.TotalConn, 1)
	defer atomic.AddInt64(&p.stats.Conn, -1)

	t, err := net.DialTimeout("tcp", p.target, 10*time.Second)
	if err != nil {
		return
	}
	defer t.Close()

	// 🔥 مهم برای VLESS / TCP
	_ = c.(*net.TCPConn).SetNoDelay(true)
	_ = t.(*net.TCPConn).SetNoDelay(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)

	// client -> target
	go func() {
		defer wg.Done()
		defer func() { if x, ok := t.(*net.TCPConn); ok { x.CloseWrite() } }()

		io.Copy(t, &rlReader{
			r:       c,
			limiter: p.upLimiter,
			counter: &p.stats.Up,
			ctx:     ctx,
		})
	}()

	// target -> client
	go func() {
		defer wg.Done()
		defer func() { if x, ok := c.(*net.TCPConn); ok { x.CloseWrite() } }()

		io.Copy(c, &rlReader{
			r:       t,
			limiter: p.downLimiter,
			counter: &p.stats.Down,
			ctx:     ctx,
		})
	}()

	wg.Wait()
}

// ============================================================
// Start
// ============================================================

func (p *Proxy) Start(ctx context.Context) error {
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", p.listen))
	if err != nil {
		return err
	}
	defer l.Close()

	log.Printf("[%s] listening on %d -> %s", p.name, p.listen, p.target)

	go func() {
		<-ctx.Done()
		l.Close()
	}()

	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go p.handle(c)
	}
}

// ============================================================
// Stats printer
// ============================================================

func statsLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()

	var lastUp, lastDown uint64

	for range t.C {
		mu.Lock()
		for _, s := range statsMap {
			up := atomic.LoadUint64(&s.Up)
			down := atomic.LoadUint64(&s.Down)

			log.Printf("[%s] conn=%d up=%dKB down=%dKB",
				s.Name,
				atomic.LoadInt64(&s.Conn),
				(up-lastUp)/1024,
				(down-lastDown)/1024,
			)

			lastUp = up
			lastDown = down
		}
		mu.Unlock()
	}
}

// ============================================================
// MAIN
// ============================================================

func main() {
	configPath := flag.String("config", "config.json", "")
	flag.Parse()

	cfgs, err := loadConfigs(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, c := range cfgs {
		p := NewProxy(c)
		go p.Start(ctx)
	}

	go statsLoop()

	select {}
}
