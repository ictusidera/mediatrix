param([switch]$RunTests)
$ErrorActionPreference = 'Stop'
$Binding = Split-Path $PSScriptRoot -Parent
$Sample = Join-Path $Binding 'samples/ConsoleDemo/ConsoleDemo.csproj'
$Tests = Join-Path $Binding 'tests/Mediatrix.Client.Tests/Mediatrix.Client.Tests.csproj'
dotnet restore $Sample --locked-mode
if ($LASTEXITCODE) { throw 'Sample restore failed' }
dotnet build $Sample -c Release --no-restore -m:1 /p:UseSharedCompilation=false
if ($LASTEXITCODE) { throw 'Sample build failed' }
dotnet restore $Tests --locked-mode
if ($LASTEXITCODE) { throw 'Tests restore failed' }
dotnet build $Tests -c Release --no-restore -m:1 /p:UseSharedCompilation=false
if ($LASTEXITCODE) { throw 'Tests build failed' }
if ($RunTests) {
    if ($env:OS -eq 'Windows_NT') {
        & (Join-Path $Binding 'tests/Mediatrix.Client.Tests/bin/Release/net462/Mediatrix.Client.Tests.exe')
    } else {
        dotnet run --project $Tests -c Release -f net8.0 --no-build
    }
    if ($LASTEXITCODE) { throw 'Contract tests failed' }
}
