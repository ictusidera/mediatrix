package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/s-yamamoto/mediatrix/internal/config"
	"github.com/s-yamamoto/mediatrix/internal/control"
	"github.com/s-yamamoto/mediatrix/internal/p2p"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "config file path")
	role := flag.String("role", "node", "node role: node or relay")
	apiAddr := flag.String("api", "", "optional HTTP control API address override, e.g. 127.0.0.1:8080")
	apiToken := flag.String("api-token", "", "optional HTTP control API bearer token override")
	advertiseInterval := flag.Duration("advertise-interval", 10*time.Minute, "provider re-advertise interval")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	node, err := p2p.New(ctx, cfg, p2p.Options{Role: *role})
	if err != nil {
		log.Fatalf("start node: %v", err)
	}
	defer node.Close()

	if err := node.RegisterConfigured(ctx); err != nil {
		log.Fatalf("register configured providers: %v", err)
	}

	fmt.Printf("peer: %s\n", node.PeerID())
	for _, addr := range node.ListenAddrs() {
		fmt.Printf("addr: %s\n", addr)
	}
	if hint := p2p.LocalIPHint(); hint != "" {
		fmt.Printf("local-ip-hint: %s\n", hint)
	}

	go node.AdvertiseLoop(ctx, *advertiseInterval)
	controlAddr := cfg.Control.Addr
	if *apiAddr != "" {
		controlAddr = *apiAddr
	}
	controlToken := cfg.Control.Token
	if *apiToken != "" {
		controlToken = *apiToken
	}
	if controlAddr != "" {
		api := control.New(controlAddr, controlToken, node)
		go func() {
			if err := api.ListenAndServe(); err != nil && err.Error() != "http: Server closed" {
				log.Printf("control api stopped: %v", err)
				stop()
			}
		}()
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = api.Shutdown(shutdownCtx)
		}()
		fmt.Printf("api: http://%s\n", controlAddr)
	}
	<-ctx.Done()
}
