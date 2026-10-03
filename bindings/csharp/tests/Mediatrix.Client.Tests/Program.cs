using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.Linq;
using System.Net;
using System.Reflection;
using System.Runtime.Versioning;
using System.Security.Cryptography;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using Mediatrix;
using Newtonsoft.Json;
using Newtonsoft.Json.Linq;

namespace Mediatrix.Client.Tests
{
    internal static class Program
    {
        private const string Token = "contract-test-only-0123456789abcdef0123456789abcdef";
        private const string Service = "service:contract";
        private const string Id = "contract-id";
        private static readonly byte[] Payload = Enumerable.Range(0, 300123).Select(n => (byte)(n * 37)).ToArray();
        private static readonly string Key = FileKey(Payload);
        private static int passed, failed;
        private static readonly List<Tuple<string, Func<Task>>> Tests = new List<Tuple<string, Func<Task>>>();

        public static int Main(string[] args)
        {
            try { return Run(args).GetAwaiter().GetResult(); }
            catch (Exception ex) { Console.Error.WriteLine(ex); return 2; }
        }
        private static async Task<int> Run(string[] args)
        {
            Console.WriteLine("Host: " + Environment.Version + " / " + Environment.OSVersion);
            Console.WriteLine("Client: " + typeof(MediatrixClient).Assembly.Location);
            Console.WriteLine("Client target: " + Framework(typeof(MediatrixClient).Assembly));
            Console.WriteLine("Json.NET target: " + Framework(typeof(JsonConvert).Assembly));
            Add("exact .NET Framework 4.6.2 client assembly", () => { Equal(".NETFramework,Version=v4.6.2", Framework(typeof(MediatrixClient).Assembly)); return Task.CompletedTask; });
            Add("CLR v4 image metadata", () => { Equal("v4.0.30319", typeof(MediatrixClient).Assembly.ImageRuntimeVersion); return Task.CompletedTask; });
            Add("AnyCPU IL-only image metadata", () => {
                PortableExecutableKinds kind; ImageFileMachine machine;
                typeof(MediatrixClient).Assembly.ManifestModule.GetPEKind(out kind, out machine);
                Equal(ImageFileMachine.I386, machine);
                Check((kind & PortableExecutableKinds.ILOnly) != 0 && (kind & PortableExecutableKinds.Required32Bit) == 0, "Client is not AnyCPU IL-only.");
                return Task.CompletedTask;
            });
            AddValidation(); AddJsonAndRpc(); AddStrictStatusSchemas(); AddCancellationAndTransport(); AddFiles(); AddConcurrency();
            if (args.Length != 0)
            {
                if (args.Length != 2 || args[0] != "--daemon") throw new ArgumentException("Usage: [--daemon http://127.0.0.1:PORT]; real-daemon token comes only from MEDIATRIX_API_TOKEN.");
                Add("real daemon: registration, RPC, errors, files, ACL, cleanup", () => RealDaemon(args[1]));
            }
            foreach (var test in Tests)
            {
                var timer = Stopwatch.StartNew();
                try
                {
                    Task operation = test.Item2();
                    if (await Task.WhenAny(operation, Task.Delay(15000)).ConfigureAwait(false) != operation) throw new TimeoutException("Test did not finish in 15 seconds.");
                    await operation.ConfigureAwait(false);
                    passed++; Console.WriteLine("PASS " + test.Item1 + " (" + timer.ElapsedMilliseconds + " ms)");
                }
                catch (Exception ex) { failed++; Console.WriteLine("FAIL " + test.Item1 + ": " + ex); }
            }
            Console.WriteLine("RESULT: " + passed + " passed, " + failed + " failed, " + (passed + failed) + " cases");
            Console.WriteLine("The net8.0 run verifies the actual net462 client DLL with host-compatible Json.NET. It does not verify Windows/.NET Framework runtime behavior.");
            return failed == 0 ? 0 : 1;
        }
        private static string Framework(Assembly assembly)
        { var attribute = assembly.GetCustomAttribute<TargetFrameworkAttribute>(); return attribute == null ? "unknown" : attribute.FrameworkName; }
        private static void Add(string name, Func<Task> test) { Tests.Add(Tuple.Create(name, test)); }
        private static MediatrixClient NewClient(string url = "http://127.0.0.1:9", int timeoutMs = 2500, int maxJson = 16 * 1024 * 1024, long maxFile = 1024L * 1024 * 1024)
        { return new MediatrixClient(Token, new MediatrixClientOptions { ApiUrl = url, RequestTimeout = TimeSpan.FromMilliseconds(timeoutMs), MaxJsonBytes = maxJson, MaxFileBytes = maxFile }); }
        private static async Task WithServer(Func<Request, Response> handler, Func<MediatrixClient, LoopbackServer, Task> test, int timeoutMs = 2500, int maxJson = 16 * 1024 * 1024, long maxFile = 1024L * 1024 * 1024)
        {
            using (var server = new LoopbackServer(handler))
            using (var client = NewClient(server.Url, timeoutMs, maxJson, maxFile))
            { await test(client, server).ConfigureAwait(false); Check(server.Errors.IsEmpty, "Test server handler failed: " + string.Join("; ", server.Errors.Select(e => e.Message))); }
        }
        private static Task WithResponse(Response response, Func<MediatrixClient, LoopbackServer, Task> test, int timeoutMs = 2500, int maxJson = 16 * 1024 * 1024, long maxFile = 1024L * 1024 * 1024)
        { return WithServer(r => response, test, timeoutMs, maxJson, maxFile); }
        private static async Task<T> Throws<T>(Func<Task> operation) where T : Exception
        {
            try { await operation().ConfigureAwait(false); }
            catch (T ex) { return ex; }
            catch (Exception ex) { throw new Exception("Expected " + typeof(T).Name + ", got " + ex.GetType().Name, ex); }
            throw new Exception("Expected " + typeof(T).Name + ", but operation succeeded.");
        }
        private static void Check(bool condition, string detail = "Assertion failed.") { if (!condition) throw new Exception(detail); }
        private static void Equal<T>(T expected, T actual) { if (!EqualityComparer<T>.Default.Equals(expected, actual)) throw new Exception("Expected [" + expected + "]; got [" + actual + "]"); }
        private static void Bytes(byte[] expected, byte[] actual) { Check(expected.SequenceEqual(actual), "Byte-for-byte content differs."); }
        private static string FileKey(byte[] bytes) { using (SHA256 sha = SHA256.Create()) return "file:sha256:" + BitConverter.ToString(sha.ComputeHash(bytes)).Replace("-", "").ToLowerInvariant(); }
        private static Request Only(LoopbackServer server) { Equal(1, server.Count); return server.Requests.Single(); }
        private static string Receipt(string key, long size) { return "{\"key\":" + JsonConvert.SerializeObject(key) + ",\"size\":" + size + ",\"allowed_peers\":[]}"; }
        private static string Rpc(string result, string requestId = Id) { return "{\"request_id\":" + JsonConvert.SerializeObject(requestId) + ",\"result\":" + result + "}"; }

        private static void AddValidation()
        {
            string[] invalidUrls = { "http://localhost:47832", "https://127.0.0.1:47832", "http://192.0.2.1:47832", "http://0.0.0.0:47832", "http://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:65536", "http://127.0.0.1:47832/v1", "http://127.0.0.1:47832?x=1", "http://127.0.0.1:47832/#fragment", "http://user@127.0.0.1:47832", "http://127.1:47832", "http://2130706433:47832", "http://0177.0.0.1:47832", "http://127.0.0.1:47832\\evil", " http://127.0.0.1:47832", "http://[::]:47832", "http://[2001:db8::1]:47832" };
            foreach (string url in invalidUrls) Add("reject API URI " + url, () => Throws<ArgumentException>(() => { using (NewClient(url)) { } return Task.CompletedTask; }));
            foreach (string url in new[] { "http://127.0.0.1:47832", "http://127.0.0.1:47832/", "http://127.12.34.56:47832", "http://[::1]:47832" })
                Add("accept numeric loopback URI " + url, () => { using (NewClient(url)) { } return Task.CompletedTask; });
            var invalidTokens = new[] { null, "", new string('a', 31), new string('a', 4097), new string('a', 32) + " ", new string('a', 32) + "\r\nInjected:yes", new string('a', 32) + "\t", new string('a', 32) + "é", new string('a', 32) + "\u007f" };
            for (int i = 0; i < invalidTokens.Length; i++) { string value = invalidTokens[i]; int row = i; Add("reject API token case " + row, () => Throws<ArgumentException>(() => { using (new MediatrixClient(value)) { } return Task.CompletedTask; })); }
            foreach (int length in new[] { 32, 4096 }) Add("accept token length " + length, () => { using (new MediatrixClient(new string('a', length))) { } return Task.CompletedTask; });
            Add("options invalid minimum timeout", () => Throws<ArgumentOutOfRangeException>(() => { using (NewClient(timeoutMs: 0)) { } return Task.CompletedTask; }));
            Add("options invalid maximum timeout", () => Throws<ArgumentOutOfRangeException>(() => { using (NewClient(timeoutMs: 310001)) { } return Task.CompletedTask; }));
            Add("options invalid JSON cap", () => Throws<ArgumentOutOfRangeException>(() => { using (NewClient(maxJson: 0)) { } return Task.CompletedTask; }));
            Add("options invalid file cap", () => Throws<ArgumentOutOfRangeException>(() => { using (NewClient(maxFile: 0)) { } return Task.CompletedTask; }));
            foreach (string service in new[] { "demo", "service:", "service:-bad", "service:bad?query", "service:" + new string('a', 129) })
                Add("reject service " + service, async () => { using (var c = NewClient()) await Throws<ArgumentException>(() => c.CallAsync(service, "echo")); });
            foreach (string json in new[] { "{", "NaN", "Infinity", "01", "+1", "1.", "1e", "{}{}", "/*x*/null", "{\"x\":1,\"x\":2}", "[1,]", "\"bad\nstring\"", "\"\\x20\"" })
                Add("reject non-JSON params " + json.Replace("\n", "\\n"), async () => { using (var c = NewClient()) await Throws<FormatException>(() => c.CallAsync(Service, "echo", json)); });
            Add("reject excessive params depth", async () => { using (var c = NewClient()) await Throws<FormatException>(() => c.CallAsync(Service, "echo", new string('[', 66) + "0" + new string(']', 66))); });
            Add("reject invalid method", async () => { using (var c = NewClient()) await Throws<ArgumentException>(() => c.CallAsync(Service, "bad\nmethod")); });
            Add("reject overlong UTF-8 method", async () => { using (var c = NewClient()) await Throws<ArgumentException>(() => c.CallAsync(Service, new string('é', 65))); });
            Add("reject invalid request ID", async () => { using (var c = NewClient()) await Throws<ArgumentException>(() => c.CallAsync(Service, "echo", "null", "id\0")); });
            Add("reject daemon timeout outside range", async () => { using (var c = NewClient()) await Throws<ArgumentOutOfRangeException>(() => c.CallAsync(Service, "echo", timeoutMilliseconds: 300001)); });
            Add("reject DNS handler URL", async () => { using (var c = NewClient()) await Throws<ArgumentException>(() => c.RegisterServiceAsync(Service, "http://localhost:9000/rpc")); });
            Add("reject empty ACL peer", async () => { using (var c = NewClient()) await Throws<ArgumentException>(() => c.SetFileAccessAsync(Key, new[] { "" })); });
            Add("reject uppercase file digest", async () => { using (var c = NewClient()) await Throws<ArgumentException>(() => c.FetchAsync(Key.ToUpperInvariant())); });
            Add("reject oversized JSON request before send", () => WithResponse(Response.Json(Rpc("0")), async (c, s) => { await Throws<ArgumentException>(() => c.CallAsync(Service, "echo", "\"" + new string('a', 100) + "\"")); Equal(0, s.Count); }, maxJson: 64));
        }

        private static void AddJsonAndRpc()
        {
            Add("health and bearer authentication", () => WithResponse(Response.Json("{\"status\":\"ok\"}"), async (c, s) => {
                Check(await c.HealthAsync()); Request r = Only(s); Equal("GET", r.Method); Equal("/v1/health", r.Target); Equal("Bearer " + Token, r.Headers["Authorization"]); Check(!r.Target.Contains(Token)); Check(!r.Headers.ContainsKey("Cookie")); }));
            Add("node snake_case decoding", () => WithResponse(Response.Json("{\"version\":\"0.2.0\",\"peer_id\":\"peer\",\"addrs\":[\"/ip4/127.0.0.1/tcp/1\"],\"connected_peers\":2,\"services\":3,\"files\":4,\"network\":\"test\"}"), async (c, s) => { NodeInfo n = await c.GetNodeAsync(); Equal("peer", n.PeerId); Equal(2, n.ConnectedPeers); Equal(1, n.Addresses.Length); Equal(3, n.Services); Equal(4, n.Files); Equal("test", n.Network); Equal("/v1/node", Only(s).Target); }));
            Add("services snake_case decoding", () => WithResponse(Response.Json("[{\"name\":\"service:contract\",\"url\":\"http://127.0.0.1:9000/rpc\",\"allowed_peers\":[\"p\"]}]"), async (c, s) => { ServiceInfo[] items = await c.GetServicesAsync(); Equal(1, items.Length); Equal("p", items[0].AllowedPeers[0]); Equal("/v1/services", Only(s).Target); }));
            Add("register 204, handler path and full empty ACL", () => WithResponse(Response.NoContent(), async (c, s) => { await c.RegisterServiceAsync(Service, "http://127.0.0.1:9000/rpc/nested"); Request r = Only(s); Equal("PUT", r.Method); Equal("/v1/services", r.Target); JObject body = JObject.Parse(r.Text); Equal(Service, (string)body["name"]); Equal(0, ((JArray)body["allowed_peers"]).Count); Check(body["allowedPeers"] == null); Check(r.Headers["Content-Type"].StartsWith("application/json", StringComparison.Ordinal)); }));
            Add("remove 204 and escaped service query", () => WithResponse(Response.NoContent(), async (c, s) => { await c.RemoveServiceAsync("service:folder/demo"); Request r = Only(s); Equal("DELETE", r.Method); Equal("/v1/services?name=service%3Afolder%2Fdemo", r.Target); }));
            Add("file ACL 204 and snake_case", () => WithResponse(Response.NoContent(), async (c, s) => { await c.SetFileAccessAsync(Key, new[] { "peer-a" }); Request r = Only(s); Equal("PUT", r.Method); Equal("/v1/files/access", r.Target); JObject body = JObject.Parse(r.Text); Equal(Key, (string)body["key"]); Equal("peer-a", (string)body["allowed_peers"][0]); }));
            Add("RPC result null remains successful literal", () => WithResponse(Response.Json(Rpc("null")), async (c, s) => { RpcResponse r = await c.CallAsync(Service, "echo", requestId: Id); Check(r.IsSuccess); Check(r.Error == null); Equal("null", r.ResultJson); Equal<object>(null, r.DeserializeResult<object>()); r.ThrowIfError(); }));
            Add("RPC exact very large numbers and request serialization", () => {
                const string value = "{\"integer\":12345678901234567890123456789012345678901234567890,\"decimal\":0.123456789012345678901234567890123456789,\"exponent\":1.234567890123456789e+200,\"date\":\"2030-01-02T03:04:05Z\"}";
                return WithResponse(Response.Json(Rpc(value)), async (c, s) => { RpcResponse r = await c.CallAsync(Service, "echo", value, Id, 1234); Equal(value, r.ResultJson); Request request = Only(s); Check(request.Text.Contains(value), "Parameters were normalized or rounded."); JObject body = JObject.Parse(request.Text); Equal(Id, (string)body["request_id"]); Equal(1234, (int)body["timeout_ms"]); Check(body["requestId"] == null && body["timeoutMilliseconds"] == null); }); });
            Add("RPC application error is distinct from API error", () => WithResponse(Response.Json("{\"request_id\":\"contract-id\",\"error\":{\"code\":\"invalid\",\"message\":\"operation rejected\"}}"), async (c, s) => { RpcResponse r = await c.CallAsync(Service, "fail", requestId: Id); Check(!r.IsSuccess); Equal<string>(null, r.ResultJson); Equal("invalid", r.Error.Code); var ex = await Throws<MediatrixApplicationException>(() => { r.ThrowIfError(); return Task.CompletedTask; }); Equal(Id, ex.RequestId); Check(!ex.OutcomeUnknown); Only(s); }));
            Add("API status and server outcome_unknown preserved", () => WithResponse(Response.Json("{\"error\":{\"code\":\"timeout\",\"message\":\"deadline\",\"outcome_unknown\":true}}", 504), async (c, s) => { var ex = await Throws<MediatrixApiException>(() => c.CallAsync(Service, "echo")); Equal(HttpStatusCode.GatewayTimeout, ex.StatusCode); Equal("timeout", ex.Code); Check(ex.OutcomeUnknown); Only(s); }));
            Add("API rejected mutation known failure", () => WithResponse(Response.Json("{\"error\":{\"code\":\"unauthorized\",\"message\":\"denied\"}}", 401), async (c, s) => { var ex = await Throws<MediatrixApiException>(() => c.CallAsync(Service, "echo")); Check(!ex.OutcomeUnknown); Equal(HttpStatusCode.Unauthorized, ex.StatusCode); Only(s); }));
            string[] badRpc = { "{", "[]", "{}", "{\"result\":1}", "{\"request_id\":\"wrong\",\"result\":1}", "{\"request_id\":\"contract-id\"}", "{\"request_id\":\"contract-id\",\"result\":null,\"error\":{\"code\":\"x\",\"message\":\"m\"}}", "{\"request_id\":\"contract-id\",\"result\":NaN}", "{\"request_id\":\"contract-id\",\"result\":1,\"result\":2}", "{\"request_id\":\"contract-id\",\"error\":null}", "{\"request_id\":\"contract-id\",\"error\":{\"code\":3,\"message\":\"m\"}}", "{\"request_id\":\"contract-id\",\"error\":{\"code\":\"x\",\"message\":null}}", "{\"request_id\":\"contract-id\",\"error\":{\"code\":\"x\",\"message\":\"m\",\"outcome_unknown\":\"true\"}}", "{\"request_id\":\"contract-id\",\"result\":1}{}" };
            for (int i = 0; i < badRpc.Length; i++) { string json = badRpc[i]; int row = i; Add("malformed RPC outcome unknown and no retry " + row, () => WithResponse(Response.Json(json), async (c, s) => { var ex = await Throws<MediatrixProtocolException>(() => c.CallAsync(Service, "echo", requestId: Id)); Check(ex.OutcomeUnknown); Only(s); })); }
            Add("numeric response request ID rejected", () => WithResponse(Response.Json("{\"request_id\":123,\"result\":null}"), async (c, s) => { var ex = await Throws<MediatrixProtocolException>(() => c.CallAsync(Service, "echo")); Check(ex.OutcomeUnknown); Only(s); }));
            Add("malformed API error outcome unknown", () => WithResponse(Response.Json("{\"Error\":{\"code\":\"denied\",\"message\":\"no\"}}", 400), async (c, s) => { var ex = await Throws<MediatrixProtocolException>(() => c.CallAsync(Service, "echo")); Check(ex.OutcomeUnknown); Only(s); }));
            Add("wrong JSON content type rejected", () => WithResponse(new Response { ContentType = "text/plain", Body = Encoding.UTF8.GetBytes(Rpc("1")) }, async (c, s) => { var ex = await Throws<MediatrixProtocolException>(() => c.CallAsync(Service, "echo")); Check(ex.OutcomeUnknown); Only(s); }));
            Add("compressed JSON rejected", () => { Response response = Response.Json(Rpc("1")); response.Headers.Add("Content-Encoding", "gzip"); return WithResponse(response, async (c, s) => { var ex = await Throws<MediatrixProtocolException>(() => c.CallAsync(Service, "echo")); Check(ex.OutcomeUnknown); Only(s); }); });
            Add("invalid UTF-8 JSON rejected", () => WithResponse(new Response { Body = new byte[] { (byte)'"', 0xc3, 0x28, (byte)'"' } }, async (c, s) => { var ex = await Throws<MediatrixProtocolException>(() => c.CallAsync(Service, "echo")); Check(ex.OutcomeUnknown); Only(s); }));
            Add("oversized Content-Length JSON rejected", () => WithResponse(Response.Json(Rpc("\"" + new string('a', 1000) + "\"")), async (c, s) => { var ex = await Throws<MediatrixProtocolException>(() => c.CallAsync(Service, "echo")); Check(ex.OutcomeUnknown); Only(s); }, maxJson: 256));
            Add("oversized chunked JSON rejected", () => { Response response = Response.Json(Rpc("\"" + new string('a', 1000) + "\"")); response.Chunked = true; return WithResponse(response, async (c, s) => { var ex = await Throws<MediatrixProtocolException>(() => c.CallAsync(Service, "echo")); Check(ex.OutcomeUnknown); Only(s); }, maxJson: 256); });
        }
        private static void AddStrictStatusSchemas()
        {
            foreach (string json in new[] { "{}", "[]", "null", "{\"status\":123}", "{\"status\":null}", "{\"Status\":\"ok\"}", "{\"status\":\"bad\"}" })
                Add("strict health schema " + json, () => WithResponse(Response.Json(json), async (c, s) => { var ex = await Throws<MediatrixProtocolException>(() => c.HealthAsync()); Check(!ex.OutcomeUnknown); Only(s); }));
            const string node = "{\"version\":\"v\",\"peer_id\":\"peer\",\"addrs\":[],\"connected_peers\":0,\"services\":0,\"files\":0,\"network\":\"n\"}";
            foreach (string json in new[] { "{}", "null", node.Replace("\"peer_id\":\"peer\"", "\"Peer_Id\":\"peer\""), node.Replace("\"connected_peers\":0", "\"connected_peers\":\"0\""), node.Replace("\"files\":0", "\"files\":-1"), node.Replace("\"files\":0", "\"files\":1.5"), node.Replace("\"addrs\":[]", "\"addrs\":[42]") })
                Add("strict node schema " + json, () => WithResponse(Response.Json(json), async (c, s) => { var ex = await Throws<MediatrixProtocolException>(() => c.GetNodeAsync()); Check(!ex.OutcomeUnknown); Only(s); }));
            foreach (string json in new[] { "{}", "null", "[null]", "[{}]", "[{\"name\":\"service:x\",\"url\":\"http://127.0.0.1:9000/rpc\",\"allowed_peers\":[1]}]", "[{\"name\":123,\"url\":\"http://127.0.0.1:9000/rpc\",\"allowed_peers\":[]}]" })
                Add("strict services schema " + json, () => WithResponse(Response.Json(json), async (c, s) => { var ex = await Throws<MediatrixProtocolException>(() => c.GetServicesAsync()); Check(!ex.OutcomeUnknown); Only(s); }));
            Add("API differently cased shadow fields cannot override lowercase error", () => WithResponse(Response.Json("{\"error\":{\"code\":\"expected\",\"Code\":\"shadow\",\"message\":\"actual\",\"Message\":\"shadow\",\"outcome_unknown\":false,\"Outcome_Unknown\":true}}", 400), async (c, s) => { var ex = await Throws<MediatrixApiException>(() => c.CallAsync(Service, "echo")); Equal("expected", ex.Code); Equal("expected: actual", ex.Message); Check(!ex.OutcomeUnknown); Only(s); }));
        }

        private static void AddCancellationAndTransport()
        {
            Add("pre-canceled RPC has known outcome and sends nothing", () => WithResponse(Response.Json(Rpc("1")), async (c, s) => {
                using (var cancel = new CancellationTokenSource()) { cancel.Cancel(); var ex = await Throws<MediatrixCanceledException>(() => c.CallAsync(Service, "echo", cancellationToken: cancel.Token)); Check(!ex.OutcomeUnknown && !ex.IsTimeout); Equal(cancel.Token, ex.CancellationToken); Equal(0, s.Count); } }));
            Add("RPC truncated response unknown and no retry", () => WithResponse(new Response { Body = Encoding.UTF8.GetBytes(Rpc("1")), TruncateAfter = 10 }, async (c, s) => { var ex = await Throws<MediatrixTransportException>(() => c.CallAsync(Service, "echo")); Check(ex.OutcomeUnknown); Only(s); }));
            Add("RPC disconnected response unknown and no retry", () => WithResponse(new Response { Disconnect = true }, async (c, s) => { var ex = await Throws<MediatrixTransportException>(() => c.CallAsync(Service, "echo")); Check(ex.OutcomeUnknown); Only(s); }));
            Add("read-only truncated response known outcome", () => WithResponse(new Response { Body = Encoding.UTF8.GetBytes("{\"status\":\"ok\"}"), TruncateAfter = 3 }, async (c, s) => { var ex = await Throws<MediatrixTransportException>(() => c.HealthAsync()); Check(!ex.OutcomeUnknown); Only(s); }));
            Add("user cancellation interrupts stalled JSON body", () => WithResponse(new Response { Body = Encoding.UTF8.GetBytes(Rpc("1")), StallMilliseconds = 6000, BytesBeforeStall = 4 }, async (c, s) => {
                using (var cancel = new CancellationTokenSource()) { Task<RpcResponse> call = c.CallAsync(Service, "echo", cancellationToken: cancel.Token); await s.Arrived.Task; await Task.Delay(50); var time = Stopwatch.StartNew(); cancel.Cancel(); var ex = await Throws<MediatrixCanceledException>(() => call); Check(ex.OutcomeUnknown && !ex.IsTimeout); Check(time.ElapsedMilliseconds < 1500, "Cancellation did not interrupt response body read."); Only(s); } }));
            Add("deadline interrupts stalled JSON body", () => WithResponse(new Response { Body = Encoding.UTF8.GetBytes(Rpc("1")), StallMilliseconds = 6000, BytesBeforeStall = 4 }, async (c, s) => {
                var time = Stopwatch.StartNew(); var ex = await Throws<MediatrixCanceledException>(() => c.CallAsync(Service, "echo")); Check(ex.OutcomeUnknown && ex.IsTimeout); Check(time.ElapsedMilliseconds < 1500); Only(s); }, timeoutMs: 150));
            Add("user cancellation interrupts stalled binary body", () => WithResponse(new Response { ContentType = "application/octet-stream", Body = Payload, StallMilliseconds = 6000, BytesBeforeStall = 7 }, async (c, s) => {
                using (var destination = new MemoryStream()) using (var cancel = new CancellationTokenSource()) { Task<long> call = c.DownloadToAsync(Key, destination, cancel.Token); await s.Arrived.Task; await Task.Delay(50); cancel.Cancel(); var ex = await Throws<MediatrixCanceledException>(() => call); Check(!ex.OutcomeUnknown && !ex.IsTimeout); Check(destination.CanWrite); Only(s); } }));
            Add("deadline interrupts stalled binary body", () => WithResponse(new Response { ContentType = "application/octet-stream", Body = Payload, StallMilliseconds = 6000, BytesBeforeStall = 7 }, async (c, s) => {
                using (var destination = new MemoryStream()) { var ex = await Throws<MediatrixCanceledException>(() => c.DownloadToAsync(Key, destination)); Check(!ex.OutcomeUnknown && ex.IsTimeout); Check(destination.CanWrite); Only(s); } }, timeoutMs: 150));
            Add("redirect does not forward token or replay mutation", async () => {
                using (var target = new LoopbackServer(r => Response.Json(Rpc("1")))) {
                    Response response = Response.Json("{\"error\":{\"code\":\"redirect\",\"message\":\"do not follow\"}}", 307); response.Headers.Add("Location", target.Url + "/v1/call");
                    await WithResponse(response, async (c, s) => { var ex = await Throws<MediatrixApiException>(() => c.CallAsync(Service, "echo")); Equal((HttpStatusCode)307, ex.StatusCode); Only(s); Equal(0, target.Count); }); } });
            Add("client snapshots mutable options at construction", async () => {
                using (var server = new LoopbackServer(r => Response.Json("{\"status\":\"ok\"}"))) {
                    var options = new MediatrixClientOptions { ApiUrl = server.Url, RequestTimeout = TimeSpan.FromSeconds(2), MaxJsonBytes = 256, MaxFileBytes = 1024 };
                    using (var client = new MediatrixClient(Token, options)) { options.ApiUrl = "http://192.0.2.1:80"; options.RequestTimeout = TimeSpan.FromMilliseconds(1); options.MaxJsonBytes = 1; options.MaxFileBytes = 1; Check(await client.HealthAsync()); Only(server); }
                }
            });
            Add("proxy environment ignored", async () => {
                string[] names = { "HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy" };
                string[] old = names.Select(Environment.GetEnvironmentVariable).ToArray();
                using (var proxy = new LoopbackServer(r => Response.Json("{\"error\":{\"code\":\"proxy\",\"message\":\"unexpected\"}}", 502))) {
                    try { foreach (string name in names) Environment.SetEnvironmentVariable(name, name.ToUpperInvariant() == "NO_PROXY" ? "" : proxy.Url);
                        await WithResponse(Response.Json("{\"status\":\"ok\"}"), async (c, s) => { Check(await c.HealthAsync()); Only(s); Equal(0, proxy.Count); }); }
                    finally { for (int i = 0; i < names.Length; i++) Environment.SetEnvironmentVariable(names[i], old[i]); } } });
            Add("disposed client fails without network", () => WithResponse(Response.Json("{}"), async (c, s) => { c.Dispose(); c.Dispose(); await Throws<ObjectDisposedException>(() => c.HealthAsync()); Equal(0, s.Count); }));
        }

        private static void AddFiles()
        {
            Add("binary upload exact bytes, SHA, size, auth and leaves stream open", () => WithServer(r => Response.Json(Receipt(FileKey(r.Body), r.Body.Length), 201), async (c, s) => {
                using (var source = new TrackingStream(Payload, false)) { Mediatrix.FileInfo f = await c.UploadAsync(source); Equal(Key, f.Key); Equal((long)Payload.Length, f.Size); Equal(0, f.AllowedPeers.Length); Check(!source.WasDisposed && source.CanRead); Request r = Only(s); Equal("POST", r.Method); Equal("/v1/files", r.Target); Equal("application/octet-stream", r.Headers["Content-Type"]); Equal("Bearer " + Token, r.Headers["Authorization"]); Bytes(Payload, r.Body); Check(source.ReadCalls > 1, "Upload did not stream multiple chunks."); } }));
            Add("nonseekable chunked upload leaves stream open", () => WithServer(r => Response.Json(Receipt(FileKey(r.Body), r.Body.Length), 201), async (c, s) => {
                using (var source = new TrackingStream(Payload, true)) { Mediatrix.FileInfo f = await c.UploadAsync(source); Equal(Key, f.Key); Check(!source.WasDisposed); Request r = Only(s); Equal("chunked", r.Headers["Transfer-Encoding"]); Bytes(Payload, r.Body); } }));
            Add("upload respects source current position", () => WithServer(r => Response.Json(Receipt(FileKey(r.Body), r.Body.Length), 201), async (c, s) => {
                using (var source = new MemoryStream(Payload)) { source.Position = 8193; Mediatrix.FileInfo f = await c.UploadAsync(source); byte[] suffix = Payload.Skip(8193).ToArray(); Equal(FileKey(suffix), f.Key); Equal((long)suffix.Length, f.Size); Bytes(suffix, Only(s).Body); Check(source.CanRead); } }));
            Add("empty upload verifies SHA and size", () => WithServer(r => Response.Json(Receipt(FileKey(r.Body), r.Body.Length), 201), async (c, s) => {
                using (var source = new MemoryStream()) { Mediatrix.FileInfo f = await c.UploadAsync(source); Equal(FileKey(new byte[0]), f.Key); Equal(0L, f.Size); Equal(0, Only(s).Body.Length); } }));
            Add("upload wrong digest outcome unknown", () => WithResponse(Response.Json(Receipt(FileKey(new byte[0]), Payload.Length), 201), async (c, s) => {
                using (var source = new MemoryStream(Payload)) { var ex = await Throws<MediatrixProtocolException>(() => c.UploadAsync(source)); Check(ex.OutcomeUnknown); Check(source.CanRead); Only(s); } }));
            Add("upload wrong receipt size outcome unknown", () => WithResponse(Response.Json(Receipt(Key, Payload.Length + 1), 201), async (c, s) => {
                using (var source = new MemoryStream(Payload)) { var ex = await Throws<MediatrixProtocolException>(() => c.UploadAsync(source)); Check(ex.OutcomeUnknown); Check(source.CanRead); Only(s); } }));
            Add("seekable upload cap enforced before send", () => WithResponse(Response.Json("{}", 201), async (c, s) => {
                using (var source = new MemoryStream(Payload)) { await Throws<ArgumentException>(() => c.UploadAsync(source)); Equal(0, s.Count); Check(source.CanRead); } }, maxFile: 1024));
            Add("nonseekable upload cap aborts without retry", () => WithResponse(Response.Json("{}", 201), async (c, s) => {
                using (var source = new TrackingStream(Payload, true)) { var ex = await Throws<MediatrixTransportException>(() => c.UploadAsync(source)); Check(ex.OutcomeUnknown); Check(!source.WasDisposed); Check(s.Count <= 1); } }, maxFile: 1024));
            Add("pre-canceled upload leaves source unread", () => WithResponse(Response.Json("{}", 201), async (c, s) => {
                using (var source = new TrackingStream(Payload, false)) using (var cancel = new CancellationTokenSource()) { cancel.Cancel(); var ex = await Throws<MediatrixCanceledException>(() => c.UploadAsync(source, cancel.Token)); Check(!ex.OutcomeUnknown); Equal(0, source.ReadCalls); Check(!source.WasDisposed); Equal(0, s.Count); } }));
            Add("upload source cancellation interrupts stalled read", () => WithResponse(Response.Json("{}", 201), async (c, s) => {
                using (var source = new StallingReadStream()) using (var cancel = new CancellationTokenSource()) { Task<Mediatrix.FileInfo> call = c.UploadAsync(source, cancel.Token); await source.ReadStarted.Task; cancel.Cancel(); var ex = await Throws<MediatrixCanceledException>(() => call); Check(ex.OutcomeUnknown && !ex.IsTimeout); Check(!source.WasDisposed); Check(s.Count <= 1); } }));
            Add("upload source deadline interrupts stalled read", () => WithResponse(Response.Json("{}", 201), async (c, s) => {
                using (var source = new StallingReadStream()) { var ex = await Throws<MediatrixCanceledException>(() => c.UploadAsync(source)); Check(ex.OutcomeUnknown && ex.IsTimeout); Check(!source.WasDisposed); Check(s.Count <= 1); } }, timeoutMs: 150));
            Add("binary download exact bytes SHA size and leaves destination open", () => WithResponse(Response.Binary(Payload), async (c, s) => {
                using (var destination = new MemoryStream()) { long n = await c.DownloadToAsync(Key, destination); Equal((long)Payload.Length, n); Bytes(Payload, destination.ToArray()); Check(destination.CanWrite); Request r = Only(s); Equal("GET", r.Method); Equal("/v1/files/" + Key.Substring(12), r.Target); } }));
            Add("chunked binary download verifies SHA and size", () => { Response response = Response.Binary(Payload); response.Chunked = true; return WithResponse(response, async (c, s) => {
                using (var destination = new MemoryStream()) { Equal((long)Payload.Length, await c.DownloadToAsync(Key, destination)); Bytes(Payload, destination.ToArray()); Only(s); } }); });
            Add("empty download verifies empty SHA", () => WithResponse(Response.Binary(new byte[0]), async (c, s) => {
                using (var destination = new MemoryStream()) { Equal(0L, await c.DownloadToAsync(FileKey(new byte[0]), destination)); Equal(0L, destination.Length); Only(s); } }));
            Add("download hash mismatch is integrity error", () => WithResponse(Response.Binary(Payload), async (c, s) => {
                using (var destination = new MemoryStream()) { var ex = await Throws<MediatrixIntegrityException>(() => c.DownloadToAsync(FileKey(new byte[0]), destination)); Check(!ex.OutcomeUnknown); Check(destination.CanWrite); Only(s); } }));
            Add("download content-length limit before copying", () => WithResponse(Response.Binary(Payload), async (c, s) => {
                using (var destination = new MemoryStream()) { await Throws<MediatrixIntegrityException>(() => c.DownloadToAsync(Key, destination)); Equal(0L, destination.Length); Check(destination.CanWrite); Only(s); } }, maxFile: 1024));
            Add("download chunked limit while copying", () => { Response response = Response.Binary(Payload); response.Chunked = true; return WithResponse(response, async (c, s) => {
                using (var destination = new MemoryStream()) { await Throws<MediatrixIntegrityException>(() => c.DownloadToAsync(Key, destination)); Check(destination.Length <= 10000); Check(destination.CanWrite); Only(s); } }, maxFile: 10000); });
            Add("download truncated content-length fails", () => { Response response = Response.Binary(Payload); response.TruncateAfter = 123; return WithResponse(response, async (c, s) => {
                using (var destination = new MemoryStream()) { var ex = await Throws<MediatrixTransportException>(() => c.DownloadToAsync(Key, destination)); Check(!ex.OutcomeUnknown); Check(destination.CanWrite); Only(s); } }); });
            Add("download wrong content type rejected", () => WithResponse(Response.Json("{}"), async (c, s) => {
                using (var destination = new MemoryStream()) { var ex = await Throws<MediatrixProtocolException>(() => c.DownloadToAsync(Key, destination)); Check(!ex.OutcomeUnknown); Equal(0L, destination.Length); Only(s); } }));
            Add("download compressed content rejected", () => { Response response = Response.Binary(Payload); response.Headers.Add("Content-Encoding", "gzip"); return WithResponse(response, async (c, s) => {
                using (var destination = new MemoryStream()) { await Throws<MediatrixProtocolException>(() => c.DownloadToAsync(Key, destination)); Equal(0L, destination.Length); Only(s); } }); });
            Add("fetch key size and snake_case request", () => WithResponse(Response.Json(Receipt(Key, Payload.Length)), async (c, s) => {
                Mediatrix.FileInfo f = await c.FetchAsync(Key, 2345); Equal(Key, f.Key); Equal((long)Payload.Length, f.Size); Request r = Only(s); Equal("/v1/fetch", r.Target); JObject body = JObject.Parse(r.Text); Equal(Key, (string)body["key"]); Equal(2345, (int)body["timeout_ms"]); }));
            Add("fetch mismatched receipt key rejected", () => WithResponse(Response.Json(Receipt(FileKey(new byte[0]), 0)), async (c, s) => { var ex = await Throws<MediatrixProtocolException>(() => c.FetchAsync(Key)); Check(ex.OutcomeUnknown); Only(s); }));
            foreach (string bad in new[] { "{\"key\":\"" + Key + "\",\"allowed_peers\":[]}", "{\"key\":\"" + Key + "\",\"size\":-1,\"allowed_peers\":[]}", "{\"key\":\"" + Key + "\",\"size\":\"0\",\"allowed_peers\":[]}", "{\"key\":\"" + Key + "\",\"size\":0}", "{\"Key\":\"" + Key + "\",\"Size\":0,\"Allowed_Peers\":[]}" })
                Add("reject malformed fetch receipt " + bad, () => WithResponse(Response.Json(bad), async (c, s) => { var ex = await Throws<MediatrixProtocolException>(() => c.FetchAsync(Key)); Check(ex.OutcomeUnknown); Only(s); }));
            Add("DownloadFile verifies before publishing and cleans temp", () => InDirectory(async dir => await WithResponse(Response.Binary(Payload), async (c, s) => {
                string output = Path.Combine(dir, "result.bin"); Equal((long)Payload.Length, await c.DownloadFileAsync(Key, output)); Bytes(Payload, File.ReadAllBytes(output)); Equal(1, Directory.GetFiles(dir).Length); Only(s); })));
            Add("DownloadFile invalid hash removes partial temp and destination", () => InDirectory(async dir => await WithResponse(Response.Binary(Payload), async (c, s) => {
                string output = Path.Combine(dir, "result.bin"); await Throws<MediatrixIntegrityException>(() => c.DownloadFileAsync(FileKey(new byte[0]), output)); Check(!File.Exists(output)); Equal(0, Directory.GetFileSystemEntries(dir).Length); Only(s); })));
            Add("DownloadFile refuses existing destination without request", () => InDirectory(async dir => await WithResponse(Response.Binary(Payload), async (c, s) => {
                string output = Path.Combine(dir, "result.bin"); File.WriteAllText(output, "keep existing"); await Throws<IOException>(() => c.DownloadFileAsync(Key, output)); Equal("keep existing", File.ReadAllText(output)); Equal(1, Directory.GetFiles(dir).Length); Equal(0, s.Count); })));
            Add("DownloadFile race does not overwrite newly created target", () => InDirectory(async dir => {
                string output = Path.Combine(dir, "result.bin"); await WithServer(r => { File.WriteAllText(output, "racing writer"); return Response.Binary(Payload); }, async (c, s) => {
                    await Throws<IOException>(() => c.DownloadFileAsync(Key, output)); Equal("racing writer", File.ReadAllText(output)); Equal(1, Directory.GetFiles(dir).Length); Only(s); }); }));
            Add("DownloadFile cancellation cleans partial temp", () => InDirectory(async dir => await WithResponse(new Response { ContentType = "application/octet-stream", Body = Payload, BytesBeforeStall = 100, StallMilliseconds = 6000 }, async (c, s) => {
                string output = Path.Combine(dir, "result.bin"); using (var cancel = new CancellationTokenSource()) { Task<long> call = c.DownloadFileAsync(Key, output, cancel.Token); await s.Arrived.Task; await Task.Delay(50); cancel.Cancel(); await Throws<MediatrixCanceledException>(() => call); Check(!File.Exists(output)); Equal(0, Directory.GetFileSystemEntries(dir).Length); Only(s); } })));
            Add("UploadFile reads from disk and releases handle", () => InDirectory(async dir => await WithServer(r => Response.Json(Receipt(FileKey(r.Body), r.Body.Length), 201), async (c, s) => {
                string input = Path.Combine(dir, "input.bin"); File.WriteAllBytes(input, Payload); Mediatrix.FileInfo f = await c.UploadFileAsync(input); Equal(Key, f.Key); Bytes(Payload, Only(s).Body); using (File.Open(input, FileMode.Open, FileAccess.ReadWrite, FileShare.None)) { } })));
        }

        private static void AddConcurrency()
        {
            Add("24 simultaneous RPC calls keep IDs isolated", () => WithServer(r => {
                JObject input = JObject.Parse(r.Text); string id = (string)input["request_id"]; return Response.Json(Rpc(input["params"].ToString(Formatting.None), id));
            }, async (c, s) => {
                Task[] calls = Enumerable.Range(0, 24).Select(async i => { string id = "parallel-" + i; RpcResponse result = await c.CallAsync(Service, "echo", i.ToString(), id); Equal(id, result.RequestId); Equal(i, result.DeserializeResult<int>()); }).ToArray();
                await Task.WhenAll(calls); Equal(24, s.Count); Equal(24, s.Requests.Select(r => (string)JObject.Parse(r.Text)["request_id"]).Distinct().Count()); Check(s.Requests.All(r => r.Headers["Authorization"] == "Bearer " + Token)); }));
            Add("8 concurrent file downloads are independently verified", () => WithResponse(Response.Binary(Payload), async (c, s) => {
                Task[] downloads = Enumerable.Range(0, 8).Select(async i => { using (var output = new MemoryStream()) { Equal((long)Payload.Length, await c.DownloadToAsync(Key, output)); Bytes(Payload, output.ToArray()); } }).ToArray();
                await Task.WhenAll(downloads); Equal(8, s.Count); }));
        }
        private static async Task InDirectory(Func<string, Task> action)
        {
            string path = Path.Combine(Path.GetTempPath(), "mediatrix-client-tests-" + Guid.NewGuid().ToString("N")); Directory.CreateDirectory(path);
            try { await action(path); } finally { Directory.Delete(path, true); }
        }
        private static async Task RealDaemon(string url)
        {
            string token = Environment.GetEnvironmentVariable("MEDIATRIX_API_TOKEN");
            if (string.IsNullOrEmpty(token)) throw new InvalidOperationException("MEDIATRIX_API_TOKEN is required for --daemon (never pass tokens in arguments).");
            string name = "service:csharp-contract-" + Guid.NewGuid().ToString("N");
            using (var handler = new LoopbackServer(r => {
                JObject body = JObject.Parse(r.Text); string method = (string)body["method"];
                if (method == "fail") return Response.Json("{\"error\":{\"code\":\"expected\",\"message\":\"contract test\"}}");
                if (method == "echo") return Response.Json("{\"result\":" + body["params"].ToString(Formatting.None) + "}");
                return Response.Json("{\"error\":{\"code\":\"not_found\",\"message\":\"unknown method\"}}");
            }))
            using (var c = new MediatrixClient(token, new MediatrixClientOptions { ApiUrl = url }))
            {
                Check(await c.HealthAsync()); NodeInfo node = await c.GetNodeAsync(); Check(!string.IsNullOrEmpty(node.PeerId));
                bool registered = false;
                try
                {
                    await c.RegisterServiceAsync(name, handler.Url + "/rpc"); registered = true;
                    Check((await c.GetServicesAsync()).Any(s => s.Name == name));
                    RpcResponse echo = await c.CallAsync(name, "echo", "{\"message\":\"C# net462 DLL\",\"n\":42}", "daemon-csharp-echo"); Check(echo.IsSuccess); Equal(42, (int)JObject.Parse(echo.ResultJson)["n"]);
                    RpcResponse nil = await c.CallAsync(name, "echo", "null", "daemon-csharp-null"); Check(nil.IsSuccess); Equal("null", nil.ResultJson);
                    RpcResponse error = await c.CallAsync(name, "fail", requestId: "daemon-csharp-error"); Check(!error.IsSuccess); Equal("expected", error.Error.Code);
                    using (var source = new MemoryStream(Payload)) { Mediatrix.FileInfo file = await c.UploadAsync(source); Equal(Key, file.Key); Equal((long)Payload.Length, file.Size); Check(source.CanRead); }
                    await c.SetFileAccessAsync(Key); Mediatrix.FileInfo fetched = await c.FetchAsync(Key); Equal(Key, fetched.Key); Equal((long)Payload.Length, fetched.Size);
                    using (var output = new MemoryStream()) { Equal((long)Payload.Length, await c.DownloadToAsync(Key, output)); Bytes(Payload, output.ToArray()); }
                    await InDirectory(async dir => { string output = Path.Combine(dir, "download.bin"); await c.DownloadFileAsync(Key, output); Bytes(Payload, File.ReadAllBytes(output)); await Throws<IOException>(() => c.DownloadFileAsync(Key, output)); });
                    await c.RemoveServiceAsync(name); registered = false; Check(!(await c.GetServicesAsync()).Any(s => s.Name == name));
                    var missing = await Throws<MediatrixApiException>(() => c.CallAsync(name, "echo")); Equal(HttpStatusCode.NotFound, missing.StatusCode);
                    Console.WriteLine("  Real daemon verified peer " + node.PeerId + "; local registration removed; uploaded test content remains in daemon store.");
                }
                finally { if (registered) await c.RemoveServiceAsync(name); }
            }
        }
    }

    internal sealed class TrackingStream : Stream
    {
        private readonly MemoryStream inner;
        private readonly bool noSeek;
        internal bool WasDisposed;
        internal int ReadCalls;
        internal TrackingStream(byte[] bytes, bool noSeek) { inner = new MemoryStream(bytes); this.noSeek = noSeek; }
        public override bool CanRead { get { return !WasDisposed; } }
        public override bool CanSeek { get { return !noSeek; } }
        public override bool CanWrite { get { return false; } }
        public override long Length { get { if (noSeek) throw new NotSupportedException(); return inner.Length; } }
        public override long Position { get { if (noSeek) throw new NotSupportedException(); return inner.Position; } set { if (noSeek) throw new NotSupportedException(); inner.Position = value; } }
        public override int Read(byte[] buffer, int offset, int count) { ReadCalls++; return inner.Read(buffer, offset, count); }
        public override Task<int> ReadAsync(byte[] buffer, int offset, int count, CancellationToken token) { ReadCalls++; return inner.ReadAsync(buffer, offset, count, token); }
        public override long Seek(long offset, SeekOrigin origin) { if (noSeek) throw new NotSupportedException(); return inner.Seek(offset, origin); }
        public override void Flush() { }
        public override void SetLength(long value) { throw new NotSupportedException(); }
        public override void Write(byte[] buffer, int offset, int count) { throw new NotSupportedException(); }
        protected override void Dispose(bool disposing) { WasDisposed = true; if (disposing) inner.Dispose(); base.Dispose(disposing); }
    }
    internal sealed class StallingReadStream : Stream
    {
        internal readonly TaskCompletionSource<bool> ReadStarted = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        internal bool WasDisposed;
        public override bool CanRead { get { return !WasDisposed; } }
        public override bool CanSeek { get { return false; } }
        public override bool CanWrite { get { return false; } }
        public override long Length { get { throw new NotSupportedException(); } }
        public override long Position { get { throw new NotSupportedException(); } set { throw new NotSupportedException(); } }
        public override async Task<int> ReadAsync(byte[] buffer, int offset, int count, CancellationToken token) { ReadStarted.TrySetResult(true); await Task.Delay(Timeout.Infinite, token); return 0; }
        public override int Read(byte[] buffer, int offset, int count) { throw new NotSupportedException(); }
        public override long Seek(long offset, SeekOrigin origin) { throw new NotSupportedException(); }
        public override void Flush() { }
        public override void SetLength(long value) { throw new NotSupportedException(); }
        public override void Write(byte[] buffer, int offset, int count) { throw new NotSupportedException(); }
        protected override void Dispose(bool disposing) { WasDisposed = true; base.Dispose(disposing); }
    }
}
