# Mediatrix

利用者が管理する Windows / Linux ノード間で、論理サービス名による RPC と SHA256 ファイル転送を提供する常駐 daemon です。各言語のアプリは認証付き loopback HTTP/JSON API を使います。ネットワークは Go/libp2p の TCP、QUIC、明示的な circuit relay v2 と専用 Kad-DHT です。

この版は `main` からの再設計です。旧 `master` の Go-only API や command handler との互換性はありません。履歴は残しています。

## ビルドと試験

Go 1.26 系の最新セキュリティ修正版を使用してください。検証に使用した版は 1.26.7 です。

```sh
go build -o mediatrixd ./cmd/mediatrixd
go build -o mediatrix ./cmd/mediatrix
go test -race ./...
go vet ./...
bash examples/smoke-two-nodes.sh
```

Windows では `.exe` を付けてビルドできます。CI は Linux / Windows の test・vet・build と Linux race / 2-node smoke を定義しています。CI 設定を置いただけでは実行済みになりません。手元の実行結果は [検証記録](docs/verification.md) を参照してください。

## ローカルで試す

`examples/local.json` をコピーし、専用の `data_dir` を指定してください。相対パスは設定ファイルのディレクトリ基準です。初期状態の `allowed_peers: []` では remote 通信を拒否します。

API token は 32 文字以上のランダム値を自分で生成し、環境変数または private な token file で渡します。コード例に固定 token は埋め込みません。

```sh
export MEDIATRIX_API_TOKEN="$(python3 -c 'import secrets; print(secrets.token_urlsafe(32))')"
./mediatrixd --config examples/local.json
# 別 terminal には同じ環境変数を安全に設定してから実行
./mediatrix node
```

Windows PowerShell では .NET の cryptographic RNG などで生成した token を `$env:MEDIATRIX_API_TOKEN` に設定し、`mediatrixd.exe --config ...` を実行します。token や identity key をソース管理・チャット・ログに貼らないでください。

別 terminal で Python handler を起動します。表示された numeric loopback URL を登録します。

```sh
python3 examples/python-handler.py
./mediatrix register-service --name service:demo --url http://127.0.0.1:表示されたポート/rpc
./mediatrix call --service service:demo --method add --params '[20,22]'
```

登録は再起動後も残りますが、handler アプリ自身の起動はアプリ側の責任です。固定ポートで登録する場合は Python example の `--port` を使えます。

## 2 台以上で使う

1. 各 daemon を empty allowlist で一度起動し、`mediatrix node` の `peer_id` と到達可能な `addrs` を交換する
2. 各設定の `allowed_peers` に信頼する相手の canonical peer ID を入れ、`bootstrap` に管理ノードの到達可能な `/.../p2p/<peer-id>` address を指定して再起動する
3. service 登録時の `--allow-peer`、ファイルアップロード後の `grant-file --allow-peer` で相手の権限を明示する
4. 呼び出し側アプリは相手の IP を扱わず、daemon に `service:<name>` または `file:sha256:<digest>` を渡す

構成作成から実通信まで自動で試す `examples/smoke-two-nodes.sh` は、temporary directory と loopback ノードだけを使い、終了時に片付けます。

## 公開契約の要点

- RPC handler は登録済み numeric loopback HTTP endpoint。shell / executable を remote から起動しない
- Peer ID allowlist + 各 service / file の ACL。空 ACL は remote deny。local API token は node 全体の operator 権限
- API は browser Origin、DNS hostname、redirect、proxy を受け付けない
- RPC 送信開始後の自動再試行はしない。応答喪失は `outcome_unknown: true`。request ID は永続重複排除ではない
- ファイルは upload により専用 content store に取り込み、SHA256 検証後にのみ提供。任意 remote path / TCP / UDP tunnel API はない
- サービス登録、file ACL、identity は再起動で復元。DHT provider 情報は volatile な探索候補

## 言語別バインディング

[C# / .NET Framework 4.6.2用DLLとサンプル](bindings/csharp/README.md)を `bindings/csharp/` に用意しています。導入方法・非同期呼出し・ファイル転送・エラー/cancelの扱いは同README、言語別の入口は [bindings/README.md](bindings/README.md) を参照してください。Pythonは既存HTTP handlerサンプルの起動・登録方法を [bindings/python/README.md](bindings/python/README.md) にまとめています。

## 文書

- [API と CLI、異言語 handler 契約](docs/api.md)
- [設計・受入条件](docs/architecture.md)
- [設定・relay・復旧・セキュリティ・既知制限](docs/operations.md)
- [実行済み検証と未検証範囲](docs/verification.md)

実インターネットの NAT / firewall / 複数地域配置、Windows runtime、長期負荷運用はこの環境では未検証です。macOS/iOS は今後の候補であり、iOS の常時バックグラウンド server を保証しません。
