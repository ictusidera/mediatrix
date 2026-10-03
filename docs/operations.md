# 運用・設定・復旧

## 信頼境界

Mediatrix は自分が管理するノードのための transport / daemon です。peer ID は libp2p の認証済み secure connection に結び付きます。allowlist 以外とは接続せず、各 RPC/file の ACL はそれとは別に検査します。暗号化は libp2p transport の Noise/TLS に任せ、独自暗号は実装しません。

allowed_peers は、その peer が任意の論理 service 名を提供・広告することも信頼します。service ACL は inbound caller の権限であり、service 名の所有者を固定する provider pinning ではありません。侵害された許可 node が別 service を名乗ることは防げないため、互いに信頼する管理ドメインに限定してください。

local API token を持つプログラムは、登録・ACL変更・file読み出しができる operator です。同一 OS account の悪意あるプロセスや管理者を隔離しません。handler は localhost の他サービスと同様の OS 信頼境界です。handler に独立した署名/tokenを付ける仕組みはなく、他 local process が handler に直接 POST して caller_peer を偽装できるため、handler の副作用の権限判断はこの境界を理解して設計してください。

handler URL は http + numeric loopback + explicit port のみです。userinfo/query/fragment、DNS、redirect、environment proxy は禁止します。URL path に秘密を埋め込まないでください。各 API/handler の入力・結果と token は daemon log に記録しません。peer ID / request ID / latency / success のみ RPC log に記録します。

`network` は DHT の protocol namespace と key 導出に使用します。秘密鍵でも PSK でもありません。application protocol の権限は peer allowlist と ACL で決まります。同じ peer を複数 network に許可すれば、それだけで強い tenant 隔離にはなりません。

DHT provider metadata は per-service ACL と同じ秘密性を持ちません。trusted routing peer は論理鍵を推測して provider の存在を照会できます。probe/RPC/file API は非許可の場合と未存在の場合を同じ unavailable/not_found として返します。機密 service 名を隠すための private discovery はこの版では提供しません。

## 主な設定

JSON は unknown field / 余分な JSON value を拒否します。設定変更は再起動が必要です。

- data_dir: 専用の private local directory。相対パスは設定ファイル基準
- api_addr: numeric loopback IP:port。既定 127.0.0.1:47832
- listen: libp2p multiaddr 配列。既定 TCP と QUIC、port 0。安定配置は固定 port を明示
- network: 必須の管理網名。1〜64 文字の英数字・`.`・`_`・`-`
- allowed_peers: canonical peer IDs、最大128。空は local-only
- bootstrap: 到達可能な管理 node の `/p2p/<id>` 付き address。全 peer ID が allowlist 内
- dht_server: 安定した routing node で true。default false
- relays: 利用者管理の固定 relay addresses。public relay 自動選択はしない
- relay_service: 公開到達可能な管理 node で true。自分の到達可能性は operator が保証する

## リソース上限

既定 limits:

- max_file_bytes: 64 MiB
- max_store_bytes: 1 GiB（blob + upload staging の合計、registry/FS metadata は別）
- max_json_bytes: 1 MiB（local API / peer frame / handler response）
- max_concurrent: 16（node operation semaphore と API request semaphore）
- timeout_seconds: 30（最大300秒、短い caller timeout を優先）
- max_connections: 64（P2P connection limit）

API TCP接続は `2 * max_concurrent + 16` に制限し、未認証 / idle socket も含みます。HTTP header timeout は最大5秒。libp2p transient 16 connections、system memory accounting 64 MiB、application streams に上限を設定します。JSON buffer は application semaphore × max_json_bytes に比例し、libp2p memory accounting とは別です。limits を極端に増やせばメモリも増えます。

DHT provider metadata は最大1024 keys、各16 peers、各8 addresses、5分TTL。満杯時は最古 key を退避し、定期再広告で更新します。非接続 peer addresses は2048、signed records256、protocols32。DHT query/routing は allowed peers のみです。trusted peer の妨害を完全に防ぐ availability 保証はなく、上限内の負荷や record eviction は起こり得ます。監視は JSON logs と authenticated `/v1/node` / `/v1/health` が基点です。Prometheus exporter はまだありません。

## relay / NAT

TCP と QUIC の直結、管理 relay の circuit v2 を使います。static relay client では reservation と hole-punch を libp2p に任せます。relay_service は public reachability、static relay client は private reachability を明示する設定です。両役割を同一 node で混ぜないでください。

relay server の reservation は最大 allowed peer 数（上限128）、同時 circuit は max_concurrent、1 circuit の時間は timeout_seconds+10秒、各方向 bytes は max_file_bytes + 2*max_json_bytes + 1 MiB に制限します。複数RPC/転送で同じ circuit を消費すると上限で切断されることがあります。その際も送信済みRPCは再試行しません。relay node と各 endpoint は相互 allowlist が必要です。

public bootstrap / relay への暗黙接続、UPnP port mapping はしません。router/firewall は operator が必要な TCP/UDP port と経路を整えてください。NAT 越えはどの network でも成功する保証ではありません。試験は同一 Linux host 上の実 TCP / QUIC / relay protocol に限り、実 NAT や firewall traversal は未検証です。

## ファイルと永続化

`registry.json` は desired service registration / file ACL を保存します。`blobs/<sha256>` は immutable な取り込み済み bytes です。upload 後に元ファイルが変化しても配信内容は変わりません。fetch は検証後に cache へ保存され、remote への再公開は default deny。ACL の取り消しは既に取得済みの別 node のコピーを回収しません。転送再開/resume、chunk dedupe、自動GC/オンライン file delete は未実装です。容量不足はエラーになります。

data directory は専用 account のローカル disk に置き、NFS/共有ディレクトリは使わないでください。Unix は0700/0600、Windows は owner-only NTFS ACLを operator が設定します。Go の mode bits は Windows DACL の検証を保証しません。`--token-file` の Unix private permissions は検査しますが Windows ではACLの運用が必要です。

通常停止はSIGINT/SIGTERM、API request の最大10秒 grace、残ったHTTP接続は強制close、P2P/handlerをcancelして保存領域のlockを解放します。アップロード途中はcommitせずtemporary fileを片付けます。強制kill/電源断の復旧は下記を参照してください。

### Crash / backup

- `.daemon-lock/owner` はPID診断情報であり、lockを自動回収する権限ではありません
- crash後は、同じdata_dirを使うdaemonが完全に停止していると確認してから、`.daemon-lock/owner` と空 `.daemon-lock` directory だけを削除して再起動します
- 不正registry、失われたblob、symlink、破損contentは fail closed。勝手に空registryを作りません
- 起動時に有効registryを確認した後、未commit upload / registry staging と未参照blobを掃除します
- backupはdaemon停止後にdata_dir全体を取得します。identity.keyを失うとpeer IDが変わり、相手allowlist/ACLの更新が必要です
- storeを空にする/容量整理は、この版ではdaemon停止・backup取得の上で別の専用data_dirへ明示移行してください。registryを手編集してオンライン削除しないでください
- POSIXではfile flushとrename、directory flushを使用します。Windowsはportable Goでdirectory flushができないため、power-loss durabilityはfilesystemに依存します

## 既知の設計制約

request_id は相関用で、永続 dedup / exactly-once 保証はありません。timeout/cancel/切断でも既に起きた副作用は巻き戻せません。重要な更新をするhandlerは独自のidempotency保存と結果照会を実装し、outcome_unknownの場合に問い合わせてください。

サービス削除とACL変更は新規受理に適用します。既に認証して進行中のRPC/転送を遡って取り消すものではありません。local-only nodeは起動できますが、remoteアクセスは明示設定後だけです。OS service installer、auto update、証明書/鍵のrotation workflow、metrics exporter、public cloud配備はこの版の範囲外です。
