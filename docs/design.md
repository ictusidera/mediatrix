# Mediatrix Design

## Goal

Mediatrix hides physical connectivity behind logical names:

- `service:<name>` resolves to one or more provider peers for RPC.
- `file:sha256:<digest>` resolves to provider peers for file transfer.
- libp2p handles dialing, peer addresses, QUIC/TCP, relays, and connection reuse.
- Kad-DHT is used for provider discovery, not as a hand-written IP address book.

The target user should not need to know whether a request uses a direct QUIC path,
TCP fallback, or a relay path. The application API should look like:

```text
Call("service:tenant-a/calculator", "Add", {"a":1,"b":2})
Fetch("file:sha256:...", "./restore.tar.zst")
```

## Architecture

```text
cmd/mediatrixd
  Long-running node. Owns identity, DHT, stream handlers, service/file
  advertisement, peer observations, transfer state, and optional local HTTP
  control API.

cmd/mediatrix
  Operator/client CLI. Generates config, queries daemon state, publishes local
  services/files through the control API, runs one-shot calls, and fetches files.

internal/p2p
  libp2p host, Kad-DHT, provider discovery, RPC protocol, file protocol.

internal/config
  YAML configuration and defaults.

internal/identity
  Persistent libp2p private key.

internal/store
  SQLite state for peer cache, file index, services, and transfer history.
```

## Network Model

The recommended deployment has at least one public node:

- public bootstrap node
- public circuit relay v2 node
- optional AutoNAT helper role

Private nodes connect to bootstrap peers, join Kad-DHT, advertise local providers,
and use relay/DCUtR when direct connectivity is not available.

For native Go-to-Go nodes, STUN/TURN is not the first dependency. The first
production deployment should use libp2p relay and hole punching. TURN becomes a
separate concern if browser/WebRTC clients or non-libp2p peers are added.

## Configuration Boundaries

Runtime flags select commands and small overrides. Durable behavior belongs in
the YAML config:

- identity key path
- state database path
- listen multiaddrs
- bootstrap peers
- DHT namespace and mode
- relay client/service behavior
- local RPC services
- local file share roots
- security and limits

The sample implementation from the chat used positional arguments for nearly
everything. That is intentionally replaced with config because node behavior
needs to be reproducible under systemd, containers, and multiple environments.

## State Boundaries

The config contains desired static inputs. The state database contains observed
or generated runtime data:

- peer id and observed addresses
- discovered peer protocols
- last connection time
- relay reservation observations
- service advertisements
- local file index
- transfer history

DHT provider records are treated as discoverable network state with TTL, not as
the local source of truth.

## Protocols

Mediatrix reserves versioned protocol IDs:

- `/mediatrix/rpc/1.0.0`
- `/mediatrix/file/1.0.0`

RPC uses one JSON request and one JSON response per stream.

File transfer uses a JSON request, followed by a JSON header, followed by raw
file bytes. The receiver verifies the requested `file:sha256:<digest>` key after
download.

## Key Derivation

Kad-DHT provider discovery works with CIDs. Mediatrix derives deterministic CIDs
from logical provider keys:

```text
sha256("mediatrix:" + key)
```

The original logical key is still sent in the wire protocol, so the receiver can
validate behavior and maintain human-readable logs.

## Security Plan

The MVP demonstrates structure, not the final security boundary. Production work
should add:

- private network key support for isolated swarms
- signed service and file metadata
- per-service ACL
- peer allow/deny lists
- stronger request authentication for P2P RPC/file requests
- path traversal hardening for shared roots
- resource manager limits tuned for the deployment

The MVP already protects the daemon control API with a bearer token, keeps file
serving constrained to configured or explicitly published files, and verifies
fetched content hashes.

## Implementation Phases

1. MVP foundation
   - persistent identity
   - YAML config
   - SQLite state
   - libp2p host and Kad-DHT
   - RPC and file stream handlers

2. Connectivity hardening
   - relay reservation management
   - AutoNAT visibility
   - richer peer cache and retry policy
   - connection/resource limits

3. Product API
   - local daemon control API
   - library API for embedding applications
   - service registration without daemon restart

4. Security and operations
   - signed metadata
   - ACL and authentication
   - Prometheus metrics
   - Docker/systemd packaging
   - integration test harness with relay/private nodes
