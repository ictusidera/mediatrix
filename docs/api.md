# ローカル API / operator CLI

API は **既に起動した daemon の操作用**です。CLI は P2P node を起動しません。
HTTP の送信先は numeric loopback IP と明示 port のみです。
既定値は `http://127.0.0.1:47832`。DNS 名、public IP、proxy、redirect は利用しません。
全 endpoint は `Authorization: Bearer <token>` が必須です。`Origin` header を持つ
browser request は拒否します。token は query、JSON、command-line 引数へ入れません。

## 起動と認証

```sh
# 例: 十分な長さのランダム token を shell 環境へ保存
export MEDIATRIX_API_TOKEN="$(python3 -c 'import secrets; print(secrets.token_urlsafe(32))')"
mediatrixd --config examples/local.json
# 別 terminal では同じ token を安全に設定してから実行
mediatrix node
```

`MEDIATRIX_API_TOKEN` の代わりに `--token-file /path/to/token` を指定できます。
明示した token file が環境変数より優先されます。token は printable ASCII 32–4096文字、
空白なしです。file の末尾改行は許可します。Unix では regular file、owner-only permission
（通常 `chmod 600`）、symlink なしが必須です。Windows では operator が NTFS ACL を
owner-only に制限してください。Go の file mode は Windows DACL を検証できないため、
この実装ではその ACL を自動検証しません。

`examples/local.json` は allowlist が空の local-only 設定です。`mediatrix node` の
`peer_id` を operator 間で交換し、`allowed_peers` と、必要に応じて `bootstrap` / `relays`
を設定して再起動します。Peer ID は `node` が出力する canonical form を使用します。
`network` は全参加 node で一致させます。`data_dir` の相対 path は config file 所在地が基準です。

## CLI

global flags は command より前です。

```sh
mediatrix --api http://127.0.0.1:47832 --token-file ./api.token node
mediatrix services
mediatrix register-service --name service:demo --url http://127.0.0.1:9000/rpc --allow-peer PEER_ID
mediatrix remove-service --name service:demo
mediatrix call --service service:demo --method echo --params '{"hello":"world"}' --request-id operation-123
mediatrix upload --file ./input.bin
mediatrix grant-file --key file:sha256:HEX_DIGEST --allow-peer PEER_ID
mediatrix fetch --key file:sha256:HEX_DIGEST --out ./new-output.bin
```

`--allow-peer` は複数指定できます。省略した登録/ACL 更新は empty ACL になります。
`grant-file --key KEY` のみで remote 読取許可を撤回できます。file のローカル内容は削除しません。
service/file ACL の peer は config の `allowed_peers` に含まれる必要があります。
取得済みの複製を ACL 変更で相手から回収することはできません。

`fetch` はまず daemon に取得を依頼し、その後 local API から内容を保存します。
サイズと SHA-256 が一致した一時 file だけを、同じ directory の hardlink により atomically
公開します。既存 output や検証中に作られた output を上書きしません。
hardlink に対応しない filesystem では安全に失敗します。CLI の file 上限は1 GiB、JSON 応答上限は16 MiBです。
正常時は JSON を stdout、失敗時は説明を stderr に出し exit code 1 を返します。
更新/削除の HTTP 204 は stdout なしで成功します。RPC が application error を返した場合は
応答 JSON を出したうえで exit code 1 になります。

## HTTP schemas

request JSON は UTF-8、`Content-Type: application/json` を使用します。
unknown fields、連結した複数 JSON 値、上限超過、圧縮 request body は拒否します。
service 名は `service:` から始まる1–128文字の名前です。使用可能文字は英数字、`.`、`_`、`/`、`-`、
先頭は英数字です。file key は `file:sha256:` + lowercase hex 64桁です。

### 状態

`GET /v1/health` → 200 `{"status":"ok"}`

`GET /v1/node` → 200:

```json
{"version":"0.2.0-dev","peer_id":"PEER_ID","addrs":["/ip4/127.0.0.1/tcp/1234/p2p/PEER_ID"],"connected_peers":0,"services":0,"files":0,"network":"my-private-network"}
```

`GET /v1/services` → 200: 下記 Service object の配列。

### Service 登録・削除

`PUT /v1/services` → 204:

```json
{"name":"service:demo","url":"http://127.0.0.1:9000/rpc","allowed_peers":["PEER_ID"]}
```

同名の登録を更新します。URL は numeric loopback HTTP + explicit port のみです。
userinfo、query、fragment は使用できません。API client が remote request ごとに
handler URL を指定する設計ではありません。

`DELETE /v1/services?name=service%3Ademo` → 204。

### RPC

`POST /v1/call` → 200:

```json
{"service":"service:demo","method":"echo","params":{"hello":"world"},"request_id":"operation-123","timeout_ms":10000}
```

`params` は任意の JSON 値です。`request_id` と `timeout_ms` は省略できます。
request ID を省略すると daemon が生成します。正常応答:

```json
{"request_id":"operation-123","result":{"hello":"world"}}
```

application error 応答:

```json
{"request_id":"operation-123","error":{"code":"invalid","message":"invalid operation"}}
```

`result` と `error` はどちらか一方です。handler の application error と API/transport error
は同じものではありません。後者は非2xxと下記 error envelope になります。

### File upload、ACL、fetch、download

`POST /v1/files` + `Content-Type: application/octet-stream` + raw bytes → 201:

```json
{"key":"file:sha256:HEX_DIGEST","size":123,"allowed_peers":[]}
```

upload は既定で非公開です。同じ内容は同じ key になります。file は operator-managed store に
保持され、node 再起動後も残ります。

`PUT /v1/files/access` → 204:

```json
{"key":"file:sha256:HEX_DIGEST","allowed_peers":["PEER_ID"]}
```

`POST /v1/fetch` → 200 FileInfo object（upload 応答と同じ schema）:

```json
{"key":"file:sha256:HEX_DIGEST","timeout_ms":10000}
```

ローカルに検証済み内容があれば再利用し、なければ許可された peer から取得します。
取得内容は key の SHA-256 と一致した場合のみ保存します。受信した複製の ACL は既定で空です。

`GET /v1/files/HEX_DIGEST` → 200 `application/octet-stream`、raw bytes。
この path に `file:sha256:` prefix は付けません。これは local store の取得 endpoint です。
remote fetch を暗黙には開始しません。

### API error

```json
{"error":{"code":"timeout","message":"operation deadline exceeded","outcome_unknown":true}}
```

主な status: 400 invalid、401 unauthorized、403 forbidden、404 not_found、
409 conflict、413 too_large、429 busy、504 timeout。それ以外の backend/transport failure は
502になる場合があります。`outcome_unknown` は必要時のみ現れます。

## Timeout・cancel・再試行

- daemon の既定 timeout は30秒、config で1–300秒。API request 全体と peer/handler 処理を制限します
- `timeout_ms: 0` / 省略は daemon 設定値。正値はさらに短い operation deadline を指定します。daemon 上限を延長しません
- CLI の `--timeout` は1–310秒、既定45秒。全 global flags と同じく command の前へ置きます。fetch の取得要求と content download は別 HTTP request です
- 例: `mediatrix --timeout 125s call --service service:demo --method work --params '{}' --timeout-ms 120000`。daemon 側 timeout も必要な長さに設定してください
- client 切断、deadline、peer stream reset は処理 cancel を伝播させます。ただし handler がすでに外部へ与えた副作用は取り消せません
- **request_id は相関 ID です。daemon は deduplication、exactly-once、永続 replay cache を提供しません。同じ ID で再送すると再実行されます**
- 送信開始後の RPC failure は provider を切り替えて自動再試行しません。`outcome_unknown: true`、または CLI の「outcome unknown」は成功/失敗が未確定であることを示します
- mutation の安全な再試行が必要なら、handler 側で request ID と request 内容を検証する永続的な重複排除・結果保存を実装してください

## Handler の実装

daemon は登録済み loopback URL に POST します。request schema:

```json
{"service":"service:demo","method":"echo","params":{"hello":"world"},"request_id":"operation-123","timeout_ms":10000,"caller_peer":"AUTHENTICATED_PEER_ID"}
```

`caller_peer` は認証済み peer identity から daemon が設定します。handler は HTTP 200 と
`{"result":ANY_JSON}` または `{"error":{"code":"APP_CODE","message":"MESSAGE"}}` を返します。
request ID を handler 応答へ重複して追加しないでください。これは JSON-RPC 2.0 envelope ではありません。
localhost 上の handler にも他の local process は接続できるため、この field は単独の
localhost 認証機構にはなりません。local machine/operator を信頼境界とします。

`python3 examples/python-handler.py --port 9000` は標準 library のみで動く参考実装です。
`echo` と `add` を提供します。実 production handler の認可、rate limiting、idempotency、
長時間処理や外部 side effect の管理はアプリケーション側で実装してください。

`examples/smoke-two-nodes.sh` は一時 directory と loopback の2 node を使い、peer ID交換、
再起動、remote RPC、ACL付きfile転送、service削除を実行します。public bootstrap は使わず、
終了時に子processと一時dataを削除します。Bash / Python 3 / Go が必要です。
既存 binary を使う場合は `MEDIATRIX_BIN` / `MEDIATRIXD_BIN` に absolute path を設定します。
