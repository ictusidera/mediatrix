# C# client contract tests

A standalone console test harness, without a unit-test framework dependency. It uses a raw TCP loopback HTTP server so it can deliberately return malformed JSON, truncated bodies, redirects, chunked data, and stalled response streams. Every case is named, counted, and returns a nonzero process exit code on failure.

The project targets `net8.0` and `net462`. Both reference the **same built `net462` Mediatrix.Client.dll** via `HintPath`; neither rebuilds or substitutes a net8.0 SDK client. The test checks the loaded assembly's .NET Framework 4.6.2 target, CLR v4 image metadata, and AnyCPU IL-only flags.

## Build and run

From the repository root, with the .NET 8 SDK installed:

```sh
dotnet build bindings/csharp/src/Mediatrix.Client -c Release
dotnet build bindings/csharp/tests/Mediatrix.Client.Tests -c Release -m:1
dotnet run --project bindings/csharp/tests/Mediatrix.Client.Tests -c Release -f net8.0 --no-build
```

If MSBuild worker or compiler-server processes are unavailable in a constrained environment, add `-m:1 /p:UseSharedCompilation=false` to build commands.

On Windows with .NET Framework 4.6.2 or later installed, run the framework-targeted harness directly:

```powershell
.\bindings\csharp\tests\Mediatrix.Client.Tests\bin\Release\net462\Mediatrix.Client.Tests.exe
```

### Runtime qualification

A Linux `net8.0` pass is **not** a Windows/.NET Framework runtime certification. The SDK under test is the delivered .NET Framework assembly, but Linux tests use Json.NET 13.0.4's host-compatible `net6.0` dependency. The `net462` harness selects Json.NET's `net45` assembly, matching the shipped dependency. The framework harness is compiled against .NET Framework 4.6.2 reference assemblies; it must also be executed on Windows to qualify that runtime.

The runner prints both loaded assembly locations/targets. Use SHA-256 comparisons if validating a release bundle against the tested client DLL.

## Coverage

- Numeric loopback URLs, explicit ports, unsafe URL variants, API token length/character limits, option limits, service/key/method/request-ID checks, strict parameter JSON
- Bearer auth, snake_case request/response fields, HTTP 204 mutation responses, exact large-number JSON and successful `result:null`
- HTTP API errors versus HTTP 200 application errors; invalid/missing/mismatched IDs; duplicate/invalid JSON; strict health/node/service/file schemas; case-shadow fields
- Unknown mutation outcomes and no automatic replay after malformed/truncated/disconnected responses
- Cancellation before send, stalled JSON/binary body cancellation, request deadlines, nonseekable upload cancellation, and ownership of caller streams
- Redirect refusal and proxy-environment avoidance; no cookie forwarding
- Seekable/nonseekable/empty binary upload, stream position, SHA-256 and size receipts, streaming limits, fetch metadata, ACL serialization
- Stream and file download, exact bytes/hash/size, malformed content types/encoding, chunked limits, temporary-file cleanup, existing-file refusal, and no-overwrite races
- 24 simultaneous RPC calls and eight simultaneous binary downloads on one shared client

The suite uses ephemeral loopback ports, caller-owned in-memory streams, and per-test temporary directories. It restores proxy environment variables after its proxy test. Test content and the hard-coded loopback token are disposable fixtures, not credentials for any real daemon.

## Optional real-daemon integration

Start a dedicated disposable daemon first. Set its API token through `MEDIATRIX_API_TOKEN` in the test process environment, then run:

```sh
dotnet run --project bindings/csharp/tests/Mediatrix.Client.Tests -c Release -f net8.0 --no-build -- --daemon http://127.0.0.1:47832
```

Do not put the token in arguments, URLs, or logs. The harness starts its own ephemeral loopback handler, registers a uniquely named test service, verifies real health/node/service/RPC/application-error/file/ACL/download behavior, and removes that service in cleanup. It imports deterministic 300,123-byte test content into the daemon's local store. That file remains in the daemon store, so use a disposable daemon data directory.

The real-daemon scenario is reported as one additional case, with multiple assertions. The ordinary contract suite always runs first.
