using System;
using System.IO;
using System.Net;
using System.Net.Http;
using System.Net.Http.Headers;
using System.Security.Cryptography;
using System.Threading;
using System.Threading.Tasks;

namespace Mediatrix
{
    internal sealed class UploadContent : HttpContent
    {
        private readonly Stream source;
        private readonly long? length;
        private readonly long limit;
        private readonly CancellationToken token;
        private int started;
        internal long BytesCopied { get; private set; }
        internal string Digest { get; private set; }
        internal UploadContent(Stream source, long? length, long limit, CancellationToken token)
        {
            this.source = source; this.length = length; this.limit = limit; this.token = token;
            Headers.ContentType = new MediaTypeHeaderValue("application/octet-stream");
        }
        protected override bool TryComputeLength(out long value) { value = length ?? 0; return length.HasValue; }
        protected override async Task SerializeToStreamAsync(Stream destination, TransportContext context)
        {
            if (Interlocked.Exchange(ref started, 1) != 0) throw new IOException("Upload replay is disabled.");
            using (var hash = SHA256.Create())
            {
                byte[] buffer = new byte[81920]; int count;
                while ((count = await source.ReadAsync(buffer, 0, buffer.Length, token).ConfigureAwait(false)) != 0)
                {
                    BytesCopied += count;
                    if (BytesCopied > limit || (length.HasValue && BytesCopied > length.Value)) throw new IOException("Upload exceeds the client limit or declared length.");
                    hash.TransformBlock(buffer, 0, count, null, 0);
                    await destination.WriteAsync(buffer, 0, count, token).ConfigureAwait(false);
                }
                if (length.HasValue && BytesCopied != length.Value) throw new IOException("Source length changed during upload.");
                hash.TransformFinalBlock(new byte[0], 0, 0); Digest = MediatrixClient.Hex(hash.Hash);
            }
        }
        // Deliberately do not dispose the caller-owned source stream.
    }
}
