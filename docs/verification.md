# 検証記録

実施日: 2026-10-03 UTC。Linux amd64、Go 1.26.7。ソースは main `d1ecaf2ec792e1f0e8a54ac0807aae6000b5b5ec` を起点とする独立した `rebuild/mediatrix` branch。

## 最終依存構成で合格

- `gofmt -l .` が空
- `go vet ./...`
- `go test -race -count=3 ./...`（全 package）
- `go test -cover ./...`
- `go test -run '^$' -fuzz=FuzzReadFrame -fuzztime=5s -parallel=2 ./internal/node`（31,530 executions、panicなし）
- `bash examples/smoke-two-nodes.sh`
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...`
- Windows amd64 用全 test package の `go test -c`（実行していない）

依存更新前の合格だけで止めず、libp2p v0.50.0 / Kad-DHT v0.39.2 / dtls v3.1.10 / webtransport v0.13.0 / quic-go v0.62.0 の最終構成で上記を再実行しています。Kad-DHT v0.42 系は custom ProviderStore の injection API がなくなったため、この版の明示的な bounded ProviderStore と互換な v0.39.2 に固定しています。

## 実際に確認した経路

- TCP の別 node RPC、認証済み caller peer が Python/Go HTTP handler に伝わること
- QUIC のみ listener を使う2ノード RPC
- 第三の管理 DHT router 経由で直接未登録 provider を発見しRPC
- direct listener を持たない node への管理 circuit relay v2 経由 RPC と128KB超ファイル転送（240KB）。relay経路をassert
- 独立した2つの daemon process＋Python標準library handler＋CLIでRPC結果42、ACL付きfile upload/fetch、削除後のservice拒否
- identity / service登録 / file metadata / ACL の再起動復元
- 未許可peer、空service/fileACL、認証失敗、browser Origin、非numeric-loopback Host/URL、redirect、unknown JSON fields、過大JSON/bodyの拒否
- RPC cancel が遠端 handler の request context まで到達すること
- timeout / concurrency飽和 / offline・不明providerの失敗
- 応答喪失時に2番目のproviderへ再送しないこと、CLIの壊れた成功応答も outcome unknown として扱うこと
- local/remote破損fileを公開・commitしないこと、失敗transferのstaging cleanup
- store quota、duplicate upload、atomic registry、malformed registry fail closed、symlink、排他lock、crash orphan cleanup
- slow upload がservice registry照会を塞がないこと、待機uploadをcancelできること
- 未認証/idle API TCP接続の上限、実socketのstalled upload timeout / close / shutdown grace終了による解放

## 脆弱性検査: 未解決項目あり

`go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` を実行しました。初期検査で発見した修正版のある DTLS / WebTransport / QUIC 依存の3件は更新後に消えました。

最終検査は成功ではありません。次の1件が引き続き報告され、govulncheck は exit status 3 です。

- [GO-2024-3218](https://pkg.go.dev/vuln/GO-2024-3218): Kad-DHT による content censorship / 探索妨害。Go vulnerability database は全 version affected、既知の修正版なしと記載。`github.com/libp2p/go-libp2p-kad-dht@v0.39.2` も報告対象
- この構成は static authenticated peer allowlist、DHT query/routing filter、公開bootstrap禁止で、攻撃者が任意の新しいidentityを大量参加させる経路を制限する。configured/connected peerへの直接probeも探索候補を増やす
- これらは根本修正ではない。侵害された許可peer/routerの情報隠蔽、provider偽装、探索妨害は残る。信頼する自分の管理ノードに限定し、公開参加型networkとして配備しない
- 最終scanには「呼出しは確認されない required module 内の4件」もあり、0件の保証はしない

一次情報: [Go vulnerability report](https://pkg.go.dev/vuln/GO-2024-3218)、[GitHub advisory](https://github.com/advisories/GHSA-mqr9-hjr8-2m9w)、[研究論文](https://arxiv.org/abs/2307.12212)。依存更新時に再検査してください。

## 未検証・保証しないこと

- Windows runtime 実行、NTFS ACL・電源断復旧・service起動。cross compile はこれらの代用ではない
- 実際のLAN/WAN、NAT・firewall、multi-region latency、hole punching成功率、長時間relay予約更新
- 長期負荷、最大容量/最大peer数でのsoak test、突然の電源断を伴う耐久性
- macOS/iOS実行、iOS常時background server
- GitHub Actionsは設定ファイルを追加しただけで、remote CIはまだ実行していない

検証環境ではlibp2pがlocal interface列挙時に `netlinkrib: operation not permitted` を出しました。明示的なloopback TCP/QUIC/relay試験は通りましたが、wildcard address自動検出・外部到達性を確認したことにはなりません。
