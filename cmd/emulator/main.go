package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/arctex-one/emulator/internal/sim"
)

func main() {
	if err := run(); err != nil {
		slog.Error("Emulator stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		client := http.Client{Timeout: time.Second}
		resp, err := client.Get("http://127.0.0.1:51080/healthz")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("health status %d", resp.StatusCode)
		}
		return nil
	}
	data := flag.String("data", "/data/emulator.sqlite", "Persistent SQLite file")
	public := flag.String("listen", "127.0.0.1:51080", "Public API listen address")
	control := flag.String("control-listen", "127.0.0.1:51081", "Control API listen address")
	flag.Parse()
	if err := os.MkdirAll(filepath.Dir(*data), 0700); err != nil {
		return err
	}
	store, err := sim.Open(*data)
	if err != nil {
		return err
	}
	defer store.Close()
	token := os.Getenv("EMULATOR_CONTROL_TOKEN")
	if token == "" {
		token = "emulator-control"
	}
	api := sim.New(store, token)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.Handle("/", api.Public())
	servers := []*http.Server{{Addr: *public, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}, {Addr: *control, Handler: api.Control(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}}
	listeners := []net.Listener{}
	for _, server := range servers {
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			for _, l := range listeners {
				l.Close()
			}
			return err
		}
		listeners = append(listeners, listener)
	}
	failures := make(chan error, 3)
	for i, server := range servers {
		go func() {
			err := server.Serve(listeners[i])
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			failures <- err
		}()
	}
	go func() { failures <- api.RunWebhooks(ctx) }()
	slog.Info("Arctex development emulator ready", "public", *public, "control", *control)
	remaining := 3
	select {
	case <-ctx.Done():
	case err = <-failures:
		remaining--
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, server := range servers {
		if e := server.Shutdown(shutdown); e != nil && err == nil {
			err = e
		}
	}
	// Wait for callback persistence before closing SQLite.
	for i := 0; i < remaining; i++ {
		select {
		case e := <-failures:
			if e != nil && err == nil {
				err = e
			}
		case <-shutdown.Done():
			return shutdown.Err()
		}
	}
	return err
}
