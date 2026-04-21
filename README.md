# Mediatrix

Mediatrix is a Go/libp2p based prototype for logical-name RPC and content-keyed
file transfer over a P2P network.

The first implementation focuses on a practical MVP:

- persistent libp2p identity
- YAML based node configuration
- Kad-DHT provider discovery
- versioned RPC and file transfer protocols
- SQLite state for peer observations and transfer/index metadata
- optional public relay/bootstrap node role

## Build

```powershell
go build ./cmd/mediatrix
go build ./cmd/mediatrixd
```

## Quick Start

Create a config:

```powershell
go run ./cmd/mediatrix init --config config.yaml
```

Run a bootstrap/relay node on a public machine:

```powershell
go run ./cmd/mediatrixd --config relay.yaml --role relay
```

Run a private node with services/files defined in its config:

```powershell
go run ./cmd/mediatrixd --config node-a.yaml
```

Optionally enable the local control API:

```powershell
go run ./cmd/mediatrixd --config node-a.yaml --api 127.0.0.1:8080 --api-token dev-secret
```

Then register providers without restarting the daemon:

```powershell
go run ./cmd/mediatrix publish-service --config node-a.yaml `
  --api 127.0.0.1:8080 --token dev-secret `
  --name service:echo builtin:echo

go run ./cmd/mediatrix publish-file --config node-a.yaml `
  --api 127.0.0.1:8080 --token dev-secret `
  --path C:\path\to\sample.bin --name sample.bin
```

Call a logical service from another node:

```powershell
go run ./cmd/mediatrix call --config node-b.yaml --service service:echo --method Echo --params '{"message":"hello"}'
```

Fetch a file by content key:

```powershell
go run ./cmd/mediatrix fetch --config node-b.yaml --key file:sha256:<digest> --out ./download.bin
```

See [docs/design.md](docs/design.md) for the design and production plan.
