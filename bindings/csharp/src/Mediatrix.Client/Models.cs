using System;
using System.Net;
using System.Threading;
using Newtonsoft.Json;
using Newtonsoft.Json.Linq;

namespace Mediatrix
{
    /// <summary>Options are copied at construction; subsequent changes have no effect.</summary>
    public sealed class MediatrixClientOptions
    {
        public string ApiUrl { get; set; } = "http://127.0.0.1:47832";
        public TimeSpan RequestTimeout { get; set; } = TimeSpan.FromSeconds(45);
        public int MaxJsonBytes { get; set; } = 16 * 1024 * 1024;
        public long MaxFileBytes { get; set; } = 1024L * 1024 * 1024;
    }

    public sealed class NodeInfo
    {
        [JsonProperty("version")] public string Version { get; set; }
        [JsonProperty("peer_id")] public string PeerId { get; set; }
        [JsonProperty("addrs")] public string[] Addresses { get; set; }
        [JsonProperty("connected_peers")] public int ConnectedPeers { get; set; }
        [JsonProperty("services")] public int Services { get; set; }
        [JsonProperty("files")] public int Files { get; set; }
        [JsonProperty("network")] public string Network { get; set; }
    }

    public sealed class ServiceInfo
    {
        [JsonProperty("name")] public string Name { get; set; }
        [JsonProperty("url")] public string Url { get; set; }
        [JsonProperty("allowed_peers")] public string[] AllowedPeers { get; set; }
    }

    public sealed class FileInfo
    {
        [JsonProperty("key")] public string Key { get; set; }
        [JsonProperty("size")] public long Size { get; set; }
        [JsonProperty("allowed_peers")] public string[] AllowedPeers { get; set; }
    }

    public sealed class RpcError
    {
        [JsonProperty("code")] public string Code { get; set; }
        [JsonProperty("message")] public string Message { get; set; }
        [JsonProperty("outcome_unknown")] public bool OutcomeUnknown { get; set; }
    }

    /// <summary>HTTP 200 can carry an application error. Inspect Error or call ThrowIfError.</summary>
    public sealed class RpcResponse
    {
        public string RequestId { get; internal set; }
        /// <summary>Exact JSON result, including the literal null. Null property means an application error.</summary>
        public string ResultJson { get; internal set; }
        public RpcError Error { get; internal set; }
        public bool IsSuccess { get { return Error == null; } }
        public void ThrowIfError()
        {
            if (Error != null) throw new MediatrixApplicationException(RequestId, Error);
        }
        /// <summary>Converts the result on demand. Choose a type that preserves the numeric precision you need.</summary>
        public T DeserializeResult<T>()
        {
            ThrowIfError();
            return JsonConvert.DeserializeObject<T>(ResultJson, JsonSupport.Settings());
        }
    }

    public class MediatrixException : Exception
    {
        public bool OutcomeUnknown { get; private set; }
        internal MediatrixException(string message, bool outcomeUnknown, Exception inner = null)
            : base(message, inner) { OutcomeUnknown = outcomeUnknown; }
    }
    public sealed class MediatrixApiException : MediatrixException
    {
        public HttpStatusCode StatusCode { get; private set; }
        public string Code { get; private set; }
        internal MediatrixApiException(HttpStatusCode status, RpcError error)
            : base(error.Code + ": " + error.Message, error.OutcomeUnknown)
        { StatusCode = status; Code = error.Code; }
    }
    public sealed class MediatrixApplicationException : MediatrixException
    {
        public string RequestId { get; private set; }
        public string Code { get; private set; }
        internal MediatrixApplicationException(string requestId, RpcError error)
            : base(error.Code + ": " + error.Message, error.OutcomeUnknown)
        { RequestId = requestId; Code = error.Code; }
    }
    public sealed class MediatrixTransportException : MediatrixException
    {
        internal MediatrixTransportException(bool unknown, Exception inner)
            : base("The daemon connection failed. No automatic retry was attempted.", unknown, inner) { }
    }
    public sealed class MediatrixProtocolException : MediatrixException
    {
        internal MediatrixProtocolException(string message, bool unknown, Exception inner = null)
            : base(message, unknown, inner) { }
    }
    public sealed class MediatrixIntegrityException : MediatrixException
    {
        internal MediatrixIntegrityException(string message) : base(message, false) { }
    }
    /// <summary>Cancellation does not roll back work already performed by a daemon or handler.</summary>
    public sealed class MediatrixCanceledException : OperationCanceledException
    {
        public bool OutcomeUnknown { get; private set; }
        public bool IsTimeout { get; private set; }
        internal MediatrixCanceledException(bool unknown, bool timeout, Exception inner, CancellationToken token)
            : base(timeout ? "The client request deadline expired." : "The client request was canceled.", inner, token)
        { OutcomeUnknown = unknown; IsTimeout = timeout; }
    }

    internal static class JsonSupport
    {
        internal static JsonSerializerSettings Settings()
        {
            return new JsonSerializerSettings { TypeNameHandling = TypeNameHandling.None, DateParseHandling = DateParseHandling.None, MaxDepth = 64 };
        }
        internal static JsonSerializer Serializer() { return JsonSerializer.Create(Settings()); }
    }
}
