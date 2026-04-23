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

	// Connection limiting - this is what actually protects the backend.
	MaxConns       int     `json:"max_conns,omitempty"`         // total concurrent conns (0 = no limit)
	MaxConnsPerIP  int     `json:"max_conns_per_ip,omitempty"`  // per-IP concurrent conns (0 = no limit)
	NewConnsPerSec float64 `json:"new_conns_per_sec,omitempty"` // rate of accepting new conns (0 = no limit)
	IdleTimeoutSec int     `json:"idle_timeout_sec,omitempty"`  // close conns idle for this long (0 = disabled)
}

// loadConfigs reads a JSONL file (one JSON object per line).
func loadConfigs(path string) ([]ProxyConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	var configs []ProxyConfig
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}

		var cfg ProxyConfig
		if err := json.Unmarshal([]byte(line), &cfg); err != nil {
			return nil, fmt.Errorf("line %d: invalid JSON: %w", lineNum, err)
		}

		if err := validateConfig(&cfg, lineNum); err != nil {
			return nil, err
		}

		configs = append(configs, cfg)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	if len(configs) == 0 {
		return nil, fmt.Errorf("no valid proxy entries found in %s", path)
	}

	return configs, nil
}

func validateConfig(cfg *ProxyConfig, lineNum int) error {
	if cfg.Name == "" {
		cfg.Name = fmt.Sprintf("proxy-%d", cfg.Listen)
	}
	if cfg.Listen <= 0 || cfg.Listen > 65535 {
		return fmt.Errorf("line %d (%s): invalid listen port %d", lineNum, cfg.Name, cfg.Listen)
	}
	if cfg.Target == "" {
		return fmt.Errorf("line %d (%s): target is required", lineNum, cfg.Name)
	}
	if cfg.Up == "" {
		return fmt.Errorf("line %d (%s): up is required", lineNum, cfg.Name)
	}
	if cfg.Down == "" {
		return fmt.Errorf("line %d (%s): down is required", lineNum, cfg.Name)
	}
	return nil
}

// ============================================================
// Stats
// ============================================================

type ProxyStats struct {
	Name          string
	UploadBytes   uint64
	DownloadBytes uint64
	Connections   int64
	TotalConns    uint64
	RejectedConns uint64
}

var (
	allStats   = make(map[string]*ProxyStats)
	statsMutex sync.RWMutex
)

func getOrCreateStats(name string) *ProxyStats {
	statsMutex.Lock()
	defer statsMutex.Unlock()
	s, ok := allStats[name]
	if !ok {
		s = &ProxyStats{Name: name}
		allStats[name] = s
	}
	return s
}

// ============================================================
// Per-IP connection counter
// ============================================================

type ipCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func newIPCounter() *ipCounter {
	return &ipCounter{counts: make(map[string]int)}
}

func (c *ipCounter) tryAdd(ip string, max int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if max > 0 && c.counts[ip] >= max {
		return false
	}
	c.counts[ip]++
	return true
}

func (c *ipCounter) remove(ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts[ip] <= 1 {
		delete(c.counts, ip)
	} else {
		c.counts[ip]--
	}
}

// ============================================================
// Rate Limited Reader with idle timeout
// ============================================================

type rateLimitedReader struct {
	r            io.Reader
	limiter      *rate.Limiter
	counter      *uint64
	lastActivity *int64 // unix nano, atomic
	ctx          context.Context
}

func (rlr *rateLimitedReader) Read(p []byte) (int, error) {
	// 16KB chunks - smooth for tunneled protocols, good enough for rate control.
	const maxChunk = 16 * 1024
	if len(p) > maxChunk {
		p = p[:maxChunk]
	}

	n, err := rlr.r.Read(p)
	if n > 0 {
		atomic.AddUint64(rlr.counter, uint64(n))
		if rlr.lastActivity != nil {
			atomic.StoreInt64(rlr.lastActivity, time.Now().UnixNano())
		}
		if waitErr := rlr.limiter.WaitN(rlr.ctx, n); waitErr != nil {
			return n, waitErr
		}
	}
	return n, err
}

// ============================================================
// Proxy
// ============================================================

type Proxy struct {
	cfg          ProxyConfig
	uploadRate   int
	downloadRate int
	burst        int
	stats        *ProxyStats

	// Shared rate limiters across all connections of this proxy.
	uploadLimiter   *rate.Limiter
	downloadLimiter *rate.Limiter

	// Connection limiting.
	connSem     chan struct{}  // semaphore for max total conns
	acceptLimit *rate.Limiter  // rate of accepting new conns
	ipCounter   *ipCounter     // per-IP active conn counter
	idleTimeout time.Duration
}

func NewProxy(cfg ProxyConfig) (*Proxy, error) {
	uploadRate, err := parseRate(cfg.Up)
	if err != nil {
		return nil, fmt.Errorf("%s: upload rate: %w", cfg.Name, err)
	}
	downloadRate, err := parseRate(cfg.Down)
	if err != nil {
		return nil, fmt.Errorf("%s: download rate: %w", cfg.Name, err)
	}

	var burst int
	if cfg.Burst == "" {
		burst = downloadRate
		if uploadRate > burst {
			burst = uploadRate
		}
		if burst < 64*1024 {
			burst = 64 * 1024
		}
		if burst > 256*1024 {
			burst = 256 * 1024
		}
	} else {
		burst, err = parseRate(cfg.Burst)
		if err != nil {
			return nil, fmt.Errorf("%s: burst: %w", cfg.Name, err)
		}
		if burst < 16*1024 {
			burst = 16 * 1024
		}
	}

	p := &Proxy{
		cfg:             cfg,
		uploadRate:      uploadRate,
		downloadRate:    downloadRate,
		burst:           burst,
		stats:           getOrCreateStats(cfg.Name),
		uploadLimiter:   rate.NewLimiter(rate.Limit(uploadRate), burst),
		downloadLimiter: rate.NewLimiter(rate.Limit(downloadRate), burst),
		ipCounter:       newIPCounter(),
	}

	if cfg.MaxConns > 0 {
		p.connSem = make(chan struct{}, cfg.MaxConns)
	}
	if cfg.NewConnsPerSec > 0 {
		// Allow a small burst (5 connections) to absorb legitimate bursts.
		p.acceptLimit = rate.NewLimiter(rate.Limit(cfg.NewConnsPerSec), 5)
	}
	if cfg.IdleTimeoutSec > 0 {
		p.idleTimeout = time.Duration(cfg.IdleTimeoutSec) * time.Second
	}

	return p, nil
}

func (p *Proxy) Start(ctx context.Context) error {
	lc := net.ListenConfig{}
	listener, err := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", p.cfg.Listen))
	if err != nil {
		return fmt.Errorf("[%s] listen :%d: %w", p.cfg.Name, p.cfg.Listen, err)
	}
	defer listener.Close()

	log.Printf("[%s] listening on :%d -> %s", p.cfg.Name, p.cfg.Listen, p.cfg.Target)
	log.Printf("[%s]   rates:  up=%s/s  down=%s/s  burst=%s",
		p.cfg.Name,
		humanizeBytes(p.uploadRate),
		humanizeBytes(p.downloadRate),
		humanizeBytes(p.burst))
	log.Printf("[%s]   limits: max_conns=%d  per_ip=%d  new_per_sec=%g  idle=%ds",
		p.cfg.Name,
		p.cfg.MaxConns,
		p.cfg.MaxConnsPerIP,
		p.cfg.NewConnsPerSec,
		p.cfg.IdleTimeoutSec)

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	for {
		clientConn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("[%s] accept error: %v", p.cfg.Name, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go p.handleConnection(clientConn)
	}
}

func (p *Proxy) handleConnection(clientConn net.Conn) {
	defer clientConn.Close()

	clientIP := remoteIP(clientConn)

	// 1. Rate-limit acceptance of NEW connections.
	// Prevents connection storms from slamming the backend.
	if p.acceptLimit != nil {
		// Reserve rather than wait - if we'd wait too long, just drop.
		r := p.acceptLimit.Reserve()
		if !r.OK() || r.Delay() > 2*time.Second {
			r.Cancel()
			atomic.AddUint64(&p.stats.RejectedConns, 1)
			return
		}
		time.Sleep(r.Delay())
	}

	// 2. Per-IP concurrent connection limit.
	// Blocks a single client (or IDM) from hogging all slots.
	if p.cfg.MaxConnsPerIP > 0 {
		if !p.ipCounter.tryAdd(clientIP, p.cfg.MaxConnsPerIP) {
			atomic.AddUint64(&p.stats.RejectedConns, 1)
			return
		}
		defer p.ipCounter.remove(clientIP)
	}

	// 3. Global concurrent connection limit (semaphore).
	// Keeps total load on the backend bounded - this is the key knob.
	if p.connSem != nil {
		select {
		case p.connSem <- struct{}{}:
			defer func() { <-p.connSem }()
		default:
			atomic.AddUint64(&p.stats.RejectedConns, 1)
			return
		}
	}

	atomic.AddInt64(&p.stats.Connections, 1)
	atomic.AddUint64(&p.stats.TotalConns, 1)
	defer atomic.AddInt64(&p.stats.Connections, -1)

	// Dial the target.
	targetConn, err := net.DialTimeout("tcp", p.cfg.Target, 10*time.Second)
	if err != nil {
		log.Printf("[%s] dial %s: %v", p.cfg.Name, p.cfg.Target, err)
		return
	}
	defer targetConn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Idle timeout tracking.
	var lastActivity int64
	atomic.StoreInt64(&lastActivity, time.Now().UnixNano())

	if p.idleTimeout > 0 {
		go p.watchIdle(ctx, cancel, &lastActivity, clientConn, targetConn)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// client -> target (upload)
	go func() {
		defer wg.Done()
		defer func() {
			if tc, ok := targetConn.(*net.TCPConn); ok {
				tc.CloseWrite()
			}
		}()
		limited := &rateLimitedReader{
			r:            clientConn,
			limiter:      p.uploadLimiter,
			counter:      &p.stats.UploadBytes,
			lastActivity: &lastActivity,
			ctx:          ctx,
		}
		io.Copy(targetConn, limited)
	}()

	// target -> client (download)
	go func() {
		defer wg.Done()
		defer func() {
			if tc, ok := clientConn.(*net.TCPConn); ok {
				tc.CloseWrite()
			}
		}()
		limited := &rateLimitedReader{
			r:            targetConn,
			limiter:      p.downloadLimiter,
			counter:      &p.stats.DownloadBytes,
			lastActivity: &lastActivity,
			ctx:          ctx,
		}
		io.Copy(clientConn, limited)
	}()

	wg.Wait()
}

// watchIdle closes the connection if it has been idle for too long.
func (p *Proxy) watchIdle(ctx context.Context, cancel context.CancelFunc, lastActivity *int64, c1, c2 net.Conn) {
	ticker := time.NewTicker(p.idleTimeout / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			last := time.Unix(0, atomic.LoadInt64(lastActivity))
			if time.Since(last) > p.idleTimeout {
				c1.Close()
				c2.Close()
				cancel()
				return
			}
		}
	}
}

func remoteIP(c net.Conn) string {
	addr := c.RemoteAddr().String()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// ============================================================
// Stats printer
// ============================================================

func printStats(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	type snapshot struct {
		upload, download uint64
	}
	last := make(map[string]snapshot)
	lastTime := time.Now()

	for range ticker.C {
		now := time.Now()
		elapsed := now.Sub(lastTime).Seconds()
		lastTime = now

		statsMutex.RLock()
		names := make([]string, 0, len(allStats))
		for name := range allStats {
			names = append(names, name)
		}
		statsMutex.RUnlock()

		log.Printf("------ stats ------")
		for _, name := range names {
			statsMutex.RLock()
			s := allStats[name]
			statsMutex.RUnlock()

			up := atomic.LoadUint64(&s.UploadBytes)
			down := atomic.LoadUint64(&s.DownloadBytes)
			conns := atomic.LoadInt64(&s.Connections)
			total := atomic.LoadUint64(&s.TotalConns)
			rejected := atomic.LoadUint64(&s.RejectedConns)

			prev := last[name]
			upRate := float64(up-prev.upload) / elapsed
			downRate := float64(down-prev.download) / elapsed

			log.Printf("[%s] active:%d total:%d rejected:%d | up:%s/s down:%s/s | totals: up=%s down=%s",
				name, conns, total, rejected,
				humanizeBytes(int(upRate)),
				humanizeBytes(int(downRate)),
				humanizeBytes(int(up)),
				humanizeBytes(int(down)),
			)

			last[name] = snapshot{upload: up, download: down}
		}
	}
}

// ============================================================
// Helpers
// ============================================================

func humanizeBytes(b int) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func parseRate(s string) (int, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	multiplier := 1
	switch {
	case strings.HasSuffix(s, "KB"):
		multiplier = 1024
		s = strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "MB"):
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "GB"):
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}

	var value int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &value); err != nil {
		return 0, fmt.Errorf("invalid rate format: %q (example: 30KB, 1MB)", s)
	}
	return value * multiplier, nil
}

// ============================================================
// Main
// ============================================================

func main() {
	var (
		configPath    = flag.String("config", "config.json", "path to JSONL config file")
		statsInterval = flag.Duration("stats-interval", 5*time.Second, "stats print interval")
	)
	flag.Parse()

	configs, err := loadConfigs(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	log.Printf("loaded %d proxy entries from %s", len(configs), *configPath)

	seen := make(map[int]string)
	for _, c := range configs {
		if prev, ok := seen[c.Listen]; ok {
			log.Fatalf("duplicate listen port %d used by %q and %q", c.Listen, prev, c.Name)
		}
		seen[c.Listen] = c.Name
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for _, cfg := range configs {
		p, err := NewProxy(cfg)
		if err != nil {
			log.Fatalf("create proxy: %v", err)
		}

		wg.Add(1)
		go func(proxy *Proxy) {
			defer wg.Done()
			if err := proxy.Start(ctx); err != nil {
				log.Printf("[%s] stopped: %v", proxy.cfg.Name, err)
			}
		}(p)
	}

	go printStats(*statsInterval)

	wg.Wait()
}
