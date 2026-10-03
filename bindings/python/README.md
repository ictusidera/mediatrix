# Python: HTTP handlerサンプル

現時点ではPython用クライアントSDK/DLLはありません。[標準ライブラリだけのhandler](../../examples/python-handler.py)をdaemonに登録する呼出方法です。

```sh
# terminal 1: 環境変数 MEDIATRIX_API_TOKEN を設定したdaemonを起動済みとする
python3 examples/python-handler.py --port 9000

# terminal 2: 同じdaemonのtokenを安全に設定済みとする
mediatrix register-service --name service:python-demo --url http://127.0.0.1:9000/rpc
mediatrix call --service service:python-demo --method add --params '[20,22]'
mediatrix call --service service:python-demo --method echo --params '{"hello":"world"}'
```

登録だけではhandlerは起動しません。登録はdaemon再起動後も残ります。上例のACLは空なのでremoteからは呼べません。

C#から同じhandlerを呼ぶこともできます。

```csharp
await client.RegisterServiceAsync("service:python-demo", "http://127.0.0.1:9000/rpc");
var response = await client.CallAsync("service:python-demo", "add", "[20,22]");
response.ThrowIfError();
Console.WriteLine(response.ResultJson); // 42
```

HTTP request/responseのschema、認可、timeoutと重複実行の扱いは[API契約](../../docs/api.md#handler-の実装)を参照してください。[2-node smoke](../../examples/smoke-two-nodes.sh)はPython handlerと実daemonの別process間通信を試験します。
