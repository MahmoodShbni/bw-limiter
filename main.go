package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// ============================================================
// Stats
// ============================================================

type Stats struct {
	uploadBytes   uint64 // bytes from client to server
	downloadBytes uint64 // bytes from server to client
	connections   int64  // active connections
	totalConns    uint64 // total connections since start
}

var stats = &Stats{}

// ============================================================
// Rate Limited Reader - core logic
// ============================================================

// rateLimitedReader wraps an io.Reader and limits read throughput
type rateLimitedReader struct {
	r       io.Reader
	limiter *rate.Limiter
	counter *uint64 // pointer to stats counter
	ctx     context.Context
}

func (rlr *rateLimitedReader) Read(p []byte) (int, error) {
	// Cap each read to a small chunk for tighter rate control.
	// Must be <= burst size or WaitN will fail.
	const maxChunk = 4096
	if len(p) > maxChunk {
		p = p[:maxChunk]
	}

	n, err := rlr.r.Read(p)
	if n > 0 {
		atomic.AddUint64(rlr.counter, uint64(n))

		// Block until the token bucket allows n bytes through.
		// This is where the actual rate limiting happens.
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
	listenPort   int
	targetAddr   string
	uploadRate   int // bytes per second - client to server
	downloadRate int // bytes per second - server to client
	burst        int // token bucket burst size
}

func (p *Proxy) Start() error {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", p.listenPort))
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %v", p.listenPort, err)
	}
	defer listener.Close()

	log.Printf("proxy started on port %d", p.listenPort)
	log.Printf("target: %s", p.targetAddr)
	log.Printf("upload limit:   %s/s", humanizeBytes(p.uploadRate))
	log.Printf("download limit: %s/s", humanizeBytes(p.downloadRate))

	for {
		clientConn, err := listener.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			continue
		}

		go p.handleConnection(clientConn)
	}
}

func (p *Proxy) handleConnection(clientConn net.Conn) {
	defer clientConn.Close()

	atomic.AddInt64(&stats.connections, 1)
	atomic.AddUint64(&stats.totalConns, 1)
	defer atomic.AddInt64(&stats.connections, -1)

	clientAddr := clientConn.RemoteAddr().String()
	log.Printf("new connection from %s", clientAddr)

	targetConn, err := net.DialTimeout("tcp", p.targetAddr, 10*time.Second)
	if err != nil {
		log.Printf("failed to dial target %s: %v", p.targetAddr, err)
		return
	}
	defer targetConn.Close()

	// Per-connection limiters so each client gets its own budget.
	uploadLimiter := rate.NewLimiter(rate.Limit(p.uploadRate), p.burst)
	downloadLimiter := rate.NewLimiter(rate.Limit(p.downloadRate), p.burst)

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
			limiter: uploadLimiter,
			counter: &stats.uploadBytes,
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
			limiter: downloadLimiter,
			counter: &stats.downloadBytes,
			ctx:     ctx,
		}
		io.Copy(clientConn, limited)
	}()

	wg.Wait()
	log.Printf("connection %s closed", clientAddr)
}

// ============================================================
// Stats printer
// ============================================================

func printStats() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var lastUpload, lastDownload uint64
	lastTime := time.Now()

	for range ticker.C {
		now := time.Now()
		elapsed := now.Sub(lastTime).Seconds()

		currentUpload := atomic.LoadUint64(&stats.uploadBytes)
		currentDownload := atomic.LoadUint64(&stats.downloadBytes)
		currentConns := atomic.LoadInt64(&stats.connections)
		totalConns := atomic.LoadUint64(&stats.totalConns)

		uploadRate := float64(currentUpload-lastUpload) / elapsed
		downloadRate := float64(currentDownload-lastDownload) / elapsed

		log.Printf("stats | conns: %d active / %d total | up: %s/s | down: %s/s | total up: %s down: %s",
			currentConns,
			totalConns,
			humanizeBytes(int(uploadRate)),
			humanizeBytes(int(downloadRate)),
			humanizeBytes(int(currentUpload)),
			humanizeBytes(int(currentDownload)),
		)

		lastUpload = currentUpload
		lastDownload = currentDownload
		lastTime = now
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

// parseRate parses a string like "30KB" or "1MB" into bytes/second.
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
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}

	var value int
	if _, err := fmt.Sscanf(s, "%d", &value); err != nil {
		return 0, fmt.Errorf("invalid rate format: %s (example: 30KB, 1MB)", s)
	}
	return value * multiplier, nil
}

// ============================================================
// Main
// ============================================================

func main() {
	var (
		listenPort  = flag.Int("listen", 8080, "local port to listen on")
		target      = flag.String("target", "127.0.0.1:80", "target address (host:port)")
		uploadStr   = flag.String("up", "30KB", "upload rate limit (e.g. 30KB, 1MB)")
		downloadStr = flag.String("down", "300KB", "download rate limit (e.g. 300KB, 2MB)")
		burstKB     = flag.Int("burst", 32, "token bucket burst size in KB")
	)
	flag.Parse()

	uploadRate, err := parseRate(*uploadStr)
	if err != nil {
		log.Fatalf("upload: %v", err)
	}
	downloadRate, err := parseRate(*downloadStr)
	if err != nil {
		log.Fatalf("download: %v", err)
	}

	proxy := &Proxy{
		listenPort:   *listenPort,
		targetAddr:   *target,
		uploadRate:   uploadRate,
		downloadRate: downloadRate,
		burst:        *burstKB * 1024,
	}

	go printStats()

	if err := proxy.Start(); err != nil {
		log.Fatal(err)
	}
}
