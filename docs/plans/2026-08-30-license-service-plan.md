# license-service 实施计划（gRPC-only + DB 对齐 gorm-cli 规范版）

- 日期：2026-08-30
- 状态：已批准，执行中
- 输入：`docs/design.md`（原 `servekit/specs/2026-08-26-license-service-design.md`）
- 执行方式：superpowers:executing-plans / subagent-driven-development 逐任务执行

## 与 spec 的两处经用户确认的偏差

1. **gRPC-only**：本期不在本服务内启 HTTP 网关（grpcx `registerGW=nil`，`http_addr` 默认空）。
   客户端 HTTP 面由将来独立网关提供——本期交付完整 `google.api.http` 注解（网关契约源）
   + `docs/wire-contract.md`。**不改 go-common**（无 GWMuxOptions/GWHandlerWrapper 变更）。
2. **DB 层对齐 gorm-cli-development skill**：每表显式 `ID int64` 自增代理主键 +
   `CreatedAt/UpdatedAt`；业务键降为命名复合唯一索引；struct 名带服务前缀
   （LicenseKey/LicenseEntitlement/LicenseDevice/LicenseTrial），表名
   license_keys/license_entitlements/license_devices/license_trials；**不声明 DeletedAt**
   （spec §4 硬行语义：槽位释放/试用重置/upsert 唯一性，models 包注释写明偏差）。

## Global Constraints

- module `github.com/servekit/license-service`；gRPC `:19096`；`:18086` 预留不监听（未来网关）。
- **gRPC 状态码是错误契约本体**（未来网关按 grpc→HTTP 默认映射得 400/401/403/409/429）：
  InvalidArgument / Unauthenticated / PermissionDenied / AlreadyExists / ResourceExhausted 精确分流；
  `SlotLimitInfo`/`RetryAfterInfo` 经 status details 透传。
- 凭证 payload **必须本身是 canonical JSON**（客户端逐字节互证）；**永不复用 issuedAt**
  （每次响应当前时间重签，certId 新 UUID）。
- perpetual 条目 `expiresAt` 恒 null、subscription/trial 恒非 null（双侧校验）。
- 日志脱敏：明文 key / fingerprint_id / remote_addr / payload / signature 绝不进日志
  （排障用 licenseId + device_token + certId）。
- activate/deactivate 接受 `key` 与 `licenseKey` 双字段名；409 退还限速额度（DECR）；
  `{keyId}` 接受 `lk_...` 或完整 key_hash 双形态。
- dal 规范：models/dal 文件一一对应、单文件=单表（跨表上移 service 层事务）、方法名带表名前缀、
  收 `ctx+*gorm.DB` 绝不开事务、错误直接返回（service 层转 xcodes）。
- 收尾**本地 commit 为止**：push 标注为手动步骤；每任务过 `go build ./...` + 相关测试后才
  commit；golangci-lint 严格模式。

---

## Task 1: scaffold + 仓库 bootstrap + 文档入仓 ✅

- `new-service.sh license --db --redis` → `/Users/moss/code/servekit/license-service`
- module 改名 `license-service` → `github.com/servekit/license-service`（go.mod / imports /
  buf.gen.yaml go_package_prefix / .golangci local-prefixes / Makefile VERSION_PKG）
- go-common 升至 `v0.0.0-20260803030322-51df8b4769d2`（user-service 同款，无 replace）；
  随之修 migrate_test.go 的 `dbx.SetupTestDB(t, dbx.DriverPostgres)` 新签名
- 端口：GRPCAddr 默认 `:19096`，HTTPAddr 默认 `""`（空=不启网关，:18086 预留）；
  重跑 golang-service-docker render.sh（--grpc-port 19096 --http-port 18086 --database postgres
  --redis）；compose 的 HTTP_ADDR 默认值改为空
- 配置增补：`signing:{seed,seed_secondary,key_id}`（${LICENSE_SIGNING_SEED} 等）、
  `admin_token: ${ADMIN_TOKEN}`、`trial:{days:14}`；不加 table_prefix（前缀在 struct 名）
- 验证：`make proto && make tidy && go build ./... && go vet ./...`
- `git init` + remote；`docs/design.md` + 本计划入仓；首个 commit
- 手动（用户）：gh repo create + push

## Task 2: proto 双 service（网关契约源）

`api/proto/license/v1/license.proto`，package `license.v1`：

- 枚举：`Module{MODULE_UNSPECIFIED=0, MODULE_DOWNLOADS=1, MODULE_TOOLS=2}`、
  `EntitlementKind{ENTITLEMENT_KIND_UNSPECIFIED=0, KIND_PERPETUAL=1, KIND_SUBSCRIPTION=2, KIND_TRIAL=3}`、
  `KeyStatus{KEY_STATUS_UNSPECIFIED=0, KEY_STATUS_ACTIVE=1, KEY_STATUS_REVOKED=2}`
- `LicenseService`：`Ping`(GET /ping) + `Activate`(POST /v1/activate) +
  `Deactivate`(POST /v1/deactivate) + `TrialStart`(POST /v1/trial/start) + `Health`(GET /healthz)
- 消息（protovalidate 全覆盖）：
  - `ActivateRequest{key, license_key, fingerprint_id(^v1\.[0-9a-f]{64}$), device_token(uuid),
    evict_device_token(uuid,可选)}` + CEL：key/license_key 至少一个非空
  - `ActivateResponse{payload, signature, slots: SlotSummary{used,max,devices[]}}`
  - `DeactivateRequest{key|license_key, device_token}` / `DeactivateResponse{released}`
  - `TrialStartRequest{module(defined_only,not_in:0), fingerprint_id, device_token, license_key?}` /
    `TrialStartResponse{payload, signature, already_started}`
  - `HealthResponse{status, checks: HealthChecks{db, signing}}`
  - `DeviceSlotInfo{device_token, fingerprint_id, first_seen_at, last_seen_at, name(optional 恒不设)}`
    （google.protobuf.Timestamp）
  - error details：`SlotLimitInfo{max_slots, devices[]}`、`RetryAfterInfo{seconds}`
- `LicenseAdminService` 14 RPC（spec §5.2 表）：
  CreateKey(POST /v1/admin/keys) / ShowKey(GET /v1/admin/keys/{keyId}) /
  ListKeys(GET /v1/admin/keys) / UpdateKey(PATCH /v1/admin/keys/{keyId}) /
  RevokeKey(POST .../revoke) / UnrevokeKey(POST .../unrevoke) / DeleteKey(DELETE /v1/admin/keys/{keyId}) /
  GrantModule(PUT /v1/admin/keys/{keyId}/grants/{module}) /
  RevokeModule(DELETE /v1/admin/keys/{keyId}/grants/{module}) /
  ListKeyDevices(GET /v1/admin/keys/{keyId}/devices) /
  KickDevice(DELETE /v1/admin/keys/{keyId}/devices/{deviceToken}) /
  ShowTrial(GET /v1/admin/trials/{fingerprintId}) /
  ResetTrial(POST /v1/admin/trials/{fingerprintId}/{module}/reset) /
  ShowPubKey(GET /v1/admin/signing/pubkey)
  - `GrantModuleRequest`：`duration_days`/`expires_at` 二选一 CEL 互斥；perpetual 禁带过期
  - `ResetTrialRequest.reason` 必填
- 验证：`make proto && go build ./...`；gen/ 与 api/swagger/ 生成物齐全；commit

## Task 3: xcodes + 错误拦截器 + admin 鉴权 + server 接线

- `pkg/xcodes/license.go` 六码：
  `BAD_KEY_FORMAT`(BadRequest,400) / `KEY_NOT_FOUND`(Unauthorized,401) / `KEY_REVOKED`(Forbidden,403) /
  `SLOT_LIMIT`(Conflict,409) / `ALREADY_ENTITLED`(BadRequest,400) / `RATE_LIMITED`(TooManyRequests,429)
- `pkg/xcodes/details.go`：`Detailed{Err *xerr.Error; Details []proto.Message}` + `WithDetails(...)`
- `pkg/interceptor/error.go`（测试先行 error_test.go）：xerr category→gRPC code；
  message 保留 `"REASON: message"`；`Detailed` → `status.New(...).WithDetails(...)`（失败降级）
- `pkg/interceptor/admin.go`（测试先行 admin_test.go）：`AdminAuthInterceptor(token)`——
  FullMethod 前缀 `/license.v1.LicenseAdminService/` 才校验；`grpcx.BearerTokenFromCtx` +
  `subtle.ConstantTimeCompare`；token 空 → 全拒
- `pkg/server.go`：`grpcx.New(cfg, registerGRPC, nil, LicenseError, protovalidate, AdminAuth)`
- 验证：`go test ./pkg/... && go build ./...`；commit

## Task 4: cert 子包（canonical JSON + Ed25519 + golden）

`internal/service/cert/`：

- golden_test.go 三向量（spec §12.1）：
  seed `9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60` /
  pubkey `d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a` /
  expectedCanonicalPayload / expectedSignatureB64
  `VBDWouHSgIv5DbSYiQC880OpDk9gjuZMlpcIcLVDAq0G0Vz7iOexr2hApkTL2aiTEqzcjjtZacrw82hT0IaoDQ==`
  + keyless 辅助向量（licenseId:null + 账本全量形状）+ 单模块全非 null 最小向量
- canonical.go：手写确定性序列化器（无反射）：成员按键名 UTF-8 字节序递归排序；
  转义仅 `"` `\` `\b\f\n\r\t` 与 <0x20（`\u00xx` 小写 hex）；其余原样；紧凑单行；
  属性测试：序列化→解析→重序列化逐字节稳定
- cert.go：`Payload{V; CertID; LicenseID *string; DeviceToken, FingerprintID;
  IssuedAt time.Time; SigningKeyID *string; Entitlements map[string]Entitlement}`；
  issuedAt RFC3339 UTC 秒精度；`Signer.Sign(p) (payload, sigB64 string, err)`；`Verify`
- keys.go：seed 加载（64 hex）、secondary+keyID 命名钥
- 验证：`go test ./internal/service/cert/ -run Golden -v`；commit

## Task 5: store 层（4 表 models + dal，gorm-cli 规范）

models（ID 自增代理主键 + 命名复合唯一索引 + CreatedAt/UpdatedAt；无 DeletedAt）：

```go
type LicenseKey struct {
	KeyHash   string `gorm:"column:key_hash;size:64;not null;uniqueIndex:uq_license_keys_key_hash"`
	LicenseID string `gorm:"column:license_id;size:35;not null;uniqueIndex"`
	KeyPrefix string `gorm:"column:key_prefix;size:8;not null"`
	Label     string `gorm:"size:200"`
	MaxSlots  int32  `gorm:"not null;default:3"`
	Status    int32  `gorm:"not null;default:1;index"` // licensev1.KeyStatus
	RevokedAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

type LicenseEntitlement struct {
	KeyHash   string `gorm:"column:key_hash;size:64;not null;uniqueIndex:uq_license_entitlements_key_module,priority:1"`
	Module    int32  `gorm:"not null;uniqueIndex:uq_license_entitlements_key_module,priority:2"` // licensev1.Module
	Kind      int32  `gorm:"not null"`                                                          // licensev1.EntitlementKind
	ExpiresAt *time.Time // perpetual 恒 NULL；subscription/trial 恒非 NULL
	GrantedAt time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

type LicenseDevice struct {
	KeyHash           string `gorm:"column:key_hash;size:64;not null;uniqueIndex:uq_license_devices_key_token,priority:1"`
	DeviceToken       string `gorm:"column:device_token;size:36;not null;uniqueIndex:uq_license_devices_key_token,priority:2"`
	FingerprintID     string `gorm:"column:fingerprint_id;size:67;not null"`
	LastSeenAt        time.Time
	LastFingerprintAt *time.Time
	CreatedAt time.Time // 即 first_seen_at（proto firstSeenAt 由此映射）
	UpdatedAt time.Time
}

type LicenseTrial struct {
	FingerprintID    string `gorm:"column:fingerprint_id;size:67;not null;uniqueIndex:uq_license_trials_fp_module,priority:1"`
	Module           int32  `gorm:"not null;uniqueIndex:uq_license_trials_fp_module,priority:2"` // licensev1.Module
	StartedAt        time.Time
	FirstDeviceToken string `gorm:"column:first_device_token;size:36;not null"`
	CreatedAt time.Time
	UpdatedAt time.Time
}
```

dal（错误直接返回；generated 辅助器）：

- `dal/key.go`：CreateKey / GetKeyByKeyHash / GetKeyForUpdate（clause.Locking UPDATE）/
  GetKeyByIDOrHash（lk_ 或 64hex）/ ListKeys(status,limit) / UpdateKeyLabel / UpdateKeySlots /
  SetKeyRevoked / DeleteKey
- `dal/entitlement.go`：ListEntitlementsByKeyHash / UpsertEntitlement（OnConflict 指定冲突列）/
  DeleteEntitlement / DeleteEntitlementsByKeyHash
- `dal/device.go`：GetDevice / CountDevicesByKeyHash / ListDevicesByKeyHash / TouchDevice /
  InsertDevice / DeleteDevice / DeleteDevicesByKeyHash
- `dal/trial.go`：GetTrial / InsertTrialOnConflictDoNothing / ListTrialsByFingerprint / DeleteTrial
- **跨表级联与 activate 槽位事务都在 service 层** `db.Transaction` 组合多个 dal 调用
- 测试（SetupTestDB Postgres）：FOR UPDATE、ON CONFLICT 幂等、唯一索引防双插
- 验证：`make generate && git diff --exit-code` + `go test ./internal/store/...`；commit

## Task 6: activation 域（activate/deactivate/trial + 限速）

- key.go：NormalizeKey（trim→大写→剔非 [A-Za-z0-9]→`^AV1D[0-9A-Z]{20}$`）、KeyHash（SHA-256
  小写 hex）、LicenseID（`lk_`+前 32）、GenerateKey（`AV1D-`+4×5 Crockford base32）
- ratelimit.go：`INCR license:rate:{id}:{device_token}` 首次 `EXPIRE 60`，>1 → 429；
  `refund func()`（DECR）409 时调用
- activation.go（测试先行 A1–A12 表驱动 + 边界）：
  - Activate：归一化→hash→限速→查 key（401/403）→ 事务{ GetKeyForUpdate → evict 删除 →
    行存在 TouchDevice 复用/重绑 / 新 token Count≥MaxSlots→409(WithDetails SlotLimitInfo) 否则
    InsertDevice } → entitlements ∪ trials（同模块 key 优先、试用必须并入、A8 过期照常）→
    Sign（certId 新 UUID、issuedAt=now）→ 200；409 路径 refund()
  - Deactivate：DeleteDevice → released，恒 200（key 查无也 released:false）
  - TrialStart：GetTrial 存在→alreadyStarted:true；否则 InsertOnConflictDoNothing 冲突重读；
    带 key 复用 activate 的校验+槽位纪律；已有效权益→400 ALREADY_ENTITLED
  - 并发：N≥8 同 (fingerprint,module) 恰 1 行；抢最后 1 槽恰一成功；限速 429/409 退还
- facade + handler 委托；`go test ./... && go build ./...`；commit

## Task 7: admin 域（14 RPC + 审计）

- id.go：resolveKeyID（lk_ 前缀按 licenseId 查，否则 64hex 按 key_hash 查）
- admin.go（测试先行）：全 14 RPC；CreateKey 明文 key 仅响应一次（唯一冲突重试 3 次）；
  grant 顺延 `max(now, expires_at)` 或绝对覆盖；perpetual 带过期→400；revoke→403→unrevoke 恢复；
  delete 事务级联（service 层组合三 dal）；kick；reset（reason 必填）；pubkey base64
- 审计日志：`slog.Info("admin_audit", "op", ..., "target", licenseId|device_token|sha256(fp)[0:16],
  "reason", ...)`——fingerprint_id 本身不进日志
- `go test ./...`；commit

## Task 8: Health + gRPC 集成测试

- internal/service/health：`SELECT 1` + signer 就绪 → `{status, checks}`；失败→ServiceUnavailable
- internal/integration/license_test.go：NewModule/NewServer + Postgres testcontainer + miniredis +
  真实 gRPC client；断言：六种错误→gRPC code 精确；status.Details() 含 SlotLimitInfo 与
  RetryAfterInfo；payload canonical（重算比对）；key/licenseKey 双名；限速第二次 ResourceExhausted
- commit

## Task 9: 网关契约文档 + docker + 验收清单

- docs/wire-contract.md：grpc→HTTP 状态映射表；错误体 `{"error":{code,message,maxSlots,devices}}`
  展平规则；Retry-After；X-Request-Id；4KiB→413；不开 CORS；key/licenseKey 双名；15s 超时不重试；
  只放行四个客户端路径
- make docker-build / docker-up / docker-migrate / docker-health
- deploy/Caddyfile.example（Caddy → 未来独立网关 → license-service gRPC :19096）；
  README（部署/pg_dump 备份 14 份/密钥泄露 runbook）；CLAUDE.md（本服务铁律）
- 验收清单：build / lint / make proto && git diff --exit-code / make generate && git diff --exit-code /
  grpcurl 全 RPC / swagger 齐全 / NewModule 测试 / facade 一一对应 / 无 demo 残留
- `make all` 全绿 → 最终 commit。手动（用户）：push
