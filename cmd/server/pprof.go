package main

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
	"time"
)

// startPprof exposes the standard pprof endpoints on FOBE_PPROF (design §17).
//
// Two reasons this exists: a panel that feels slow can be profiled where it
// runs, and scripts/pgo.sh --url harvests the CPU profile that the NEXT build
// feeds to PGO — a profile taken from the real workload beats the test-suite
// fallback the script uses by default.
//
// Off unless FOBE_PPROF is set, and loopback-only on purpose: a heap profile
// *is* a memory dump, and this process holds the master key, the AI provider
// keys and the bot tokens in memory. Collecting from a container does not need
// a published port —
//
//	docker compose exec server wget -qO- \
//	  'http://127.0.0.1:6060/debug/pprof/profile?seconds=30' > cpu.pprof
//
// — and a remote host goes through `ssh -L`. Anything else is refused with an
// error instead of a warning, because "exposed for a minute" is how a memory
// dump ends up in a log aggregator.
func startPprof(log *slog.Logger) {
	addr := strings.TrimSpace(os.Getenv("FOBE_PPROF"))
	if addr == "" {
		return
	}
	if !loopbackAddr(addr) {
		log.Error("FOBE_PPROF ignored: only a loopback address is accepted "+
			"(a heap profile would hand out the master key); use `docker compose exec` or ssh -L",
			"value", addr)
		return
	}

	mux := http.NewServeMux()
	// The "/debug/pprof/" pattern is what makes the named profiles (heap,
	// goroutine, allocs, …) reachable; the rest are explicit so this file never
	// touches http.DefaultServeMux.
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Info("pprof endpoints enabled (loopback only)", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("pprof listener", "addr", addr, "err", err)
		}
	}()
}

// loopbackAddr reports whether host:port binds to this machine only. "localhost"
// counts (it resolves to loopback), and an empty host does not: ":6060" is
// every interface.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
