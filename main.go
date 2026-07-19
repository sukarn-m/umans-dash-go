package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	proxy "umans-dash-go/src"
)

const (
	maxPortRetries    = 3
	portRetryDelay    = 2 * time.Second
	maxPortIncrements = 10

	Version = "1.4.2"
)

func main() {
	// Step 1: Load config (§31 step 1)
	cfg, err := proxy.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Step 2: Create Proxy (runs §31 steps 2-7 inside NewProxy)
	p := proxy.NewProxy(&cfg)
	p.Version = Version
	p.DashboardHTML = proxy.DashboardHTML
	p.DashboardJS = proxy.DashboardJS

	// Step 3: Parse listen address
	listenAddr := cfg.ListenAddr
	if listenAddr == "" {
		listenAddr = "127.0.0.1:8084"
	}
	host, portStr, err := net.SplitHostPort(listenAddr)
	if err != nil {
		log.Fatalf("Invalid LISTEN_ADDR %q: %v", listenAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		log.Fatalf("Invalid port in LISTEN_ADDR %q: %v", listenAddr, err)
	}

	// Step 4: Create http.Server with timeouts
	server := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", host, port),
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}
	p.SetHTTPServer(server)

	// Step 5: Port retry listener loop (§31.2)
	var listener net.Listener
	currentPort := port
	portRetries := 0
	portIncrements := 0

	for {
		addr := fmt.Sprintf("%s:%d", host, currentPort)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			if isAddrInUse(err) && portRetries < maxPortRetries {
				portRetries++
				log.Printf("Address %s already in use, retrying (%d/%d) in %v...", addr, portRetries, maxPortRetries, portRetryDelay)
				time.Sleep(portRetryDelay)
				continue
			}
			if isAddrInUse(err) && portIncrements < maxPortIncrements {
				portIncrements++
				currentPort++
				portRetries = 0
				log.Printf("Address %s still in use after %d retries, trying port %d...", addr, maxPortRetries, currentPort)
				continue
			}
			log.Fatalf("Failed to listen on %s: %v", addr, err)
		}
		listener = ln
		break
	}

	// Step 6: Log successful listen
	log.Printf("UMANS Dash Go v%s listening on %s:%d", Version, host, currentPort)

	// Step 7: Start serving in goroutine
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Step 8: Signal handling (§31 step 10, §35.2)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("Received signal %v, shutting down...", sig)

	// Use Proxy.Shutdown() so the proxy-level drain, HTTP server shutdown,
	// error-log flush, and exit are handled consistently.
	p.Shutdown()
}

func isAddrInUse(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if errors.Is(opErr.Err, syscall.EADDRINUSE) {
			return true
		}
	}
	return strings.Contains(err.Error(), "address already in use")
}
