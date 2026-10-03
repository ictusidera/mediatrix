// mediatrixd runs the persistent, operator-managed Mediatrix node.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ictusidera/mediatrix/internal/config"
	"github.com/ictusidera/mediatrix/internal/control"
	"github.com/ictusidera/mediatrix/internal/model"
	"github.com/ictusidera/mediatrix/internal/node"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr, logger); err != nil {
		logger.Error("daemon failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer, logger *slog.Logger) error {
	fs := flag.NewFlagSet("mediatrixd", flag.ContinueOnError)
	fs.SetOutput(out)
	path := fs.String("config", "", "required JSON configuration file")
	tokenFile := fs.String("token-file", "", "private API token file (otherwise MEDIATRIX_API_TOKEN)")
	version := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *version {
		_, err := fmt.Fprintln(out, model.Version)
		return err
	}
	if *path == "" {
		return errors.New("--config is required")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	token, err := config.LoadToken(*tokenFile)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.APIAddr)
	if err != nil {
		return fmt.Errorf("listen for control API: %w", err)
	}
	defer listener.Close()
	// Cancel interrupted startup promptly, but once serving, keep the node alive
	// while Shutdown drains accepted API requests.
	nodeCtx, cancelNode := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelNode()
	startupDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			cancelNode()
		case <-startupDone:
		}
	}()
	n, err := node.New(nodeCtx, cfg, logger)
	close(startupDone)
	if err != nil {
		return fmt.Errorf("start node: %w", err)
	}
	defer n.Close()
	server, err := control.New(cfg.APIAddr, token, n, cfg.Limits.MaxJSONBytes, cfg.Limits.MaxFileBytes, cfg.Limits.MaxConcurrent, time.Duration(cfg.Limits.TimeoutSeconds)*time.Second)
	if err != nil {
		return fmt.Errorf("configure control API: %w", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	info := n.Info()
	logger.Info("daemon started", "version", model.Version, "peer_id", info.PeerID, "api_addr", listener.Addr().String(), "network", cfg.Network, "local_only", len(cfg.AllowedPeers) == 0)
	var serveFailure error
	select {
	case <-ctx.Done():
		logger.Info("shutdown requested")
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveFailure = fmt.Errorf("control API: %w", err)
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		return fmt.Errorf("control API shutdown: %w", err)
	}
	logger.Info("daemon stopped")
	return serveFailure
}
