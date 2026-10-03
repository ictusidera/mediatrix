using System;
using System.Globalization;
using System.IO;
using System.Net;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using Mediatrix;
using Newtonsoft.Json;
using Newtonsoft.Json.Linq;

internal static class Program
{
    // Blocking only at a console entry point. WinForms/WPF callers should await directly.
    private static int Main(string[] args)
    {
        using (var stop = new CancellationTokenSource())
        {
            Console.CancelKeyPress += (sender, e) => { e.Cancel = true; stop.Cancel(); };
            try { return RunAsync(args, stop.Token).GetAwaiter().GetResult(); }
            catch (MediatrixCanceledException e) { Console.Error.WriteLine("Canceled; timeout={0}, outcome_unknown={1}", e.IsTimeout, e.OutcomeUnknown); return 2; }
            catch (MediatrixException e) { Console.Error.WriteLine("{0}; outcome_unknown={1}", e.Message, e.OutcomeUnknown); return 1; }
            catch (OperationCanceledException) { return 2; }
            catch (Exception e) { Console.Error.WriteLine(e.Message); return 1; }
        }
    }
    private static async Task<int> RunAsync(string[] args, CancellationToken ct)
    {
        string command = args.Length == 0 ? "node" : args[0];
        if (command == "handler")
        {
            await HandlerAsync(ct); return 0;
        }
        var options = new MediatrixClientOptions { ApiUrl = Environment.GetEnvironmentVariable("MEDIATRIX_API_URL") ?? "http://127.0.0.1:47832" };
        using (var client = new MediatrixClient(Environment.GetEnvironmentVariable("MEDIATRIX_API_TOKEN"), options))
        {
            if (command == "node")
            {
                var node = await client.GetNodeAsync(ct);
                Console.WriteLine("Peer: {0}\nNetwork: {1}", node.PeerId, node.Network);
            }
            else if (command == "rpc")
            {
                // Start `ConsoleDemo.exe handler` in another terminal first.
                // Empty ACL allows local calls only. Registration survives daemon restart.
                await client.RegisterServiceAsync("service:csharp-demo", "http://127.0.0.1:49080/rpc/", cancellationToken: ct);
                RpcResponse response = await client.CallAsync("service:csharp-demo", "add", "[20,22]", Guid.NewGuid().ToString("N"), 10000, ct);
                response.ThrowIfError();
                Console.WriteLine("Result: " + response.ResultJson);
            }
            else if (command == "files" && args.Length == 3)
            {
                // Import source into THIS daemon. No central server/FTP is involved.
                var imported = await client.UploadFileAsync(args[1], ct);
                Console.WriteLine("Imported: " + imported.Key);
                // Explicit full ACL replacement; empty means remote deny.
                await client.SetFileAccessAsync(imported.Key, new string[0], ct);
                // Local copy exists; FetchAsync can instead obtain a missing copy from an authorized peer.
                await client.FetchAsync(imported.Key, 10000, ct);
                long size = await client.DownloadFileAsync(imported.Key, args[2], ct);
                Console.WriteLine("Verified download: " + size + " bytes");
            }
            else
            {
                Console.Error.WriteLine("Usage: ConsoleDemo.exe [node|handler|rpc|files INPUT NEW_OUTPUT]"); return 1;
            }
        }
        return 0;
    }

    // Small trusted-local-machine example, not a production application server.
    // On Windows a URL reservation may be needed; see README before running as administrator.
    private static async Task HandlerAsync(CancellationToken ct)
    {
        const string prefix = "http://127.0.0.1:49080/rpc/";
        using (var listener = new HttpListener())
        {
            listener.Prefixes.Add(prefix); listener.Start();
            Console.WriteLine("Handler: " + prefix + " (Ctrl+C to stop)");
            using (ct.Register(() => listener.Close()))
            {
                while (!ct.IsCancellationRequested)
                {
                    HttpListenerContext context;
                    try { context = await listener.GetContextAsync(); }
                    catch (HttpListenerException) when (ct.IsCancellationRequested) { break; }
                    catch (ObjectDisposedException) when (ct.IsCancellationRequested) { break; }
                    // Sequential and bounded demonstration; production needs its own concurrency/deadline policy.
                    try
                    {
                        if (context.Request.HttpMethod != "POST" || context.Request.Url.AbsolutePath != "/rpc/") { context.Response.StatusCode = 404; continue; }
                        if (!IPAddress.IsLoopback(context.Request.RemoteEndPoint.Address)) { context.Response.StatusCode = 403; continue; }
                        byte[] buffer = new byte[4096]; int count;
                        using (var body = new MemoryStream())
                        {
                            while ((count = await context.Request.InputStream.ReadAsync(buffer, 0, buffer.Length, ct)) != 0)
                            {
                                if (body.Length + count > 65536) throw new InvalidDataException("Request too large.");
                                body.Write(buffer, 0, count);
                            }
                            JObject request;
                            using (var reader = new JsonTextReader(new StringReader(new UTF8Encoding(false, true).GetString(body.ToArray()))) { DateParseHandling = DateParseHandling.None, MaxDepth = 64 })
                                request = JObject.Load(reader);
                            string method = (string)request["method"];
                            JObject response;
                            if (method == "echo") response = new JObject { ["result"] = request["params"] ?? JValue.CreateNull() };
                            else if (method == "add" && request["params"] is JArray)
                            {
                                decimal sum = 0;
                                foreach (var value in (JArray)request["params"])
                                {
                                    if (value.Type != JTokenType.Integer && value.Type != JTokenType.Float) throw new InvalidDataException("add requires numbers.");
                                    sum += (decimal)value;
                                }
                                response = new JObject { ["result"] = sum };
                            }
                            else response = Error("invalid", "Unknown method or invalid parameters.");
                            await Respond(context.Response, response, ct);
                        }
                    }
                    catch (Exception e) when (e is JsonException || e is InvalidDataException || e is OverflowException || e is FormatException)
                    { await Respond(context.Response, Error("invalid", "Invalid request."), ct); }
                    finally { context.Response.Close(); }
                }
            }
        }
    }
    private static JObject Error(string code, string message) { return new JObject { ["error"] = new JObject { ["code"] = code, ["message"] = message } }; }
    private static async Task Respond(HttpListenerResponse response, JObject value, CancellationToken ct)
    {
        byte[] bytes = Encoding.UTF8.GetBytes(value.ToString(Formatting.None));
        response.StatusCode = 200; response.ContentType = "application/json"; response.ContentLength64 = bytes.Length;
        await response.OutputStream.WriteAsync(bytes, 0, bytes.Length, ct);
    }
}
