#!/usr/bin/env python3
"""Standard-library-only Mediatrix loopback handler; prints its URL on startup.

Register the printed URL as service:demo using the operator CLI. Supported
methods: echo (any JSON params) and add (a JSON array of numbers).
"""
import argparse
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MAX_BODY = 1 << 20


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        if self.path != "/rpc":
            self.send_error(404)
            return
        try:
            length = int(self.headers.get("Content-Length", "-1"))
        except ValueError:
            length = -1
        if not 0 <= length <= MAX_BODY:
            self.close_connection = True
            self.send_error(413)
            return
        try:
            request = json.loads(self.rfile.read(length))
            if not isinstance(request, dict) or not isinstance(request.get("caller_peer"), str):
                raise ValueError("missing authenticated caller_peer")
            method = request.get("method")
            params = request.get("params")
            if method == "echo":
                response = {"result": {"params": params, "caller_peer": request["caller_peer"]}}
            elif method == "add" and isinstance(params, list) and all(
                type(value) in (int, float) for value in params
            ):
                response = {"result": sum(params)}
            else:
                response = {"error": {"code": "invalid_method", "message": "use echo, or add with a numeric array"}}
        except (ValueError, TypeError, json.JSONDecodeError):
            response = {"error": {"code": "invalid", "message": "invalid handler request"}}
        try:
            body = json.dumps(response, allow_nan=False).encode("utf-8")
        except (ValueError, OverflowError):
            body = b'{"error":{"code":"invalid","message":"non-finite result"}}'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args):
        # Request bodies and caller information are not written to logs.
        pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--port", type=int, default=0)
    args = parser.parse_args()
    server = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    print(f"http://127.0.0.1:{server.server_port}/rpc", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
