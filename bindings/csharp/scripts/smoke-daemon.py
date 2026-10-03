#!/usr/bin/env python3
"""Exercise the delivered net462 client + C# sample against an actual daemon.

On Linux only, a .NET 8 test host and the package's compatible Newtonsoft asset
are used. This does not claim Windows/.NET Framework runtime validation.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.request

ROOT = Path(__file__).resolve().parents[3]
CSHARP = ROOT / 'bindings/csharp'


def available_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--daemon', required=True, type=Path)
    parser.add_argument('--framework', action='store_true', help='Run the sample EXE on Windows .NET Framework')
    args = parser.parse_args()
    sample = CSHARP / 'samples/ConsoleDemo/bin/Release/net462'
    if not (sample / 'ConsoleDemo.exe').is_file():
        raise SystemExit('Build the Release console sample first.')
    with tempfile.TemporaryDirectory(prefix='mediatrix-csharp-') as tmp:
        work = Path(tmp)
        app = work / 'app'
        shutil.copytree(sample, app)
        if args.framework:
            command = [str(app / 'ConsoleDemo.exe')]
        else:
            test_json = CSHARP / 'tests/Mediatrix.Client.Tests/bin/Release/net8.0/Newtonsoft.Json.dll'
            if not test_json.is_file():
                raise SystemExit('Build the net8.0 test harness first (compatible Newtonsoft asset required).')
            shutil.copy2(test_json, app / 'Newtonsoft.Json.dll')
            runtime = app / 'test.runtimeconfig.json'
            runtime.write_text(json.dumps({'runtimeOptions': {'tfm': 'net8.0', 'framework': {'name': 'Microsoft.NETCore.App', 'version': '8.0.0'}}}))
            command = ['dotnet', 'exec', '--runtimeconfig', str(runtime), str(app / 'ConsoleDemo.exe')]
        # Sample intentionally uses a documented fixed handler port. Fail on conflict.
        with socket.socket() as check:
            check.bind(('127.0.0.1', 49080))
        api_port = available_port()
        token = secrets.token_urlsafe(32)
        env = dict(os.environ, MEDIATRIX_API_TOKEN=token, MEDIATRIX_API_URL=f'http://127.0.0.1:{api_port}')
        config = {'data_dir': str(work / 'data'), 'api_addr': f'127.0.0.1:{api_port}', 'network': 'csharp-smoke',
                  'listen': ['/ip4/127.0.0.1/tcp/0'], 'allowed_peers': [], 'bootstrap': [], 'relays': [],
                  'relay_service': False, 'dht_server': False}
        config_path = work / 'config.json'
        config_path.write_text(json.dumps(config))
        children = []
        try:
            with (work / 'daemon.log').open('w+') as daemon_log, (work / 'handler.log').open('w+') as handler_log:
                daemon = subprocess.Popen([str(args.daemon.resolve()), '--config', str(config_path)], env=env, stdout=daemon_log, stderr=subprocess.STDOUT)
                children.append(daemon)
                deadline = time.monotonic() + 20
                while True:
                    try:
                        request = urllib.request.Request(env['MEDIATRIX_API_URL'] + '/v1/health', headers={'Authorization': 'Bearer ' + token})
                        with urllib.request.urlopen(request, timeout=1) as response:
                            assert response.status == 200
                        break
                    except Exception:
                        if daemon.poll() is not None or time.monotonic() > deadline:
                            daemon_log.seek(0)
                            raise RuntimeError('Daemon failed readiness: ' + daemon_log.read())
                        time.sleep(0.1)
                test_root = CSHARP / 'tests/Mediatrix.Client.Tests/bin/Release'
                test_command = ([str(test_root / 'net462/Mediatrix.Client.Tests.exe')] if args.framework else ['dotnet', str(test_root / 'net8.0/Mediatrix.Client.Tests.dll')])
                tests = subprocess.run(test_command + ['--daemon', env['MEDIATRIX_API_URL']], env=env, capture_output=True, text=True, timeout=60)
                if tests.returncode:
                    raise RuntimeError('Real-daemon contract suite failed: ' + tests.stdout + tests.stderr)
                for line in tests.stdout.splitlines():
                    if line.startswith('RESULT:') or line.startswith('PASS real daemon:'):
                        print(line)
                handler = subprocess.Popen(command + ['handler'], env=env, stdout=handler_log, stderr=subprocess.STDOUT)
                children.append(handler)
                deadline = time.monotonic() + 10
                while True:
                    try:
                        with socket.create_connection(('127.0.0.1', 49080), timeout=.5):
                            break
                    except OSError:
                        if handler.poll() is not None or time.monotonic() > deadline:
                            handler_log.seek(0)
                            raise RuntimeError('Handler failed readiness: ' + handler_log.read())
                        time.sleep(.1)
                def run(*extra):
                    result = subprocess.run(command + list(extra), env=env, capture_output=True, text=True, timeout=30)
                    if result.returncode:
                        raise RuntimeError('Sample failed: ' + result.stdout + result.stderr)
                    return result.stdout
                node = run('node')
                assert 'Network: csharp-smoke' in node
                rpc = run('rpc')
                assert 'Result: 42' in rpc, rpc
                source, output = work / 'input.bin', work / 'output.bin'
                source.write_bytes(bytes(range(256)) * 1025 + b'\x00\xff\r\n')
                transfer = run('files', str(source), str(output))
                assert source.read_bytes() == output.read_bytes()
                assert hashlib.sha256(source.read_bytes()).hexdigest() in transfer
                print('PASS: real daemon node + C# handler registration/RPC42 + binary import/ACL/fetch/verified file download (262404 bytes)')
                print('Runtime: ' + ('Windows .NET Framework' if args.framework else 'Linux/.NET 8 host loading the net462 client DLL; compatible Newtonsoft test asset'))
        finally:
            for child in reversed(children):
                child.terminate()
            for child in reversed(children):
                try: child.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    child.kill(); child.wait()


if __name__ == '__main__':
    main()
