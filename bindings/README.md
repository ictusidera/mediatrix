# 言語別バインディング

各アプリは **同じマシンで既に起動した `mediatrixd`** の認証付き HTTP/JSON API を呼びます。daemon が P2P接続・探索・RPC転送・content store を担当し、アプリに相手のIPを渡す必要はありません。Go DLLを直接ロードする FFI ではありません。

- [C# / .NET Framework 4.6.2](csharp/README.md): `Mediatrix.Client.dll`、非同期クライアント、実行可能なC# console/handler sample、ビルド・テスト手順
- [Python](python/README.md): 既存の標準ライブラリHTTP handlerの起動・登録・呼出例。PythonクライアントSDKは未実装
- その他の言語: [HTTP APIとhandler契約](../docs/api.md)に従って実装可能。対応済みSDKを意味しません

API tokenはnode全体のoperator権限です。numeric loopback HTTP、明示port、Bearer headerで使い、URL・ソース・ログに埋め込まないでください。remote通信にはdaemon設定のpeer allowlistとservice/fileのACLが両方必要です。

ファイルの「upload」は、そのdaemonのlocal content storeへの取り込みです。中央FTP/HTTPサーバーへの一括送信ではありません。`fetch`が必要な複製を許可されたpeerから取得し、`download`がそのlocal storeからアプリへ取り出します。既存の[運用・セキュリティ上の制限](../docs/operations.md)はバインディングを使っても変わりません。
