#!/usr/bin/env python3
"""Package already-built net462 DLLs, source, sample, and dependency notice."""
import argparse
import hashlib
from pathlib import Path
import subprocess
import zipfile

ROOT = Path(__file__).resolve().parents[3]
CSHARP = ROOT / 'bindings/csharp'


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--output', type=Path, default=CSHARP / 'artifacts/mediatrix-csharp-net462.zip')
    args = parser.parse_args()
    entries = {}
    def add(path, name):
        entries[name] = path.read_bytes()
    library = CSHARP / 'src/Mediatrix.Client/bin/Release/net462'
    for name in ['Mediatrix.Client.dll', 'Mediatrix.Client.xml', 'Mediatrix.Client.pdb', 'Newtonsoft.Json.dll']:
        add(library / name, 'lib/net462/' + name)
    sample = CSHARP / 'samples/ConsoleDemo/bin/Release/net462'
    for name in ['ConsoleDemo.exe', 'ConsoleDemo.exe.config', 'ConsoleDemo.pdb', 'Mediatrix.Client.dll', 'Newtonsoft.Json.dll']:
        add(sample / name, 'samples/ConsoleDemo/net462/' + name)
    if entries['lib/net462/Mediatrix.Client.dll'] != entries['samples/ConsoleDemo/net462/Mediatrix.Client.dll']:
        raise SystemExit('Sample DLL is stale. Rebuild the Release sample first.')
    add(CSHARP / 'THIRD-PARTY-NOTICES.txt', 'THIRD-PARTY-NOTICES.txt')
    # Source only; git's ignore rules exclude bin/obj, credentials, state, and logs.
    files = subprocess.check_output(['git', 'ls-files', '-z', '--cached', '--others', '--exclude-standard'], cwd=ROOT).decode().split('\0')
    for name in sorted(set(files) - {''}):
        add(ROOT / name, 'source/' + name)
    patch = subprocess.check_output(['git', 'diff', '--binary', 'HEAD', '--'], cwd=ROOT)
    if patch:
        entries['changes.patch'] = patch
    entries['README.txt'] = ('''Mediatrix C# binding / .NET Framework 4.6.2\n\nlib/net462/: AnyCPU client DLL, XML/PDB, required Newtonsoft.Json 13.0.4 net45 DLL\nsamples/ConsoleDemo/net462/: runnable Windows .NET Framework sample\nsource/: repository source including bindings/csharp and language-specific README files\nsource/bindings/csharp/README.md: installation, API, samples, build and test instructions\nsource/bindings/csharp/VERIFICATION.md: actual verification and limits\nTHIRD-PARTY-NOTICES.txt: Newtonsoft.Json license\nSHA256SUMS.txt: SHA-256 for each entry except this manifest\n\nUse the existing local daemon and its API token. The DLL does not start the daemon.\nNo .NET 8 runtime is needed to consume the net462 DLL from a .NET Framework app.\nSee VERIFICATION.md for the dated execution results and remaining platform limits.\n''').encode('utf-8')
    entries['SHA256SUMS.txt'] = ''.join(hashlib.sha256(data).hexdigest() + '  ' + name + '\n' for name, data in sorted(entries.items())).encode()
    args.output.parent.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(args.output, 'w', zipfile.ZIP_DEFLATED) as z:
        for name, data in sorted(entries.items()):
            z.writestr(name, data)
    with zipfile.ZipFile(args.output) as z:
        if z.testzip() is not None:
            raise RuntimeError('ZIP verification failed.')
        for line in z.read('SHA256SUMS.txt').decode().splitlines():
            digest, name = line.split('  ', 1)
            assert hashlib.sha256(z.read(name)).hexdigest() == digest, name
    print(str(args.output.resolve()))
    print('ZIP entries:', len(entries))
    print('ZIP bytes:', args.output.stat().st_size)
    print('ZIP SHA256:', hashlib.sha256(args.output.read_bytes()).hexdigest())
    print('CRC and every manifest SHA256 verified.')


if __name__ == '__main__':
    main()
