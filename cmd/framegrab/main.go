// framegrab serves frames of the films and series in a Jellyfin or Plex
// style library. Configuration is read from the environment (README).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Pale-whale/framegrab/internal/api"
	"github.com/Pale-whale/framegrab/internal/library"
	"github.com/Pale-whale/framegrab/internal/media"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func paths(key string) []string {
	var out []string
	for _, p := range strings.Split(os.Getenv(key), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	movies, shows := paths("FRAMEGRAB_MOVIES"), paths("FRAMEGRAB_SHOWS")
	if len(movies)+len(shows) == 0 {
		return errors.New("set FRAMEGRAB_MOVIES and/or FRAMEGRAB_SHOWS")
	}
	interval, err := time.ParseDuration(env("FRAMEGRAB_SCAN_INTERVAL", "1h"))
	if err != nil {
		return fmt.Errorf("FRAMEGRAB_SCAN_INTERVAL: %w", err)
	}
	timeout, err := time.ParseDuration(env("FRAMEGRAB_FRAME_TIMEOUT", "60s"))
	if err != nil {
		return fmt.Errorf("FRAMEGRAB_FRAME_TIMEOUT: %w", err)
	}
	concurrency, err := strconv.Atoi(env("FRAMEGRAB_CONCURRENCY", "2"))
	if err != nil || concurrency < 1 {
		return errors.New("FRAMEGRAB_CONCURRENCY: expected a whole number from 1")
	}
	maxFrames, err := strconv.Atoi(env("FRAMEGRAB_MAX_FRAMES", "32"))
	if err != nil || maxFrames < 1 {
		return errors.New("FRAMEGRAB_MAX_FRAMES: expected a whole number from 1")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	lib := library.New(movies, shows)
	tool := media.New(env("FRAMEGRAB_FFMPEG", "ffmpeg"), env("FRAMEGRAB_FFPROBE", "ffprobe"), concurrency, timeout)
	rescan := make(chan struct{}, 1)
	go func() {
		var tick <-chan time.Time
		if interval > 0 {
			t := time.NewTicker(interval)
			defer t.Stop()
			tick = t.C
		}
		for {
			lib.Scan(ctx)
			select {
			case <-ctx.Done():
				return
			case <-tick:
			case <-rescan:
			}
		}
	}()
	srv := api.New(lib, tool, maxFrames, func() {
		select {
		case rescan <- struct{}{}:
		default: // one is already queued
		}
	})

	addr := env("FRAMEGRAB_LISTEN", ":8080")
	hs := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	slog.Info("listening", "addr", addr, "movies", movies, "shows", shows,
		"concurrency", concurrency, "max_frames", maxFrames)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return hs.Shutdown(shutdown)
}
