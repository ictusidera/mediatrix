using System;
using System.Globalization;
using System.Net;
using System.Net.Sockets;
using System.Text;
using System.Text.RegularExpressions;

namespace Mediatrix
{
    internal static class Validation
    {
        internal static readonly UTF8Encoding Utf8 = new UTF8Encoding(false, true);
        internal static Uri LoopbackUrl(string value, bool handler)
        {
            if (value == null) throw new ArgumentNullException(nameof(value));
            var match = Regex.Match(value, @"\Ahttp://(?<host>\[[0-9A-Fa-f:.]+\]|[0-9.]+):(?<port>[0-9]+)(?<path>/[^?#]*)?\z", RegexOptions.CultureInvariant);
            Uri uri; IPAddress address; int port;
            string host = match.Groups["host"].Value.Trim('[', ']');
            if (!match.Success || !int.TryParse(match.Groups["port"].Value, NumberStyles.None, CultureInfo.InvariantCulture, out port) || port < 1 || port > 65535 ||
                !IPAddress.TryParse(host, out address) || !IPAddress.IsLoopback(address) ||
                (address.AddressFamily == AddressFamily.InterNetwork && address.ToString() != host) ||
                !Uri.TryCreate(value, UriKind.Absolute, out uri) || uri.HostNameType == UriHostNameType.Dns || uri.UserInfo.Length != 0 || uri.Query.Length != 0 || uri.Fragment.Length != 0 ||
                (!handler && match.Groups["path"].Value != "" && match.Groups["path"].Value != "/"))
                throw new ArgumentException("Use http://<numeric loopback IP>:<explicit port> with no credentials, query or fragment; API URLs cannot include a path.", nameof(value));
            foreach (char c in value) if (char.IsControl(c) || char.IsWhiteSpace(c) || c == '\\') throw new ArgumentException("Invalid URL characters.", nameof(value));
            return uri;
        }
        internal static void Service(string service)
        {
            if (service == null || !Regex.IsMatch(service, @"\Aservice:[A-Za-z0-9][A-Za-z0-9._/-]{0,127}\z", RegexOptions.CultureInvariant)) throw new ArgumentException("Invalid service name.", nameof(service));
        }
        internal static string Digest(string key)
        {
            if (key == null || !Regex.IsMatch(key, @"\Afile:sha256:[a-f0-9]{64}\z", RegexOptions.CultureInvariant)) throw new ArgumentException("Invalid file:sha256 key.", nameof(key));
            return key.Substring(12);
        }
        internal static void Timeout(long milliseconds)
        {
            if (milliseconds < 0 || milliseconds > 300000) throw new ArgumentOutOfRangeException(nameof(milliseconds), "Use 0 (daemon default) or 1..300000; daemon limits still apply.");
        }
        internal static string[] Peers(string[] peers)
        {
            if (peers == null) return new string[0];
            var copy = (string[])peers.Clone();
            foreach (string peer in copy) if (string.IsNullOrWhiteSpace(peer)) throw new ArgumentException("Peer IDs cannot be empty.", nameof(peers));
            return copy;
        }
    }
}
