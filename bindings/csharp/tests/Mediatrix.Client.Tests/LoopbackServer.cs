using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.Globalization;
using System.IO;
using System.Linq;
using System.Net;
using System.Net.Sockets;
using System.Text;
using System.Threading;
using System.Threading.Tasks;

namespace Mediatrix.Client.Tests
{
    internal sealed class Request
    {
        internal string Method;
        internal string Target;
        internal Dictionary<string, string> Headers;
        internal byte[] Body;
        internal string Text { get { return Encoding.UTF8.GetString(Body); } }
    }

    internal sealed class Response
    {
        internal int Status = 200;
        internal string ContentType = "application/json";
        internal byte[] Body = Encoding.UTF8.GetBytes("{}");
        internal long? DeclaredLength = null;
        internal int? TruncateAfter;
        internal int StallMilliseconds;
        internal int BytesBeforeStall;
        internal bool Disconnect;
        internal bool Chunked;
        internal Dictionary<string, string> Headers = new Dictionary<string, string>();
        internal static Response Json(string json, int status = 200)
        { return new Response { Status = status, Body = Encoding.UTF8.GetBytes(json) }; }
        internal static Response Binary(byte[] body)
        { return new Response { Body = body, ContentType = "application/octet-stream" }; }
        internal static Response NoContent() { return new Response { Status = 204, Body = new byte[0], ContentType = null }; }
    }

    // No HTTP test-server package: malformed framing and incomplete bodies are intentional test cases.
    internal sealed class LoopbackServer : IDisposable
    {
        private readonly TcpListener listener;
        private readonly Func<Request, Response> handler;
        private readonly CancellationTokenSource stop = new CancellationTokenSource();
        private readonly Task accept;
        private int count;
        internal readonly ConcurrentQueue<Request> Requests = new ConcurrentQueue<Request>();
        internal readonly ConcurrentQueue<Exception> Errors = new ConcurrentQueue<Exception>();
        internal readonly TaskCompletionSource<Request> Arrived = new TaskCompletionSource<Request>(TaskCreationOptions.RunContinuationsAsynchronously);
        internal string Url { get; private set; }
        internal int Count { get { return Volatile.Read(ref count); } }

        internal LoopbackServer(Func<Request, Response> handler)
        {
            this.handler = handler;
            listener = new TcpListener(IPAddress.Loopback, 0);
            listener.Start();
            Url = "http://127.0.0.1:" + ((IPEndPoint)listener.LocalEndpoint).Port.ToString(CultureInfo.InvariantCulture);
            accept = Task.Run(AcceptAsync);
        }
        private async Task AcceptAsync()
        {
            while (!stop.IsCancellationRequested)
            {
                try
                {
                    TcpClient connection = await listener.AcceptTcpClientAsync().ConfigureAwait(false);
                    // Each connection is owned and closed by ServeAsync; observe every exception there.
                    _ = Task.Run(() => ServeAsync(connection));
                }
                catch (ObjectDisposedException) { break; }
                catch (SocketException) { if (stop.IsCancellationRequested) break; throw; }
            }
        }
        private async Task ServeAsync(TcpClient connection)
        {
            try
            {
                using (connection)
                using (NetworkStream stream = connection.GetStream())
                {
                    string first = await ReadLine(stream, stop.Token).ConfigureAwait(false);
                    if (first == null) return;
                    string[] words = first.Split(' ');
                    var request = new Request { Method = words[0], Target = words[1], Headers = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase) };
                    string line;
                    while (!string.IsNullOrEmpty(line = await ReadLine(stream, stop.Token).ConfigureAwait(false)))
                    {
                        int colon = line.IndexOf(':');
                        if (colon < 0) throw new IOException("Malformed request header.");
                        request.Headers.Add(line.Substring(0, colon), line.Substring(colon + 1).Trim());
                    }
                    Interlocked.Increment(ref count);
                    using (var body = new MemoryStream())
                    {
                        string value;
                        if (request.Headers.TryGetValue("Transfer-Encoding", out value) && value.IndexOf("chunked", StringComparison.OrdinalIgnoreCase) >= 0)
                        {
                            while (true)
                            {
                                string sizeLine = await ReadLine(stream, stop.Token).ConfigureAwait(false);
                                int size = int.Parse(sizeLine.Split(';')[0], NumberStyles.HexNumber, CultureInfo.InvariantCulture);
                                if (size == 0)
                                {
                                    while (!string.IsNullOrEmpty(await ReadLine(stream, stop.Token).ConfigureAwait(false))) { }
                                    break;
                                }
                                await ReadExactly(stream, body, size, stop.Token).ConfigureAwait(false);
                                if (await ReadLine(stream, stop.Token).ConfigureAwait(false) != "") throw new IOException("Missing chunk terminator.");
                            }
                        }
                        else if (request.Headers.TryGetValue("Content-Length", out value))
                            await ReadExactly(stream, body, long.Parse(value, CultureInfo.InvariantCulture), stop.Token).ConfigureAwait(false);
                        request.Body = body.ToArray();
                    }
                    Requests.Enqueue(request);
                    Arrived.TrySetResult(request);
                    Response response = handler(request);
                    if (response.Disconnect) return;
                    var headers = new StringBuilder("HTTP/1.1 " + response.Status + " Test\r\nConnection: close\r\n");
                    if (response.ContentType != null) headers.Append("Content-Type: " + response.ContentType + "\r\n");
                    if (response.Chunked) headers.Append("Transfer-Encoding: chunked\r\n");
                    else headers.Append("Content-Length: " + (response.DeclaredLength ?? response.Body.Length).ToString(CultureInfo.InvariantCulture) + "\r\n");
                    foreach (var header in response.Headers) headers.Append(header.Key + ": " + header.Value + "\r\n");
                    headers.Append("\r\n");
                    byte[] head = Encoding.ASCII.GetBytes(headers.ToString());
                    await stream.WriteAsync(head, 0, head.Length, stop.Token).ConfigureAwait(false);
                    if (response.Chunked)
                    {
                        for (int offset = 0; offset < response.Body.Length;)
                        {
                            int length = Math.Min(4093, response.Body.Length - offset);
                            byte[] chunkHead = Encoding.ASCII.GetBytes(length.ToString("x", CultureInfo.InvariantCulture) + "\r\n");
                            await stream.WriteAsync(chunkHead, 0, chunkHead.Length, stop.Token).ConfigureAwait(false);
                            await stream.WriteAsync(response.Body, offset, length, stop.Token).ConfigureAwait(false);
                            await stream.WriteAsync(new byte[] { 13, 10 }, 0, 2, stop.Token).ConfigureAwait(false);
                            offset += length;
                        }
                        byte[] end = Encoding.ASCII.GetBytes("0\r\n\r\n");
                        await stream.WriteAsync(end, 0, end.Length, stop.Token).ConfigureAwait(false);
                        return;
                    }
                    int sendLength = response.TruncateAfter ?? response.Body.Length;
                    int prefix = Math.Min(response.BytesBeforeStall, sendLength);
                    if (prefix != 0) await stream.WriteAsync(response.Body, 0, prefix, stop.Token).ConfigureAwait(false);
                    if (response.StallMilliseconds > 0) await Task.Delay(response.StallMilliseconds, stop.Token).ConfigureAwait(false);
                    if (sendLength > prefix) await stream.WriteAsync(response.Body, prefix, sendLength - prefix, stop.Token).ConfigureAwait(false);
                }
            }
            catch (Exception ex) when (ex is IOException || ex is SocketException || ex is OperationCanceledException || ex is ObjectDisposedException) { }
            catch (Exception ex) { Errors.Enqueue(ex); Arrived.TrySetException(ex); }
        }
        private static async Task<string> ReadLine(Stream stream, CancellationToken token)
        {
            var bytes = new List<byte>(); var one = new byte[1];
            while (await stream.ReadAsync(one, 0, 1, token).ConfigureAwait(false) != 0)
            {
                if (one[0] == 10)
                {
                    if (bytes.Count == 0 || bytes[bytes.Count - 1] != 13) throw new IOException("Invalid HTTP line.");
                    return Encoding.ASCII.GetString(bytes.Take(bytes.Count - 1).ToArray());
                }
                bytes.Add(one[0]); if (bytes.Count > 65536) throw new IOException("Header limit.");
            }
            if (bytes.Count != 0) throw new IOException("Unexpected EOF.");
            return null;
        }
        private static async Task ReadExactly(Stream source, Stream destination, long count, CancellationToken token)
        {
            byte[] bytes = new byte[81920];
            while (count > 0)
            {
                int read = await source.ReadAsync(bytes, 0, (int)Math.Min(count, bytes.Length), token).ConfigureAwait(false);
                if (read == 0) throw new IOException("Unexpected request EOF.");
                destination.Write(bytes, 0, read); count -= read;
            }
        }
        public void Dispose()
        {
            stop.Cancel(); listener.Stop();
            try { accept.GetAwaiter().GetResult(); } catch (ObjectDisposedException) { }
            stop.Dispose();
        }
    }
}
