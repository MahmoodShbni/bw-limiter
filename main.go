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
	Burst  string `json:"burst,omitempty"` // optional, default 32KB
}

// loadConfigs reads a JSONL file (one JSON object per line).
// Empty lines and lines starting with # are ignored.
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
// Stats (global, aggregated)
// ============================================================

type ProxyStats struct {
	Name          string
	UploadBytes   uint64
	DownloadBytes uint64
	Connections   int64
	TotalConns    uint64
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
// Rate Limited Reader
// ============================================================

type rateLimitedReader struct {
	r       io.Reader
	limiter *rate.Limiter
	counter *uint64
	ctx     context.Context
}

func (rlr *rateLimitedReader) Read(p []byte) (int, error) {
	const maxChunk = 4096
	if len(p) > maxChunk {
		p = p[:maxChunk]
	}

	n, err := rlr.r.Read(p)
	if n > 0 {
		atomic.AddUint64(rlr.counter, uint64(n))
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
	name         string
	listenPort   int
	targetAddr   string
	uploadRate   int
	downloadRate int
	burst        int
	stats        *ProxyStats

	// Shared limiters across ALL connections of this proxy.
	// This is what makes the total rate for this proxy respect the configured limit,
	// regardless of how many concurrent connections are open.
	uploadLimiter   *rate.Limiter
	downloadLimiter *rate.Limiter
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

	// Default burst: 1 second worth of data, but capped between 4KB and 64KB.
	// Small burst -> tighter rate enforcement, larger burst -> more forgiving
	// for bursty traffic.
	var burst int
	if cfg.Burst == "" {
		burst = downloadRate
		if uploadRate > burst {
			burst = uploadRate
		}
		if burst < 4*1024 {
			burst = 4 * 1024
		}
		if burst > 64*1024 {
			burst = 64 * 1024
		}
	} else {
		burst, err = parseRate(cfg.Burst)
		if err != nil {
			return nil, fmt.Errorf("%s: burst: %w", cfg.Name, err)
		}
	}

	return &Proxy{
		name:            cfg.Name,
		listenPort:      cfg.Listen,
		targetAddr:      cfg.Target,
		uploadRate:      uploadRate,
		downloadRate:    downloadRate,
		burst:           burst,
		stats:           getOrCreateStats(cfg.Name),
		uploadLimiter:   rate.NewLimiter(rate.Limit(uploadRate), burst),
		downloadLimiter: rate.NewLimiter(rate.Limit(downloadRate), burst),
	}, nil
}

func (p *Proxy) Start(ctx context.Context) error {
	lc := net.ListenConfig{}
	listener, err := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", p.listenPort))
	if err != nil {
		return fmt.Errorf("[%s] listen :%d: %w", p.name, p.listenPort, err)
	}
	defer listener.Close()

	log.Printf("[%s] listening on :%d -> %s (up: %s/s, down: %s/s)",
		p.name, p.listenPort, p.targetAddr,
		humanizeBytes(p.uploadRate), humanizeBytes(p.downloadRate))

	// Close listener when context is done.
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
			log.Printf("[%s] accept error: %v", p.name, err)
			continue
		}
		go p.handleConnection(clientConn)
	}
}

func (p *Proxy) handleConnection(clientConn net.Conn) {
	defer clientConn.Close()

	atomic.AddInt64(&p.stats.Connections, 1)
	atomic.AddUint64(&p.stats.TotalConns, 1)
	defer atomic.AddInt64(&p.stats.Connections, -1)

	clientAddr := clientConn.RemoteAddr().String()

	// Compute a tight socket buffer size so TCP backpressure kicks in quickly.
	// Too big: kernel buffers megabytes before the sender slows down
	//          (real bandwidth usage exceeds the configured limit).
	// Too small: TCP can't keep pipeline full, throughput drops below the limit.
	// Target: ~2 seconds of rate, clamped between 8KB and 128KB.
	sockBuf := p.downloadRate * 2
	if p.uploadRate*2 > sockBuf {
		sockBuf = p.uploadRate * 2
	}
	if sockBuf < 8*1024 {
		sockBuf = 8 * 1024
	}
	if sockBuf > 128*1024 {
		sockBuf = 128 * 1024
	}

	if tc, ok := clientConn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(sockBuf)
		_ = tc.SetWriteBuffer(sockBuf)
	}

	targetConn, err := net.DialTimeout("tcp", p.targetAddr, 10*time.Second)
	if err != nil {
		log.Printf("[%s] dial %s: %v", p.name, p.targetAddr, err)
		return
	}
	defer targetConn.Close()

	// Same for the upstream socket - the real backpressure win.
	if tc, ok := targetConn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(sockBuf)
		_ = tc.SetWriteBuffer(sockBuf)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)

	// client -> server (upload)
	go func() {
		defer wg.Done()
		defer func() {
			if tc, ok := targetConn.(*net.TCPConn); ok {
				tc.CloseWrite()
			}
		}()
		limited := &rateLimitedReader{
			r:       clientConn,
			limiter: p.uploadLimiter, // shared across all connections
			counter: &p.stats.UploadBytes,
			ctx:     ctx,
		}
		io.Copy(targetConn, limited)
	}()

	// server -> client (download)
	go func() {
		defer wg.Done()
		defer func() {
			if tc, ok := clientConn.(*net.TCPConn); ok {
				tc.CloseWrite()
			}
		}()
		limited := &rateLimitedReader{
			r:       targetConn,
			limiter: p.downloadLimiter, // shared across all connections
			counter: &p.stats.DownloadBytes,
			ctx:     ctx,
		}
		io.Copy(clientConn, limited)
	}()

	wg.Wait()
	_ = clientAddr
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

			prev := last[name]
			upRate := float64(up-prev.upload) / elapsed
			downRate := float64(down-prev.download) / elapsed

			log.Printf("[%s] conns: %d/%d | up: %s/s | down: %s/s | total: up=%s down=%s",
				name, conns, total,
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

	// Check for duplicate listen ports.
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
				log.Printf("[%s] stopped: %v", proxy.name, err)
			}
		}(p)
	}

	go printStats(*statsInterval)

	wg.Wait()
}
