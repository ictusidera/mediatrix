# Mediatrix 再構築の設計と受入条件

## 目的と境界

Windows / Linux 上の各言語のアプリが、ローカル常駐 daemon の HTTP/JSON API を介して `service:<name>` の RPC と `file:sha256:<digest>` のファイル交換を利用する。Go 埋め込み専用 API、任意 TCP/UDP トンネル、任意コマンド実行は提供しない。macOS は将来候補。iOS の常時バックグラウンド server は約束しない。

## 構成

- `cmd/mediatrixd`: identity、libp2p host、DHT、永続登録、ローカル API の所有者
- `cmd/mediatrix`: daemon HTTP API だけを使う operator CLI
- `internal/control`: token 認証済み numeric loopback HTTP API、JSON/接続/時間制限
- `internal/node`: trusted peer discovery、ACL、RPC、ファイル転送、handler 呼び出し
- `internal/store`: 排他的 data directory、JSON 登録、SHA256 content store
- `internal/config` / `internal/model`: 検証済み設定と versioned 公開 JSON 契約

## 信頼と handler

Peer ID の明示 allowlist を接続 gate で適用し、さらに各 service/file の allowed_peers を適用する。空の ACL は remote deny。自分自身のローカル API は operator 権限を持つため local call / file 読み出しを許可する。公開 bootstrap は使わず、bootstrap と relay も allowlist 内の利用者管理ノードに限定する。DHT namespace は分離用であり秘密鍵の代替ではない。

Handler は operator が登録した numeric loopback の HTTP URL のみ。DNS 名・userinfo・query・fragment を拒否し、proxy と redirect は利用しない。呼び出し側が URL・command を指定する仕組みはない。認証済み caller peer / request ID / method / JSON params を渡す。応答は JSON result または error。ローカル token を持つ process は信頼対象であり、任意の hostile local process を隔離する sandbox ではない。

## 発見とネットワーク

TCP + QUIC と circuit relay v2 を libp2p が扱う。operator 指定 peer 以外へは接続しない。DHT provider records は探索候補であり権限証明ではない。小規模管理網では configured peers への認証済み exact-key probe でもサービスを探す。ACL 非許可時の application API/probe 応答は存在有無を開示しない。DHT は別で、鍵名を推測できる trusted routing peer は provider metadata を参照できる。自動 public relay / UPnP port mapping は利用しない。固定 relay は明示設定だけで有効化し、実 NAT・firewall・多地域運用は別の配置試験が必要。

## RPC の実行契約

1 stream = 1 bounded JSON request / 1 bounded JSON response。timeout は caller と daemon の上限の小さい方を使う。HTTP request cancel は stream reset に伝播し、remote 側は handler HTTP request を cancel する。

request の書き込み開始前の接続失敗は別 provider を探せる。書き込み開始後は daemon が自動再試行しない。応答が失われた場合は outcome_unknown=true。request_id は相関用であり durable deduplication ではない。外部副作用の exactly-once は保証しない。再送を必要とする service は request_id を使った独自の永続的 idempotency を実装する。

## 永続化とファイル

サービス登録と file ACL は再起動で復元する。handler プロセスそのものの起動・復旧は担当しない。ファイルは caller がアップロードした bytes を daemon 専用ストアへ取り込み、remote 公開は別操作で明示 ACL 設定する。任意パスを remote から読む API はない。アップロード/受信/送信はサイズ・容量・concurrency・deadline を制限し、SHA256 と advertised size を検証する。未検証 bytes を API caller へ返さない。転送失敗の一時ファイルは片付ける。

registry は atomic replace、blob は commit-before-registry とし、不正 registry は fail closed。専用 data directory を複数 daemon が共有しない。OS ごとの ACL/rename semantics と disk-full/power-loss の限界は運用文書に明記する。

## 受入条件

- 異言語 HTTP handler と別 daemon を使う logical service call、SHA256 file fetch が成功
- 未認証 local API、Origin 付き browser request、非 loopback 設定、不許可 peer/service/file が拒否される
- malformed/oversize JSON、過大ファイル、容量上限、concurrency 上限を fail closed
- timeout と cancellation が handler まで伝播し、不明 outcome に自動再送が起きない
- 再起動後に identity、service、file ACL と content が復元される
- 破損 content、切断、不明 key、offline provider が安全に失敗する
- gofmt / go vet / unit / integration / race を Linux で実行
- Windows amd64 の cross compile を実行（Windows runtime 試験とは区別する）
- README、API 契約、2-node setup、運用手順、検証結果、既知制限を日本語で残す
