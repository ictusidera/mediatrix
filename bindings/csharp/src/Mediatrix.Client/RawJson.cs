using System;
using System.Collections.Generic;
using Newtonsoft.Json;

namespace Mediatrix
{
    // A bounded strict JSON grammar check also retains exact result number/string text.
    // Json.NET alone accepts comments/NaN and converts floating values while reading.
    internal sealed class RawJson
    {
        private readonly string text;
        private int position;
        private RawJson(string text) { this.text = text; }
        internal static void Validate(string text)
        {
            if (text == null) throw new ArgumentNullException(nameof(text));
            var p = new RawJson(text); p.Value(0); p.Space();
            if (p.position != text.Length) throw new FormatException("Extra JSON content.");
        }
        internal static Dictionary<string, string> Object(string text)
        {
            var p = new RawJson(text); p.Space();
            var result = p.ReadObject(0, true); p.Space();
            if (p.position != text.Length) throw new FormatException("Extra JSON content.");
            return result;
        }
        private void Value(int depth)
        {
            if (depth > 64) throw new FormatException("JSON nesting exceeds 64.");
            Space(); if (position >= text.Length) throw new FormatException("Missing JSON value.");
            switch (text[position])
            {
                case '{': ReadObject(depth, false); return;
                case '[':
                    position++; Space(); if (Take(']')) return;
                    do { Value(depth + 1); Space(); if (Take(']')) return; Require(','); } while (true);
                case '"': String(); return;
                case 't': Literal("true"); return;
                case 'f': Literal("false"); return;
                case 'n': Literal("null"); return;
                default: Number(); return;
            }
        }
        private Dictionary<string, string> ReadObject(int depth, bool capture)
        {
            if (depth > 64) throw new FormatException("JSON nesting exceeds 64.");
            Require('{'); Space(); var result = new Dictionary<string, string>(StringComparer.Ordinal);
            if (Take('}')) return result;
            do
            {
                Space(); int start = position; String();
                string name = JsonConvert.DeserializeObject<string>(text.Substring(start, position - start));
                if (result.ContainsKey(name)) throw new FormatException("Duplicate JSON property.");
                Space(); Require(':'); Space(); start = position; Value(depth + 1);
                result.Add(name, capture ? text.Substring(start, position - start) : null);
                Space(); if (Take('}')) return result; Require(',');
            } while (true);
        }
        private void String()
        {
            Require('"');
            while (position < text.Length)
            {
                char c = text[position++]; if (c == '"') return;
                if (c < 0x20) throw new FormatException("Control character in JSON string.");
                if (c != '\\') continue;
                if (position == text.Length) break;
                c = text[position++];
                if (c == 'u')
                {
                    for (int i = 0; i < 4; i++)
                    {
                        if (position == text.Length) throw new FormatException("Incomplete JSON escape.");
                        c = text[position++];
                        if (!((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'))) throw new FormatException("Invalid JSON escape.");
                    }
                }
                else if (c != '"' && c != '\\' && c != '/' && c != 'b' && c != 'f' && c != 'n' && c != 'r' && c != 't') throw new FormatException("Invalid JSON escape.");
            }
            throw new FormatException("Unterminated JSON string.");
        }
        private void Number()
        {
            Take('-');
            if (!Take('0')) { if (!Digit19()) throw new FormatException("Invalid JSON value."); while (Digit()) { } }
            if (Take('.')) { if (!Digit()) throw new FormatException("Invalid JSON fraction."); while (Digit()) { } }
            if (Take('e') || Take('E')) { if (!Take('+')) Take('-'); if (!Digit()) throw new FormatException("Invalid JSON exponent."); while (Digit()) { } }
        }
        private bool Digit19() { if (position < text.Length && text[position] >= '1' && text[position] <= '9') { position++; return true; } return false; }
        private bool Digit() { if (position < text.Length && text[position] >= '0' && text[position] <= '9') { position++; return true; } return false; }
        private void Literal(string s) { for (int i = 0; i < s.Length; i++) Require(s[i]); }
        private bool Take(char c) { if (position < text.Length && text[position] == c) { position++; return true; } return false; }
        private void Require(char c) { if (!Take(c)) throw new FormatException("Invalid JSON structure."); }
        private void Space() { while (position < text.Length && (text[position] == ' ' || text[position] == '\r' || text[position] == '\n' || text[position] == '\t')) position++; }
    }
}
