using System;
using System.Collections.Generic;
using System.Globalization;
using System.IO;
using System.Net;
using System.Net.Http;
using System.Net.Http.Headers;
using System.Security.Cryptography;
using System.Threading;
using System.Threading.Tasks;
using Newtonsoft.Json;

namespace Mediatrix
{
    /// <summary>Reusable asynchronous client for one already-running local daemon. Owns its HTTP transport.</summary>
    public sealed class MediatrixClient : IDisposable
    {
        private readonly HttpClient http;
        private readonly Uri baseUri;
        private readonly string token;
        private readonly TimeSpan timeout;
        private readonly int maxJson;
        private readonly long maxFile;
        private int disposed;

        public MediatrixClient(string apiToken, MediatrixClientOptions options = null)
        {
            if (apiToken == null || apiToken.Length < 32 || apiToken.Length > 4096) throw new ArgumentException("API token must contain 32..4096 printable ASCII characters without spaces.", nameof(apiToken));
            foreach (char c in apiToken) if (c < 33 || c > 126) throw new ArgumentException("Invalid API token characters.", nameof(apiToken));
            options = options ?? new MediatrixClientOptions();
            baseUri = Validation.LoopbackUrl(options.ApiUrl, false);
            if (options.RequestTimeout < TimeSpan.FromMilliseconds(1) || options.RequestTimeout > TimeSpan.FromSeconds(310)) throw new ArgumentOutOfRangeException(nameof(options.RequestTimeout));
            if (options.MaxJsonBytes < 1 || options.MaxJsonBytes > 16 * 1024 * 1024) throw new ArgumentOutOfRangeException(nameof(options.MaxJsonBytes));
            if (options.MaxFileBytes < 1 || options.MaxFileBytes > 1024L * 1024 * 1024) throw new ArgumentOutOfRangeException(nameof(options.MaxFileBytes));
            token = apiToken; timeout = options.RequestTimeout; maxJson = options.MaxJsonBytes; maxFile = options.MaxFileBytes;
            var handler = new HttpClientHandler { AllowAutoRedirect = false, UseProxy = false, UseCookies = false, UseDefaultCredentials = false, AutomaticDecompression = DecompressionMethods.None };
            http = new HttpClient(handler, true) { Timeout = System.Threading.Timeout.InfiniteTimeSpan };
        }

        public async Task<bool> HealthAsync(CancellationToken cancellationToken = default(CancellationToken))
        {
            var json = await JsonAsync(HttpMethod.Get, "/v1/health", null, HttpStatusCode.OK, false, cancellationToken).ConfigureAwait(false);
            try { if (ReadString(RawJson.Object(json), "status") != "ok") throw new FormatException(); return true; }
            catch (Exception e) when (e is JsonException || e is FormatException) { throw new MediatrixProtocolException("Invalid health response.", false, e); }
        }
        public async Task<NodeInfo> GetNodeAsync(CancellationToken cancellationToken = default(CancellationToken))
        {
            string json = await JsonAsync(HttpMethod.Get, "/v1/node", null, HttpStatusCode.OK, false, cancellationToken).ConfigureAwait(false);
            try
            {
                var f = RawJson.Object(json);
                return new NodeInfo { Version = ReadString(f, "version"), PeerId = ReadString(f, "peer_id"), Network = ReadString(f, "network"), Addresses = ReadStrings(f, "addrs"), ConnectedPeers = ReadCount(f, "connected_peers"), Services = ReadCount(f, "services"), Files = ReadCount(f, "files") };
            }
            catch (Exception e) when (e is JsonException || e is FormatException) { throw new MediatrixProtocolException("Invalid node response.", false, e); }
        }
        public async Task<ServiceInfo[]> GetServicesAsync(CancellationToken cancellationToken = default(CancellationToken))
        {
            string json = await JsonAsync(HttpMethod.Get, "/v1/services", null, HttpStatusCode.OK, false, cancellationToken).ConfigureAwait(false);
            try
            {
                var array = Newtonsoft.Json.Linq.JArray.Parse(json); var result = new ServiceInfo[array.Count];
                for (int i = 0; i < array.Count; i++)
                {
                    var f = RawJson.Object(array[i].ToString(Formatting.None));
                    result[i] = new ServiceInfo { Name = ReadString(f, "name"), Url = ReadString(f, "url"), AllowedPeers = ReadStrings(f, "allowed_peers") };
                }
                return result;
            }
            catch (Exception e) when (e is JsonException || e is FormatException) { throw new MediatrixProtocolException("Invalid services response.", false, e); }
        }
        /// <summary>Replaces a registration and its entire ACL. Null/empty peers means remote deny.</summary>
        public async Task RegisterServiceAsync(string name, string handlerUrl, string[] allowedPeers = null, CancellationToken cancellationToken = default(CancellationToken))
        {
            Validation.Service(name); Validation.LoopbackUrl(handlerUrl, true);
            await JsonAsync(HttpMethod.Put, "/v1/services", new { name, url = handlerUrl, allowed_peers = Validation.Peers(allowedPeers) }, HttpStatusCode.NoContent, true, cancellationToken).ConfigureAwait(false);
        }
        public async Task RemoveServiceAsync(string name, CancellationToken cancellationToken = default(CancellationToken))
        {
            Validation.Service(name);
            await JsonAsync(HttpMethod.Delete, "/v1/services?name=" + Uri.EscapeDataString(name), null, HttpStatusCode.NoContent, true, cancellationToken).ConfigureAwait(false);
        }
        /// <summary>Calls once, preserving arbitrary JSON numbers verbatim. Request ID is correlation, not deduplication.</summary>
        public async Task<RpcResponse> CallAsync(string service, string method, string paramsJson = "null", string requestId = null, long timeoutMilliseconds = 0, CancellationToken cancellationToken = default(CancellationToken))
        {
            Validation.Service(service); Validation.Timeout(timeoutMilliseconds);
            if (string.IsNullOrEmpty(method) || Validation.Utf8.GetByteCount(method) > 128 || method.IndexOfAny(new[] { '\r', '\n', '\0' }) >= 0) throw new ArgumentException("Invalid RPC method.", nameof(method));
            if (requestId != null && (Validation.Utf8.GetByteCount(requestId) > 128 || requestId.IndexOfAny(new[] { '\r', '\n', '\0' }) >= 0)) throw new ArgumentException("Invalid request ID.", nameof(requestId));
            if (paramsJson == null) paramsJson = "null";
            if (Validation.Utf8.GetByteCount(paramsJson) > maxJson) throw new ArgumentException("JSON parameters exceed client limit.", nameof(paramsJson));
            RawJson.Validate(paramsJson);
            var payload = new Dictionary<string, object> { { "service", service }, { "method", method }, { "params", new Newtonsoft.Json.Linq.JRaw(paramsJson) } };
            if (!string.IsNullOrEmpty(requestId)) payload.Add("request_id", requestId);
            if (timeoutMilliseconds != 0) payload.Add("timeout_ms", timeoutMilliseconds);
            string json = await JsonAsync(HttpMethod.Post, "/v1/call", payload, HttpStatusCode.OK, true, cancellationToken).ConfigureAwait(false);
            var fields = ParseObject(json, true);
            try
            {
                string id = ReadString(fields, "request_id");
                bool result = fields.ContainsKey("result"), error = fields.ContainsKey("error");
                if (string.IsNullOrEmpty(id) || (!string.IsNullOrEmpty(requestId) && requestId != id) || result == error) throw new FormatException();
                return new RpcResponse { RequestId = id, ResultJson = result ? fields["result"] : null, Error = error ? ParseError(fields["error"]) : null };
            }
            catch (Exception e) when (e is JsonException || e is FormatException)
            { throw new MediatrixProtocolException("Invalid or mismatched RPC response; execution may have occurred.", true, e); }
        }
        /// <summary>Imports the caller stream into this daemon's store. Does not grant access or upload to a central server. Leaves source open.</summary>
        public Task<FileInfo> UploadAsync(Stream source, CancellationToken cancellationToken = default(CancellationToken))
        {
            if (source == null || !source.CanRead) throw new ArgumentException("A readable stream is required.", nameof(source));
            long? length = source.CanSeek ? (long?)(source.Length - source.Position) : null;
            if (length.HasValue && (length < 0 || length > maxFile)) throw new ArgumentException("Source exceeds client file limit.", nameof(source));
            return ExecuteAsync(HttpMethod.Post, "/v1/files", HttpStatusCode.Created, true,
                ct => new UploadContent(source, length, maxFile, ct), async (response, request, ct) =>
                {
                    FileInfo info = ParseFile(await ReadJsonAsync(response, ct).ConfigureAwait(false), true);
                    var upload = (UploadContent)request.Content;
                    if (upload.Digest == null || info.Key != "file:sha256:" + upload.Digest || info.Size != upload.BytesCopied) throw new MediatrixProtocolException("Upload receipt does not match sent content; import outcome is unknown.", true);
                    return info;
                }, cancellationToken);
        }
        public async Task<FileInfo> UploadFileAsync(string path, CancellationToken cancellationToken = default(CancellationToken))
        {
            using (var stream = new FileStream(path, FileMode.Open, FileAccess.Read, FileShare.Read, 81920, true))
                return await UploadAsync(stream, cancellationToken).ConfigureAwait(false);
        }
        /// <summary>Replaces the full file ACL. Null/empty peers revokes remote access to this copy.</summary>
        public async Task SetFileAccessAsync(string key, string[] allowedPeers = null, CancellationToken cancellationToken = default(CancellationToken))
        {
            Validation.Digest(key);
            await JsonAsync(HttpMethod.Put, "/v1/files/access", new { key, allowed_peers = Validation.Peers(allowedPeers) }, HttpStatusCode.NoContent, true, cancellationToken).ConfigureAwait(false);
        }
        /// <summary>Ensures a verified local copy, obtaining it from an authorized peer if necessary. Does not save an application file.</summary>
        public async Task<FileInfo> FetchAsync(string key, long timeoutMilliseconds = 0, CancellationToken cancellationToken = default(CancellationToken))
        {
            Validation.Digest(key); Validation.Timeout(timeoutMilliseconds);
            FileInfo info = ParseFile(await JsonAsync(HttpMethod.Post, "/v1/fetch", new { key, timeout_ms = timeoutMilliseconds }, HttpStatusCode.OK, true, cancellationToken).ConfigureAwait(false), true);
            if (info.Key != key) throw new MediatrixProtocolException("Fetch receipt has a different key.", true);
            return info;
        }
        /// <summary>Streams a local-store file, verifies its SHA-256, and leaves destination open. On failure discard any partial destination bytes.</summary>
        public Task<long> DownloadToAsync(string key, Stream destination, CancellationToken cancellationToken = default(CancellationToken))
        {
            string digest = Validation.Digest(key);
            if (destination == null || !destination.CanWrite) throw new ArgumentException("A writable stream is required.", nameof(destination));
            return ExecuteAsync(HttpMethod.Get, "/v1/files/" + digest, HttpStatusCode.OK, false, null, async (response, request, ct) =>
            {
                if (response.Content.Headers.ContentType == null || response.Content.Headers.ContentType.MediaType != "application/octet-stream" || response.Content.Headers.ContentEncoding.Count != 0) throw new MediatrixProtocolException("Unexpected file content type/encoding.", false);
                long? expected = response.Content.Headers.ContentLength;
                if (expected > maxFile) throw new MediatrixIntegrityException("Download exceeds client file limit.");
                using (var hash = SHA256.Create())
                using (Stream source = await response.Content.ReadAsStreamAsync().ConfigureAwait(false))
                {
                    byte[] buffer = new byte[81920]; long total = 0; int count;
                    while ((count = await source.ReadAsync(buffer, 0, buffer.Length, ct).ConfigureAwait(false)) != 0)
                    {
                        total += count;
                        if (total > maxFile) throw new MediatrixIntegrityException("Download exceeds client file limit.");
                        hash.TransformBlock(buffer, 0, count, null, 0);
                        await destination.WriteAsync(buffer, 0, count, ct).ConfigureAwait(false);
                    }
                    hash.TransformFinalBlock(new byte[0], 0, 0);
                    if ((expected.HasValue && expected.Value != total) || Hex(hash.Hash) != digest) throw new MediatrixIntegrityException("Downloaded length/SHA-256 does not match. Discard the destination bytes.");
                    return total;
                }
            }, cancellationToken);
        }
        /// <summary>Verifies into a sibling temporary file, then publishes with a no-overwrite move. Existing destination is never replaced.</summary>
        public async Task<long> DownloadFileAsync(string key, string destinationPath, CancellationToken cancellationToken = default(CancellationToken))
        {
            Validation.Digest(key);
            string destination = Path.GetFullPath(destinationPath);
            if (File.Exists(destination) || Directory.Exists(destination)) throw new IOException("Destination already exists.");
            string temporary = Path.Combine(Path.GetDirectoryName(destination), ".mediatrix-" + Guid.NewGuid().ToString("N") + ".tmp");
            bool created = false;
            try
            {
                long size;
                using (var stream = new FileStream(temporary, FileMode.CreateNew, FileAccess.Write, FileShare.None, 81920, true))
                {
                    created = true;
                    size = await DownloadToAsync(key, stream, cancellationToken).ConfigureAwait(false);
                    await stream.FlushAsync(cancellationToken).ConfigureAwait(false);
                }
                cancellationToken.ThrowIfCancellationRequested();
                File.Move(temporary, destination);
                created = false; return size;
            }
            finally { if (created) File.Delete(temporary); }
        }

        private Task<string> JsonAsync(HttpMethod method, string path, object payload, HttpStatusCode status, bool mutation, CancellationToken ct)
        {
            byte[] bytes = payload == null ? null : Validation.Utf8.GetBytes(JsonConvert.SerializeObject(payload, JsonSupport.Settings()));
            if (bytes != null && bytes.Length > maxJson) throw new ArgumentException("JSON request exceeds client limit.", nameof(payload));
            return ExecuteAsync(method, path, status, mutation, bytes == null ? (Func<CancellationToken, HttpContent>)null : token =>
            {
                var content = new ByteArrayContent(bytes); content.Headers.ContentType = new MediaTypeHeaderValue("application/json") { CharSet = "utf-8" }; return content;
            }, async (response, request, token) => status == HttpStatusCode.NoContent ? null : await ReadJsonAsync(response, token).ConfigureAwait(false), ct);
        }
        private async Task<T> ExecuteAsync<T>(HttpMethod method, string path, HttpStatusCode status, bool mutation, Func<CancellationToken, HttpContent> content, Func<HttpResponseMessage, HttpRequestMessage, CancellationToken, Task<T>> read, CancellationToken cancellationToken)
        {
            if (Volatile.Read(ref disposed) != 0) throw new ObjectDisposedException(nameof(MediatrixClient));
            bool started = false;
            using (var deadline = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken))
            using (var request = new HttpRequestMessage(method, new Uri(baseUri, path)))
            {
                deadline.CancelAfter(timeout);
                request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", token);
                request.Headers.ExpectContinue = false;
                if (content != null)
                {
                    request.Content = content(deadline.Token);
                    // Classic .NET Framework may otherwise buffer unknown-length content.
                    if (request.Content is UploadContent && !request.Content.Headers.ContentLength.HasValue) request.Headers.TransferEncodingChunked = true;
                }
                try
                {
                    deadline.Token.ThrowIfCancellationRequested();
                    started = true;
                    using (HttpResponseMessage response = await http.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, deadline.Token).ConfigureAwait(false))
                    using (deadline.Token.Register(() => response.Dispose()))
                    {
                        if (response.StatusCode != status)
                        {
                            var fields = RawJson.Object(await ReadJsonAsync(response, deadline.Token).ConfigureAwait(false));
                            if (!fields.ContainsKey("error")) throw new FormatException("Missing API error.");
                            throw new MediatrixApiException(response.StatusCode, ParseError(fields["error"]));
                        }
                        T result = await read(response, request, deadline.Token).ConfigureAwait(false);
                        deadline.Token.ThrowIfCancellationRequested();
                        return result;
                    }
                }
                catch (MediatrixException) { throw; }
                catch (Exception e) when (deadline.IsCancellationRequested || e is OperationCanceledException)
                { throw new MediatrixCanceledException(started && mutation, deadline.IsCancellationRequested && !cancellationToken.IsCancellationRequested, e, cancellationToken); }
                catch (Exception e) when (e is JsonException || e is FormatException || e is System.Text.DecoderFallbackException)
                { throw new MediatrixProtocolException("Invalid daemon response. No automatic retry was attempted.", started && mutation, e); }
                catch (Exception e) when (e is HttpRequestException || e is IOException || e is ObjectDisposedException)
                { throw new MediatrixTransportException(started && mutation, e); }
            }
        }
        private async Task<string> ReadJsonAsync(HttpResponseMessage response, CancellationToken ct)
        {
            if (response.Content.Headers.ContentType == null || response.Content.Headers.ContentType.MediaType != "application/json" || response.Content.Headers.ContentEncoding.Count != 0) throw new FormatException("Expected uncompressed application/json.");
            if (response.Content.Headers.ContentLength > maxJson) throw new FormatException("JSON response exceeds client limit.");
            using (Stream source = await response.Content.ReadAsStreamAsync().ConfigureAwait(false))
            using (var destination = new MemoryStream())
            {
                byte[] buffer = new byte[8192]; int count;
                while ((count = await source.ReadAsync(buffer, 0, buffer.Length, ct).ConfigureAwait(false)) != 0)
                {
                    if (destination.Length + count > maxJson) throw new FormatException("JSON response exceeds client limit.");
                    destination.Write(buffer, 0, count);
                }
                if (response.Content.Headers.ContentLength.HasValue && destination.Length != response.Content.Headers.ContentLength.Value) throw new IOException("Truncated JSON response.");
                string json = Validation.Utf8.GetString(destination.ToArray()); RawJson.Validate(json); return json;
            }
        }
        private static Dictionary<string, string> ParseObject(string json, bool unknown)
        {
            try { return RawJson.Object(json); }
            catch (Exception e) when (e is FormatException || e is JsonException) { throw new MediatrixProtocolException("Expected a JSON object.", unknown, e); }
        }
        private static RpcError ParseError(string json)
        {
            var fields = RawJson.Object(json);
            if (!fields.ContainsKey("code") || !fields.ContainsKey("message") || !fields["code"].StartsWith("\"", StringComparison.Ordinal) || !fields["message"].StartsWith("\"", StringComparison.Ordinal)) throw new FormatException("Invalid error fields.");
            if (fields.ContainsKey("outcome_unknown") && fields["outcome_unknown"] != "true" && fields["outcome_unknown"] != "false") throw new FormatException("Invalid outcome flag.");
            var error = new RpcError { Code = ReadString(fields, "code"), Message = ReadString(fields, "message"), OutcomeUnknown = fields.ContainsKey("outcome_unknown") && fields["outcome_unknown"] == "true" };
            if (string.IsNullOrEmpty(error.Code)) throw new FormatException("Missing error code.");
            return error;
        }
        private static string ReadString(Dictionary<string, string> fields, string name)
        {
            string value;
            if (!fields.TryGetValue(name, out value) || !value.StartsWith("\"", StringComparison.Ordinal)) throw new FormatException("Missing/string field expected: " + name);
            return JsonConvert.DeserializeObject<string>(value);
        }
        private static int ReadCount(Dictionary<string, string> fields, string name)
        {
            string raw; int value;
            if (!fields.TryGetValue(name, out raw) || !int.TryParse(raw, NumberStyles.None, CultureInfo.InvariantCulture, out value)) throw new FormatException("Nonnegative integer expected: " + name);
            return value;
        }
        private static string[] ReadStrings(Dictionary<string, string> fields, string name)
        {
            string raw;
            if (!fields.TryGetValue(name, out raw) || !raw.StartsWith("[", StringComparison.Ordinal)) throw new FormatException("String array expected: " + name);
            var values = Newtonsoft.Json.Linq.JArray.Parse(raw); var result = new string[values.Count];
            for (int i = 0; i < values.Count; i++)
            {
                if (values[i].Type != Newtonsoft.Json.Linq.JTokenType.String) throw new FormatException("String array expected: " + name);
                result[i] = (string)values[i];
            }
            return result;
        }
        private static FileInfo ParseFile(string json, bool unknown)
        {
            try
            {
                var fields = RawJson.Object(json);
                string key = ReadString(fields, "key"); Validation.Digest(key);
                string sizeText; long size;
                if (!fields.TryGetValue("size", out sizeText) || !long.TryParse(sizeText, NumberStyles.None, CultureInfo.InvariantCulture, out size)) throw new FormatException("Missing/nonnegative integer file size expected.");
                string[] names = ReadStrings(fields, "allowed_peers");
                return new FileInfo { Key = key, Size = size, AllowedPeers = names };
            }
            catch (Exception e) when (e is ArgumentException || e is FormatException || e is JsonException)
            { throw new MediatrixProtocolException("Invalid file receipt.", unknown, e); }
        }
        internal static string Hex(byte[] bytes) { return BitConverter.ToString(bytes).Replace("-", "").ToLowerInvariant(); }
        /// <summary>Disposes the owned transport and cancels active requests. Do not dispose until all callers have finished.</summary>
        public void Dispose() { if (Interlocked.Exchange(ref disposed, 1) == 0) http.Dispose(); }
    }
}
