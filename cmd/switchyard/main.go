// Command switchyard runs the AI provider gateway: three simulated providers, a
// router that fails over between them, a load generator that keeps traffic
// moving, and a Prometheus endpoint that makes all of it visible.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/bezilla/switchyard/internal/breaker"
	"github.com/bezilla/switchyard/internal/loadgen"
	"github.com/bezilla/switchyard/internal/provider"
	"github.com/bezilla/switchyard/internal/router"
	"github.com/bezilla/switchyard/internal/server"
	"github.com/bezilla/switchyard/internal/telemetry"
)

// version is stamped at build time via -ldflags.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "switchyard: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr         = flag.String("addr", envString("SWITCHYARD_ADDR", ":8080"), "listen address")
		policyFlag   = flag.String("policy", envString("SWITCHYARD_POLICY", "failover"), "routing policy: failover or cost")
		rps          = flag.Float64("rps", envFloat("SWITCHYARD_RPS", 12), "load generator requests per second, 0 to disable")
		seed         = flag.Uint64("seed", envUint("SWITCHYARD_SEED", 1), "simulation seed")
		probeEvery   = flag.Duration("probe-interval", envDuration("SWITCHYARD_PROBE_INTERVAL", time.Second), "health probe interval")
		probeTimeout = flag.Duration("probe-timeout", envDuration("SWITCHYARD_PROBE_TIMEOUT", 2*time.Second), "health probe timeout")
		earlyRecover = flag.Bool("probe-early-recovery", envBool("SWITCHYARD_PROBE_EARLY_RECOVERY", false),
			"let two passing health probes cut an open circuit's cooldown short (demo affordance; see DESIGN.md)")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	policy := router.Policy(*policyFlag)
	if !policy.Valid() {
		return fmt.Errorf("unknown policy %q: want failover or cost", *policyFlag)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tel, err := telemetry.New(ctx, version)
	if err != nil {
		return fmt.Errorf("start telemetry: %w", err)
	}
	defer func() {
		if err := tel.Shutdown(context.Background()); err != nil {
			log.Error("telemetry shutdown", "error", err)
		}
	}()

	// Priority is the failover order: apex first because it is the fastest and
	// most reliable, local last because it is the smallest. Cost routing
	// ignores this ordering entirely and uses it only to break ties.
	breakerCfg := breaker.DefaultConfig()
	breakerCfg.ProbeEarlyRecovery = *earlyRecover

	targets := []*router.Target{
		{Provider: provider.Apex(*seed), Priority: 10, Breaker: breaker.New(breakerCfg)},
		{Provider: provider.Bargain(*seed), Priority: 20, Breaker: breaker.New(breakerCfg)},
		{Provider: provider.Local(*seed), Priority: 30, Breaker: breaker.New(breakerCfg)},
	}

	rt := router.New(policy, tel.Observer(), targets...)
	if err := tel.RegisterGauges(rt); err != nil {
		return fmt.Errorf("register gauges: %w", err)
	}

	health := router.NewHealthChecker(rt, *probeEvery, *probeTimeout)
	go health.Run(ctx)

	gen := loadgen.New(rt, tel, log, loadgen.Config{RPS: *rps, Seed: *seed})
	go gen.Run(ctx)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           server.New(rt, tel, gen, log, version).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		// No WriteTimeout: responses are server-sent event streams that stay
		// open for as long as the completion takes, and a write deadline would
		// cut the slow provider off mid-answer for looking exactly like the
		// failure it is meant to demonstrate.
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("switchyard listening",
			"addr", *addr, "policy", policy, "rps", *rps, "version", version,
			"probe_early_recovery", *earlyRecover)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	return nil
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envUint(key string, def uint64) uint64 {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.ParseUint(v, 10, 64); err == nil {
			return i
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
