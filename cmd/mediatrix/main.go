package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/s-yamamoto/mediatrix/internal/config"
	"github.com/s-yamamoto/mediatrix/internal/control"
	"github.com/s-yamamoto/mediatrix/internal/p2p"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "init":
		runInit(os.Args[2:])
	case "id":
		runID(os.Args[2:])
	case "call":
		runCall(os.Args[2:])
	case "fetch":
		runFetch(os.Args[2:])
	case "node":
		runNode(os.Args[2:])
	case "publish-service":
		runPublishService(os.Args[2:])
	case "publish-file":
		runPublishFile(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func runInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	cfgPath := fs.String("config", "config.yaml", "config file path")
	_ = fs.Parse(args)
	if err := config.WriteDefault(*cfgPath); err != nil {
		log.Fatalf("write config: %v", err)
	}
	fmt.Printf("wrote %s\n", *cfgPath)
}

func runID(args []string) {
	fs := flag.NewFlagSet("id", flag.ExitOnError)
	cfgPath := fs.String("config", "config.yaml", "config file path")
	_ = fs.Parse(args)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	node := mustNode(ctx, *cfgPath)
	defer node.Close()
	fmt.Printf("peer: %s\n", node.PeerID())
	for _, addr := range node.ListenAddrs() {
		fmt.Printf("addr: %s\n", addr)
	}
}

func runCall(args []string) {
	fs := flag.NewFlagSet("call", flag.ExitOnError)
	cfgPath := fs.String("config", "config.yaml", "config file path")
	service := fs.String("service", "", "logical service name, e.g. service:echo")
	method := fs.String("method", "", "RPC method")
	params := fs.String("params", "{}", "JSON params string")
	timeout := fs.Duration("timeout", 45*time.Second, "call timeout")
	_ = fs.Parse(args)
	if *service == "" || *method == "" {
		log.Fatal("call requires --service and --method")
	}
	if !json.Valid([]byte(*params)) {
		log.Fatal("--params must be valid JSON")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	node := mustNode(ctx, *cfgPath)
	defer node.Close()
	resp, err := node.Call(ctx, *service, *method, *params)
	if err != nil {
		log.Fatalf("call failed: %v", err)
	}
	if !resp.OK {
		log.Fatalf("remote error: %s", resp.Error)
	}
	fmt.Println(resp.Result)
}

func runFetch(args []string) {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	cfgPath := fs.String("config", "config.yaml", "config file path")
	key := fs.String("key", "", "file key, e.g. file:sha256:<digest>")
	out := fs.String("out", "", "output path")
	timeout := fs.Duration("timeout", 5*time.Minute, "fetch timeout")
	_ = fs.Parse(args)
	if *key == "" || *out == "" {
		log.Fatal("fetch requires --key and --out")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	node := mustNode(ctx, *cfgPath)
	defer node.Close()
	if err := node.FetchFile(ctx, *key, *out); err != nil {
		log.Fatalf("fetch failed: %v", err)
	}
	fmt.Printf("wrote %s\n", *out)
}

func runNode(args []string) {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	cfgPath := fs.String("config", "config.yaml", "config file path")
	apiAddr := fs.String("api", "", "control API address override")
	token := fs.String("token", "", "control API bearer token override")
	timeout := fs.Duration("timeout", 10*time.Second, "request timeout")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client := mustControlClient(*cfgPath, *apiAddr, *token)
	info, err := client.Node(ctx)
	if err != nil {
		log.Fatalf("node request failed: %v", err)
	}
	fmt.Printf("peer: %s\n", info.PeerID)
	for _, addr := range info.Addrs {
		fmt.Printf("addr: %s\n", addr)
	}
}

func runPublishService(args []string) {
	fs := flag.NewFlagSet("publish-service", flag.ExitOnError)
	cfgPath := fs.String("config", "config.yaml", "config file path")
	apiAddr := fs.String("api", "", "control API address override")
	token := fs.String("token", "", "control API bearer token override")
	name := fs.String("name", "", "logical service name, e.g. service:echo")
	timeout := fs.Duration("timeout", 30*time.Second, "request timeout")
	_ = fs.Parse(args)
	command := fs.Args()
	if *name == "" || len(command) == 0 {
		log.Fatal("publish-service requires --name and a command")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client := mustControlClient(*cfgPath, *apiAddr, *token)
	published, err := client.PublishService(ctx, *name, command)
	if err != nil {
		log.Fatalf("publish service failed: %v", err)
	}
	fmt.Printf("published service: %s\n", published)
}

func runPublishFile(args []string) {
	fs := flag.NewFlagSet("publish-file", flag.ExitOnError)
	cfgPath := fs.String("config", "config.yaml", "config file path")
	apiAddr := fs.String("api", "", "control API address override")
	token := fs.String("token", "", "control API bearer token override")
	path := fs.String("path", "", "file path")
	name := fs.String("name", "", "published file name")
	timeout := fs.Duration("timeout", 30*time.Second, "request timeout")
	_ = fs.Parse(args)
	if *path == "" {
		log.Fatal("publish-file requires --path")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client := mustControlClient(*cfgPath, *apiAddr, *token)
	key, err := client.PublishFile(ctx, *path, *name)
	if err != nil {
		log.Fatalf("publish file failed: %v", err)
	}
	fmt.Printf("published file: %s\n", key)
}

func mustNode(ctx context.Context, cfgPath string) *p2p.Node {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	node, err := p2p.New(ctx, cfg, p2p.Options{})
	if err != nil {
		log.Fatalf("start node: %v", err)
	}
	return node
}

func mustControlClient(cfgPath, apiAddr, token string) *control.Client {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if apiAddr == "" {
		apiAddr = cfg.Control.Addr
	}
	if token == "" {
		token = cfg.Control.Token
	}
	if apiAddr == "" {
		log.Fatal("control API address is required")
	}
	return control.NewClient(apiAddr, token)
}

func usage() {
	fmt.Fprintln(os.Stderr, strings.TrimSpace(`
usage:
  mediatrix init  --config config.yaml
  mediatrix id    --config config.yaml
  mediatrix node  --config config.yaml
  mediatrix publish-service --config config.yaml --name service:echo builtin:echo
  mediatrix publish-file    --config config.yaml --path ./sample.bin
  mediatrix call  --config config.yaml --service service:echo --method Echo --params '{"message":"hello"}'
  mediatrix fetch --config config.yaml --key file:sha256:<digest> --out ./download.bin
`))
}
