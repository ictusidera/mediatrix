# C# binding: Mediatrix.Client.dll

.NET Framework **4.6.2** をtargetとするAnyCPU managed class libraryです。WinForms/WPF/console等から、同じマシンで起動済みのMediatrix daemonを非同期で操作します。daemonの起動・停止やWindows Service登録はDLLに含みません。

## DLLを利用する

配布ZIPの `lib/net462/` には次を含みます。

- `Mediatrix.Client.dll`: 本体
- `Mediatrix.Client.xml`: IntelliSense XML documentation
- `Mediatrix.Client.pdb`: デバッグシンボル
- `Newtonsoft.Json.dll`: 13.0.4の `net45` asset。必須の実行時依存

Visual Studioの.NET Framework 4.6.2プロジェクトで本体DLLを参照し、同梱 `Newtonsoft.Json.dll` も参照/Copy Localしてください。Framework標準の `System.Net.Http` は参照アセンブリをアプリの配布物としてコピーしません。既存アプリで別のNewtonsoft版を使用している場合は依存を13.0.4へ揃え、必要なassembly binding redirectをアプリ側で設定して試験してください。console sampleは自動redirect生成を有効にしています。

利用先には.NET Framework 4.6.2（または互換性のある後継4.x runtime）が必要です。**このDLLを使うために.NET 8 runtime/SDKをインストールする必要はありません**。ビルド・Linux上での検証には.NET SDKを使いますが、配布DLLのtargetはnet462です。新しい.NET向けのSDK配布を保証するものではありません。

NuGetのMicrosoft.NETFramework.ReferenceAssemblies.net462 1.0.3はビルド専用です。配布/実行するのは上記の本体とNewtonsoft依存です。Newtonsoftのライセンスは `THIRD-PARTY-NOTICES.txt` に同梱しています。

## 最小呼出例

```csharp
using System;
using System.Threading;
using Mediatrix;

// 起動済みdaemonと同じtokenを安全に設定しておく。ソース/URL/ログへ埋め込まない。
var token = Environment.GetEnvironmentVariable("MEDIATRIX_API_TOKEN");
using (var client = new MediatrixClient(token))
{
    var node = await client.GetNodeAsync();
    Console.WriteLine(node.PeerId);

    var response = await client.CallAsync(
        "service:demo", "add", "[20,22]",
        requestId: Guid.NewGuid().ToString("N"),
        timeoutMilliseconds: 10000,
        cancellationToken: CancellationToken.None);
    response.ThrowIfError();
    Console.WriteLine(response.ResultJson);       // 42
    int result = response.DeserializeResult<int>();
}
```

既定URLは `http://127.0.0.1:47832`。変更する場合:

```csharp
var options = new MediatrixClientOptions {
    ApiUrl = "http://127.0.0.1:47833",
    RequestTimeout = TimeSpan.FromSeconds(125)
};
var client = new MediatrixClient(token, options);
```

API URLはnumeric loopback HTTP＋明示port限定です。DNS名（`localhost`も含む）、proxy、redirect、cookie、既定OS資格情報は使いません。設定は構築時にコピーされます。tokenのローテーション後は新しいclientを作成してください。token文字列を明示的にメモリ消去する機能はありません。

## UIから呼ぶ

WinForms/WPFではイベントハンドラを `async` にし、各呼出しを `await` してください。`.Result` / `.Wait()` / `.GetAwaiter().GetResult()` でUI threadを塞がないでください。

```csharp
private async void CallButton_Click(object sender, EventArgs e)
{
    try {
        var response = await client.CallAsync("service:demo", "echo", "{\"hello\":\"world\"}");
        response.ThrowIfError();
        resultTextBox.Text = response.ResultJson;
    }
    catch (MediatrixException ex) {
        resultTextBox.Text = ex.Message + " / outcome_unknown=" + ex.OutcomeUnknown;
    }
    catch (MediatrixCanceledException ex) {
        resultTextBox.Text = "中断 / outcome_unknown=" + ex.OutcomeUnknown;
    }
}
```

clientはアプリ/フォームで再利用でき、HTTP接続を再利用します。独立した複数requestの並行実行に対応します。1回のfile APIは1ファイルです。`Stream` はその1ファイルを全量メモリに載せず転送するためのもので、複数ファイルを1streamで処理するbatch APIは未実装です。SDK内部は `ConfigureAwait(false)` を使います。全呼出しの終了後にclientをDisposeしてください。Disposeは所有HTTP接続と進行中requestを終了させます。共有streamや同じ出力pathを並行操作する同期は呼出側の責任です。入力model/JSONを実行中に変更しないでください。

## ServiceをC#で登録・呼び出す

```csharp
await client.RegisterServiceAsync("service:demo", "http://127.0.0.1:9000/rpc");
var services = await client.GetServicesAsync();
// 全ACLを置換する。peer IDはdaemonのallowed_peersにも登録済みであること
await client.RegisterServiceAsync("service:demo", "http://127.0.0.1:9000/rpc", new[] { peerId });
await client.RemoveServiceAsync("service:demo");
```

空/nullの `allowedPeers` は `[]` を送信し、remote denyになります。登録は同名serviceの上書きで、handlerを起動する処理ではありません。handler URLもnumeric loopback HTTP＋明示port限定、query/fragment/userinfoは不可です。

## ファイルの取り込み・取得

```csharp
// 1. このdaemonのcontent storeへ取り込む。中央サーバーへのuploadではない
var imported = await client.UploadFileAsync("input.bin");
// 2. このcopyを読み取れるpeerのACLを全置換する
await client.SetFileAccessAsync(imported.Key, new[] { peerId });
// 3. 取得側daemonで呼ぶ。localに無ければ許可されたpeerから複製する
var local = await client.FetchAsync(imported.Key, timeoutMilliseconds: 10000);
// 4. 取得側daemonのlocal storeから新しい出力fileへ取り出す
await client.DownloadFileAsync(local.Key, "new-output.bin");
```

`FetchAsync` はファイルをアプリの指定pathへ保存しません。`DownloadFileAsync` はremote fetchを暗黙に実行しません。fetchしたcopyのACLは既定で空です。既存copyをACL変更で相手から回収することはできません。

`UploadAsync(Stream)` / `DownloadToAsync(key, Stream)` はbuffered streamingで、全fileをメモリへ載せません。callerのstreamは閉じません。uploadは現在のPositionからEOFまでを送ります。成功receiptのkey/sizeと送ったSHA-256/sizeを照合します。

downloadはSHA-256、Content-Length（存在する場合）、最大サイズを検証します。`DownloadToAsync` で失敗した場合、出力streamには未検証/部分的なbytesが残り得ます。必ず破棄してください。安全なfile公開には `DownloadFileAsync` を使ってください。同じdirectoryの一時fileへ書いて検証後にno-overwrite moveし、既存fileは置換しません。失敗時は一時fileを削除します。filesystem/permissionエラーが発生した場合は例外を確認してください。出力directoryのACL/permissionはアプリ側で制限してください。SDKはWindows ACL/Unix file modeを変更せず、OS既定権限を継承します。power-loss durabilityを保証するものではありません。

## エラー・cancel・不明な結果

- HTTP 200のhandler application error: `RpcResponse.Error` に保持。必要なら `ThrowIfError()` で `MediatrixApplicationException`
- 非成功HTTPの正しいAPI error envelope: `MediatrixApiException`。`StatusCode`、`Code`、`OutcomeUnknown`を保持
- 切断等: `MediatrixTransportException`
- 壊れた/過大/不正schemaの応答、RPC request ID不一致: `MediatrixProtocolException`
- download hash/size不一致: `MediatrixIntegrityException`
- caller cancel / client期限: `MediatrixCanceledException`（`OperationCanceledException`の派生）。`IsTimeout`、`OutcomeUnknown`
- 引数、local file、型変換の問題: 通常の.NET例外（ArgumentException、IOException、JsonException等）

RPCの `result:null` は成功です。`ResultJson` はJSON文字列をそのまま保持し、ISO日時風文字列や巨大数値の精度を勝手に変えません。`DeserializeResult<T>()` は利用者が選んだ型へ変換するため、その型の精度・範囲制約が適用されます。

**SDKは自動再試行しません。** 送信開始後の通信/応答解析失敗やcancelでは、RPCその他の更新が実行済みか不明な場合に `OutcomeUnknown=true` とします。daemonが返したAPI errorのflagはそのまま保持します。未知なら状態確認やアプリの重複排除処理が必要です。request IDは相関IDにすぎず、同じIDで再送しても重複実行されます。cancelは既に発生した副作用を取り消しません。

`RequestTimeout` はHTTP header/body全体のclient側期限で、既定45秒、1ms〜310秒です。`timeoutMilliseconds` はdaemon処理の短縮期限で、0がdaemon既定、1〜300000が明示値です。daemon設定の上限を延長しません。fetchとdownloadは別request/別client期限です。一連処理の総期限はcallerのCancellationTokenSourceで設定してください。任意のcaller streamはReadAsync/WriteAsyncのCancellationTokenに協調する必要があり、tokenを無視する独自streamまで強制停止する保証はありません。

client既定上限はJSON16MiB/file1GiB、optionsで引き下げ可能です。daemonの既定JSON1MiB/file64MiBとは別であり、daemon側の小さい上限が優先されます。JSONは深さ64まで、duplicate object member、コメント、NaN/Infinityを拒否します。daemonより厳しいclient側制限です。

## ビルド・実行可能サンプル

開発環境には.NET SDK 8.0.4xx（検証版8.0.425）と、NuGet.orgへのアクセスが必要です。リポジトリのglobal.jsonは同feature bandの最新patchを選び、別major SDKの自動選択を防ぎます。Microsoftの正式なnet462 reference assembliesをNuGetからrestoreするため、Linuxでもtargetを偽装せずcompileできます。

```powershell
dotnet restore bindings/csharp/samples/ConsoleDemo/ConsoleDemo.csproj --locked-mode
dotnet build bindings/csharp/samples/ConsoleDemo/ConsoleDemo.csproj -c Release --no-restore
# Windows / .NET Framework: 起動済みdaemonと同じtokenを環境変数へ設定済みとする
.\bindings\csharp\samples\ConsoleDemo\bin\Release\net462\ConsoleDemo.exe node
```

同梱 `samples/ConsoleDemo/Program.cs` はDLL利用側とHTTP handlerの両方を示します。

```powershell
# terminal 1: C# HTTP handler（tokenは不要）
.\ConsoleDemo.exe handler
# terminal 2: daemonのtokenを設定済みとする
.\ConsoleDemo.exe rpc
.\ConsoleDemo.exe files .\input.bin .\new-output.bin
```

handlerは `http://127.0.0.1:49080/rpc/` で `echo` / `add` を提供します。rpcモードは `service:csharp-demo` をlocal-onlyで登録して42を呼び出します。登録は残ります。不要になったら `mediatrix remove-service --name service:csharp-demo` で削除してください。handler終了で登録が自動削除されるわけではありません。

WindowsのHttpListenerがAccess Deniedになる場合、管理者がその固定URLだけを実行ユーザーに予約する必要があることがあります。例（`DOMAIN\User`を実際のaccountへ置換）:

```powershell
netsh http add urlacl url=http://127.0.0.1:49080/rpc/ user="DOMAIN\User"
# 不要になったreservationの管理者による撤去:
netsh http delete urlacl url=http://127.0.0.1:49080/rpc/
```

このOS設定変更はサンプルが自動実行しません。handlerを常時管理者として実行することを前提にしません。既存HTTP serverへ同じ[handler契約](../../docs/api.md#handler-の実装)を実装することもできます。

handler sampleはtrusted local machine用の小さな逐次処理例です。サンプルはJson.NETの通常の数値変換とdecimal加算を使い、極端な小数・指数・範囲の精度維持は保証しません。任意JSONの厳密なechoが必要なhandlerはraw JSON保持を実装してください。`caller_peer` はdaemonが付けますが、他のlocal processもhandlerへ接続できるため単独のlocalhost認証にはなりません。本番の認可、rate limiting、timeout/cancel協調、idempotency、side effect管理はアプリケーション側で実装してください。

## 検証

[検証記録](VERIFICATION.md)に実施済みと未実施を区別して記載しています。

```powershell
# 先にRelease DLLをbuild
dotnet restore bindings/csharp/tests/Mediatrix.Client.Tests/Mediatrix.Client.Tests.csproj --locked-mode
dotnet build bindings/csharp/tests/Mediatrix.Client.Tests/Mediatrix.Client.Tests.csproj -c Release --no-restore
# Windows: Framework runtime上で実行
.\bindings\csharp\tests\Mediatrix.Client.Tests\bin\Release\net462\Mediatrix.Client.Tests.exe
# Linux: 同じnet462 client DLLを.NET8 test hostから実行
dotnet run --project bindings/csharp/tests/Mediatrix.Client.Tests -c Release -f net8.0 --no-build
```

.NET8 harnessはNewtonsoftの.NET8互換assetを選択します。Windows配布のnet45依存と同一runtime構成ではなく、Windows/net462実行試験の代用ではありません。Windows/LinuxのCI workflowも用意していますが、remoteで実行していない状態を合格扱いしません。

実daemon＋C# handler＋binary fileのsmoke試験は `python3 bindings/csharp/scripts/smoke-daemon.py --daemon /absolute/path/to/mediatrixd`（Linux）、Windowsでは `python ... --daemon C:\path\mediatrixd.exe --framework` です。事前にsample/test harnessをRelease buildし、handler port49080を空けてください。temporary data/tokenは試験の終了時に削除されます。

Release build後に `python3 bindings/csharp/scripts/package.py` を実行すると、DLL＋必要依存＋Windowsサンプル＋source＋SHA256一覧を `bindings/csharp/artifacts/mediatrix-csharp-net462.zip` にまとめます。DLL/bin/obj/ZIPはgit管理対象にしません。ソース差分がある場合は `changes.patch` も含みます（新規fileはgitにaddしてからpackageしてください）。
