# C#バインディング検証記録

実施日: 2026-10-03 UTC。Linux amd64 / Debian 13。対象は `Mediatrix.Client.dll` 0.2.0、target `.NETFramework,Version=v4.6.2`。元のdaemonは `rebuild/mediatrix` の `677144004581616c40bdea770177f76cbd7f0dbf` と同じsource treeを使用しました。初回ローカル検証後に `bindings/csharp` branchへ公開しました。公開後のWindows/Linux CIも合格し、結果を下記に追記しています。

## 実施して合格

- 公式Microsoft .NET SDK 8.0.425を、公式release metadata記載SHA-512と一致確認して使用
- Microsoft.NETFramework.ReferenceAssemblies.net462 **1.0.3** で本体DLL/console sample/net462 test harnessを実compile。targetの付け替えだけではない
- Newtonsoft.Json **13.0.4**。配布物はnet45 runtime DLL、reference assembliesは配布しない
- 本体DLL、console sample、net8.0/net462 test harnessのRelease build: warning 0 / error 0
- 実DLLのTargetFrameworkAttribute、CLR v4 metadata、IL-only AnyCPUをテストで検査
- **167 / 167 contract cases**: Linux .NET 8.0.31 hostから実net462 DLLをロードして実行
- **168 / 168 cases**: 上記＋実Go daemonとの結合scenario。health/node/services、認証、service登録・削除、echo/null/application error、300,123 bytesのbinary import・ACL・fetch・stream/file download・no-overwriteを実assert
- 実Go daemon＋実C# console/HTTP handler sample: RPC結果42、**262,404 bytes** binaryのimport・ACL・fetch・SHA256検証付きdownload、入力と出力のbyte一致
- Go daemon回帰: Go 1.26.7、`go test -race -count=1 ./...` 全package、`go vet ./...`、daemon/CLI build
- Python補助scriptのsyntax compile、`git diff --check`
- ZIP CRC、全fileのSHA-256 manifest、配布DLLと試験したDLLのSHA-256一致

contract testsは各caseを名前付きで出力します。URL/token/JSON/型schema、APIとapplication error分離、exact JSON数値保持、壊れた/途切れたRPC応答、不明結果flag、再試行無し、送信前cancel、header受信後のbody停止/cancel/deadline、stream所有権、nonseekable chunked upload、hash/size/上限制御、一時filecleanup/no-overwrite競合、proxy/redirect/cookie防止、24並行RPC/8並行download等を含みます。

DLL SHA-256:

```text
2b337642f5aa78945ca991e096f0d4b3b2037fc6ee48396585041961d11d013f
```

.NET 8 harnessは同じclient DLLを使用しますが、Newtonsoftはhost互換のnet6.0 assetを選びます。Windows配布用のnet45 assetをLinux .NET8へ載せたときのSystem.Security.Permissions不足を、DLLのWindows互換性合格/失敗と取り違えていません。

## 実施していないこと

- .NET Framework **4.6.2そのもの**がインストールされた環境での実行。net462参照でのcompileとWindows/.NET Framework 4.8.1での実行は確認済み
- 利用者PCでのHttpListener URL reservation設定、NTFS ACL、WinForms/WPF実画面での試験（CIのWindows C# HttpListener sampleは動作確認済み）
- C#クライアント自体による実WAN/NAT/relay/複数地域、長期負荷、電源断耐久性試験
- batchファイルAPI、1stream複数file framing。今回の対象外

既存daemonの[検証記録とDHTの既知脆弱性](../../docs/verification.md)も引き続き適用されます。このバインディング追加はその問題の修正ではありません。

## 再現

```sh
dotnet restore bindings/csharp/samples/ConsoleDemo/ConsoleDemo.csproj --locked-mode
dotnet build bindings/csharp/samples/ConsoleDemo/ConsoleDemo.csproj -c Release --no-restore -m:1 /p:UseSharedCompilation=false
dotnet restore bindings/csharp/tests/Mediatrix.Client.Tests/Mediatrix.Client.Tests.csproj --locked-mode
dotnet build bindings/csharp/tests/Mediatrix.Client.Tests/Mediatrix.Client.Tests.csproj -c Release --no-restore -m:1 /p:UseSharedCompilation=false
dotnet run --project bindings/csharp/tests/Mediatrix.Client.Tests -c Release -f net8.0 --no-build
go build -o mediatrixd ./cmd/mediatrixd
python3 bindings/csharp/scripts/smoke-daemon.py --daemon ./mediatrixd
```

この環境ではMSBuildのparallel worker/serverが一部の実行でwarning/error無しの失敗になりました。`-m:1 /p:UseSharedCompilation=false` または `--disable-build-servers -m:1` で再現可能にcompileしています。runtime/code failureとして扱っていません。

Windowsでは `scripts/build.ps1 -RunTests` がnet462 EXEを実行し、`smoke-daemon.py --framework` が.NET Framework上でC# sampleを実行します。詳細は [README](README.md) と [tests README](tests/Mediatrix.Client.Tests/README.md) を参照してください。

## 公開後のWindows / Linux CI: 合格

検証した実装commit: [`e06a41a33c20fa8e8fc2332af2119247e1c9581f`](https://github.com/ictusidera/mediatrix/commit/e06a41a33c20fa8e8fc2332af2119247e1c9581f)。

- [C# binding CI](https://github.com/ictusidera/mediatrix/actions/runs/37125451025): Windows/Linuxの両jobがsuccess
- [Go daemon CI](https://github.com/ictusidera/mediatrix/actions/runs/37125451014): Windows/Linuxの両jobがsuccess。Linux race/2-daemon Python smokeも合格
- Windows Server 2025 runner: registry `Version=4.8.09221` / `Release=533509`、.NET Framework **4.8.1**。バージョン対応は[Microsoftの判定資料](https://learn.microsoft.com/en-us/dotnet/framework/install/how-to-determine-which-versions-are-installed)による
- Windowsでnet462 client DLL＋配布と同じnet45 Newtonsoft assetを実ロードし、**167 / 167 contract cases**が合格
- 同Windows環境で実Go daemonとの結合を含む **168 / 168 cases**、C# handlerのRPC42、262,404-byte binary import/ACL/fetch/検証付きdownloadが合格
- Linuxは引き続き.NET8 host＋net462 client DLL＋host互換Newtonsoft assetで同試験に合格

.NET Framework 4.8.1での成功を、4.6.2 runtimeそのものの実行確認とは呼びません。ターゲットの4.6.2互換性は正式なreference assembliesによるcompileで確認しています。

初回Windows CIでは、暗黙reference packageとOS別RIDによるlocked restore不一致を発見し、SDK feature band/AnyCPU/明示reference packageを統一して修正しました。次の実行で見つかったテストfixtureの途中切断chunk-size EOF処理も修正しました。SDKのruntime API/source logicを変えず、最終commitの全jobが合格しています。既知DHT脆弱性の解決や脆弱性scanのcleanを示すものではありません。
