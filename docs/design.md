# license-service 技术方案（servekit 栈版）

- 日期：2026-08-26
- 状态：已评审（brainstorming 定稿），待实施
- 修订（2026-09-05）：servekit 后端服务统一 gRPC-only 化。本方案中"全 RPC 带
  `google.api.http` 注解 / swagger 自动生成"的表述已过时——注解与 swagger 产物
  已移除，HTTP 面的路由契约改由 `docs/wire-contract.md` §0 的路由表承载，由
  网关实现。本文其余部分仍有效
- 读者：服务端实现者、aividlab Rust 客户端 license 模块维护者、运维
- 仓库：`git@github.com:servekit/license-service.git`（module `github.com/servekit/license-service`）
- 关联文档：
  - `aividlab/docs/plans/license-server-golang.md`（原方案，本文取代之；其 `licenseKey` 字段拼写与客户端代码不一致，以本文 §5.1 为准）
  - `aividlab/docs/plans/license-app-integration.md`（客户端方案，含 16 条偏差定稿）
  - `aividlab/docs/ai/fingerprint.md`（设备指纹契约：`machine_uid` / `fingerprint_id` / `device_token`）
- 本文档只定义架构、协议与逻辑，**不含 Go 实现代码**；proto 定义、表形状、HTTP wire 形状、golden 向量是协议级产物，属于文档正式内容。

## 术语速查

| 术语 | 含义 |
|---|---|
| license-service | 本方案定义的激活服务，servekit 栈 Go 微服务（gRPC + grpc-gateway） |
| license key | 用户持有的明文密钥，形如 `AV1D-XXXXX-XXXXX-XXXXX-XXXXX`，仅签发时展示一次 |
| `key_hash` | SHA-256(key 归一化明文) 的 64 位小写 hex，库中唯一标识一把 key |
| `licenseId` | `"lk_" + key_hash 前 32 个 hex 字符`，凭证与 UI 中的 key 身份；凭证中绝不出现明文 key |
| 凭证（cert） | 服务端 Ed25519 签名的授权声明，v1 详见 §8 |
| entitlement | 凭证中按模块一条的权益条目，kind ∈ {perpetual, subscription, trial} |
| 槽位（slot） | 一把 key 允许**同时**在线的设备数（默认 3），约束"同时几台"而非"总共几次" |
| 售卖模块 | `downloads`、`tools`；`player`/`storage`/`sound`/`environment`/`settings` 永远免费，不进授权体系 |
| 三模式 | servekit 栈的服务运行形态：standalone gRPC / HTTP gateway / in-process module |

## 修订记录（as-built，2026-08-31）

实施与评审后有以下修订，正文相应位置已标注【修订】；本表为权威差异源：

| # | 原文 | 修订后 | 理由 |
|---|---|---|---|
| 1 | §7.5 限速：redisx 直用 INCR/EXPIRE/DECR，每 60s 1 次，409 退还额度 | go-common `ratelimit` 固定窗口配额：每 (身份, device_token) 每窗口 `max` 次（默认 10/60s，`rate_limit.{key_prefix,window,max}` 可配）；409 消耗配额（配额已覆盖 evict 重试） | 限流目的是防滥用而非精确节拍；单发 60s 语义易误杀合法心跳；go-common 组件复用 |
| 2 | §5.2/§10.2 admin 面 Bearer ADMIN_TOKEN + 部署层双保险 | 移除服务端 token：license-service 为内网 gRPC 服务，授权由边缘用户/权限系统（网关 + user-service RBAC）负责；唯一硬边界是 gRPC 端口/管理路径绝不暴露公网 | 静态共享 token 无身份信息（审计答不了"是谁"），与 servekit 分层不符 |
| 3 | §9.1 签名钥：默认 seed（kid=null）+ 可选 secondary/kid，signingKeyId 可为 null | **扁平钥列表**：`signing.keys[]`（{key_id, seed}，≥1 条）+ `sign_key_id`（单钥可省，多钥必填）；**每张凭证恒带 signingKeyId**，客户端公钥表 {kid→pubkey} 查不到即 fail-closed，协议中不再存在 kid=null 特例 | 无"默认钥"特例更对称；单钥=列表一项，多钥=加条目；golden 向量已按新格式重生成 |
| 4 | §13 部署：Caddy TLS 反代 | Caddy 或 nginx 均可（等价配置）；拓扑不变（Caddy/nginx → 未来独立网关 → 本服务 gRPC :19096） | 部署层选型开放；nginx 原生 IP 限流更强 |
| 5 | 配置默认值散落代码兜底 | 默认值唯一来源是 configx `default:` 标签；不完整配置启动时 fail-fast（resolveSigner/resolveDomainOptions） | 避免默认值双源漂移 |
| 6 | dbx 平铺连接键 | go-common 新版 dbx 嵌套子配置（`database.postgres.*` + `driver`） | 对齐当前 go-common |

另：HTTP 网关面（§5 的 google.api.http 注解仍全部保留）由将来的独立网关服务实现，
本期服务 gRPC-only；网关实现规格见 `docs/wire-contract.md`，对接指南见 README。

## 0. 与原方案（license-server-golang.md）的差异总表

| # | 维度 | 原方案 | 本方案 | 理由 |
|---|---|---|---|---|
| 1 | 技术栈 | chi + SQLite + sqlc + goose 单二进制 | servekit 栈：grpcx + grpc-gateway + dbx(GORM) + PostgreSQL + redisx + configx + lifecycle | 对齐 dev-skills `golang-service-development` 架构约束；三模式/脚手架/代码生成白拿 |
| 2 | 存储 | SQLite WAL | PostgreSQL（dbx，GORM AutoMigrate + gorm gen dal） | 用户决策：compose 全家桶，多实例门留开；原方案 §12.4 的演进路径提前兑现 |
| 3 | 限速 | 进程内 map | Redis（redisx 直用 INCR/EXPIRE/DECR） | 用户决策；409 退还语义 go-common ratelimit 不支持，故直用 redisx |
| 4 | 管理面 | licensectl CLI 直连同库 | `LicenseAdminService` gRPC + HTTP 注解（swagger 自动生成），Bearer admin token 保护 | 用户决策：与其他服务对齐；部署层不暴露 admin 路径 |
| 5 | 错误响应 | 文档约定 `error.code` | **HTTP 状态码是硬契约**（客户端不解析 `error.code`，按状态码分流）；错误体形状不变 | 以客户端代码为准（server.rs:91-169） |
| 6 | 请求字段名 | `licenseKey` | activate/deactivate 接受 `key`（客户端实发）与 `licenseKey`（文档形态）双字段 | 客户端代码与原文档脱同步的勘误；服务端双兼容，客户端零改动 |
| 7 | 签名密钥 | 部署时生成 | 同，且明确：**新生产 seed**，aividlab 发版前替换钉死的占位公钥常量 | 客户端当前钉联调占位公钥，配对 seed 不在库；app 未发布，换钥零成本 |
| 8 | metrics | Prometheus /metrics | 裁剪：结构化日志 + healthz；需要时再补 | 单实例规模下日志够用，避免过度建设 |
| 9 | 备份 | `VACUUM INTO` 快照 | 每日 `pg_dump` + 异机拷贝 | 随存储选型 |

业务语义（激活/心跳/槽位/试用/凭证模型/离线容忍）**全部继承原方案不变**，客户端契约以 aividlab 代码为最终事实源。

## 1. 目标与非目标

### 1.1 目标

1. 一个 servekit 栈微服务 `license-service`：`LicenseService`（客户端面，HTTP 注解精确路径）+ `LicenseAdminService`（管理面）双 proto service。
2. 三个客户端端点语义与原方案一致：`POST /v1/activate`（幂等收敛器：首次激活/心跳/指纹重绑/加购刷新/槽满驱逐重试）、`POST /v1/deactivate`、`POST /v1/trial/start`，外加 `GET /healthz`。
3. 签发 Ed25519 分离式签名凭证 v1（§8）：客户端本地验签离线可用；订阅/试用离线容忍 30 天、买断 365 天（客户端侧常量，服务端无对应端点）。
4. 服务端执行槽位：`device_token` 是设备身份锚——token 认得则复用原槽位并顺手更新 `fingerprint_id`（自动重绑）；全新 token 占新槽；槽满 409 + 设备列表；带 `evictDeviceToken` 重试踢旧占新。
5. keyless 试用：按 `(fingerprint_id, module)` 记账防重置；重装应用换 token 不重置。
6. 发 key / 改权益经 `LicenseAdminService` RPC（对齐原 licensectl 全命令面），admin token + 部署层双重保护。
7. 限速：per `(key_hash 或 fingerprint_id, device_token)` 每分钟 1 次，409 不消耗额度。
8. 大陆可达单实例部署（Caddy TLS 反代，只放行客户端路径）。

### 1.2 非目标

- 不做账号体系、每模块独立 key、在线功能开关。
- 不做支付/订单对接（扩展预留 §15）。
- 不做客户端时钟回拨防御（客户端本地时间高水位 + HMAC 已落地，服务端只保证 `issuedAt` 用服务端时钟且每次重签不复用旧时间戳）。
- 不做服务端租约回收（不因设备长期不心跳而释放槽位）。
- 不做 Prometheus metrics（本期裁剪）。
- 不解决 key 共享的"人情分享"问题（3 槽即产品决策容忍度）。

## 2. 技术选型

| 层 | 选择 | 理由 |
|---|---|---|
| 架构 | dev-skills `golang-service-development`：`pkg/{handler,xcodes,config,option,server,client,module}` + `internal/{service,store,thirdcall}` 分层 | 栈强制约束；三模式（gRPC / gateway / in-process）一份代码 |
| HTTP | grpc-gateway v2，`google.api.http` 精确路径注解 | user-service `/v1/auth/login` 同款既有模式；swagger 自动生成 |
| 存储 | PostgreSQL via `dbx`（GORM），gorm gen 生成 dal，`cmd/server migrate` AutoMigrate | 栈标准件；迁移与发版解耦 |
| 限速 | `redisx` 直用（INCR/EXPIRE/DECR 固定窗口 + 409 退还） | go-common `ratelimit` 无退还语义；键命名对齐 go-common Redis 约定 |
| 错误 | `xerr` + `pkg/xcodes`（域文件）+ 自有 error interceptor + 自定义网关错误处理器 | 还原客户端错误体契约（§6） |
| 签名 | `crypto/ed25519` 标准库；canonical JSON 手写确定性序列化器 | 无第三方密码学依赖；值域全 ASCII 安全，手写比依赖 sonic/反射行为更可控，golden 锁死 |
| UUID | google/uuid（certId、请求内随机量） | 标准件 |
| 部署 | golang-service-docker `render.sh` 产出 Dockerfile/compose；Caddy 前置 TLS | 栈标准件；大陆可达约束在 Caddy/域名层解决 |

**前置依赖（go-common 小变更）**：`grpcx.ServerConfig` 增加两个向后兼容字段并打 tag——

- `GWMuxOptions []runtime.ServeMuxOption`：注入自定义错误处理器（现在 `runtime.NewServeMux()` 无参创建，错误形状无法定制）；
- `GWHandlerWrapper func(http.Handler) http.Handler`：在网关 mux 外包中间件（`X-Request-Id` 生成 + 4 KiB 请求体上限 + 访问日志）。

## 3. 总体架构

```text
                        互联网（用户网络，可能经用户 HTTP 代理）
                              │  HTTPS
                              ▼
                    ┌──────────────────┐
                    │   Caddy（境内）   │  TLS 终结 + ACME
                    │ license.aividlab.app（占位域名，部署时定）
                    │ 只放行: /v1/activate /v1/deactivate
                    │        /v1/trial/start /healthz
                    └────────┬─────────┘
                             │ 127.0.0.1 gateway 端口（HTTP，grpc-gateway）
                             ▼
   ┌──────────────────────────────────────────────────────┐
   │  license-service（Go，grpcx：gRPC + HTTP gateway）       │
   │  中间件: recover/logging → protovalidate → admin-auth    │
   │         → license error interceptor（xerr→status+details）│
   │  网关层: 自定义错误处理器 + X-Request-Id/4KiB/访问日志包装   │
   │                                                        │
   │  pkg/handler            薄壳（每个 RPC 一行委托）          │
   │  internal/service/service.go   本体 + facade             │
   │  internal/service/activation/  激活/槽位/试用域             │
   │  internal/service/admin/       key/权益/设备/试用管理域     │
   │  internal/service/cert/        canonical JSON + Ed25519  │
   │  internal/store/{models,dal,generated}                   │
   └──────────┬───────────────────────────┬──────────────────┘
              │ dbx (GORM)                 │ redisx
              ▼                            ▼
        ┌──────────┐                 ┌──────────┐
        │ Postgres │                 │  Redis   │
        └──────────┘                 └──────────┘

   运维/发 key：grpcurl（gRPC 端口，仅内网）或网关 /v1/admin/*（不对外暴露，
   Bearer <ADMIN_TOKEN>）——swagger 由 openapiv2 注解自动生成

   客户端（aividlab, Tauri）
   ├── Ed25519 公钥：编译期钉死（发版前替换为生产公钥）
   ├── fingerprint 模块提供 {deviceToken, fingerprintId}
   └── 持有明文 key；本地存凭证，启动验签，24h 心跳（心跳 = 幂等 activate）
```

三模式：standalone（`NewServer`）、客户端（`NewClient`，供未来其他服务校验凭证/查询权益）、in-process（`NewModule`，测试与嵌入）。

## 4. 数据模型

四张表（GORM models，`internal/store/models/`；枚举在 proto 定义、DB 存 int32，转换只在 service 层 store 边界做；时间列 `time.Time`；**dbx 禁用外键，级联删除走应用层事务**；无软删除——keys 的吊销是 `status` 软状态，devices/trials 是硬行）：

```text
keys                                 // key 主档，明文永不落库
  key_hash        string  PK         // 64 小写 hex
  license_id      string  unique     // 'lk_' + key_hash[0:32]
  key_prefix      string             // 归一化明文前 8 字符，客服肉眼比对用
  label           string             // 备注（购买人/渠道/订单号）
  max_slots       int32   default 3  // >= 1
  status          int32              // 1=active 2=revoked
  created_at      time.Time
  revoked_at      *time.Time         // revoked 时必有；恢复时清空

entitlements                        // 一把 key 按模块一行，免费模块不进表
  key_hash        string  PK(复合)   // → keys.key_hash
  module          int32   PK(复合)   // 1=downloads 2=tools
  kind            int32              // 1=perpetual 2=subscription 3=trial
  expires_at      *time.Time         // perpetual 恒 NULL；subscription/trial 恒非 NULL
  granted_at      time.Time          // 最近一次授予/修改

devices                             // 一行 = 一把 key 上的一个在槽设备
  key_hash             string  PK(复合)
  device_token         string  PK(复合)  // 客户端指纹模块生成后永不复改的 UUID v4
  fingerprint_id       string             // 'v1.' + 64hex；token 认得时每次顺手更新
  first_seen_at        time.Time
  last_seen_at         time.Time          // 最近一次 activate 成功（心跳即 activate）
  last_fingerprint_at  *time.Time         // 最近一次指纹重绑；从未变过则 NULL
  索引: (key_hash)                      // 槽位计数
trials                              // 试用记账，独立于 key 体系
  fingerprint_id       string  PK(复合)   // 重装应用换 token 不重置
  module               int32   PK(复合)
  started_at           time.Time          // 服务端时钟；expiresAt 由它推出
  first_device_token   string             // 仅审计
```

要点：

- 行数 ≤ `max_slots` 由 activate 事务保证（无 DB 约束）。
- `DeleteKey`（物理删除）在一个事务里先删 entitlements/devices 再删 keys，替代 SQL CASCADE。
- 无凭证表：凭证不持久化，每次 activate 按当前库状态重签（幂等），这是离线模型简单性的来源。
- kind 与 expires_at 的 NULL 配套是服务端写入硬约定（admin Grant 与 activation 双侧校验）。

## 5. proto 与 HTTP API

`api/proto/license/v1/license.proto`，package `license.v1`，生成到 `gen/license/v1`。

### 5.1 `LicenseService`（客户端面）

| RPC | HTTP | 语义 |
|---|---|---|
| `Activate` | `POST /v1/activate` | 激活/心跳/重绑/刷新/evict 重试，幂等收敛器 |
| `Deactivate` | `POST /v1/deactivate` | 释放槽位，幂等（不在槽也 200） |
| `TrialStart` | `POST /v1/trial/start` | keyless 试用 / 带 key 合并签发 |
| `Health` | `GET /healthz` | `{status, checks:{db, signing}}`，任一失败 503 |

**请求形状**（proto 字段 → wire JSON 名，protojson 接受 camelCase/snake_case 双写法）：

```jsonc
// POST /v1/activate
{
  "key":              "AV1D-XXXXX-XXXXX-XXXXX-XXXXX",   // 客户端实发字段名（proto: key）
  "licenseKey":       "AV1D-...",                        // 可选兼容字段（proto: license_key，
                                                       //   wire 接受 licenseKey/license_key 双名）
  "fingerprintId":    "v1.9f86d081884c...",              // ^v1\.[0-9a-f]{64}$
  "deviceToken":      "6f9619ff-8b86-d011-b42d-00cf4fc964ff",  // UUID v4
  "evictDeviceToken": "0f8c2c33-..."                     // 可选，仅槽满 409 后重试携带
}
// POST /v1/deactivate：{ key | licenseKey, deviceToken }
// POST /v1/trial/start：{ module, fingerprintId, deviceToken, licenseKey? }
//   module ∈ {"downloads","tools"}（proto enum 的 wire 名）
```

`key` 与 `licenseKey` 二选一（服务端取先非空者；两者都空 → 400 `BAD_KEY_FORMAT`）。此为勘误兼容：客户端代码发 `key`（server.rs:55-63），原方案文档写 `licenseKey`，两者都支持。

**成功响应**：

```jsonc
// 200 activate / trial-start（客户端只解析 payload+signature，其余字段为未来 UI 预留）
{
  "payload":   "{\"certId\":\"...\",...}",   // 被签名的 canonical JSON 字符串原样（§8）
  "signature": "MEUCIQD...",                 // base64(64 字节 Ed25519)
  "slots": { "used": 2, "max": 3, "devices": [ { "deviceToken": "...", "fingerprintId": "...",
               "firstSeenAt": "...", "lastSeenAt": "...", "name": null } ] }  // 仅 activate
}
// 200 deactivate
{ "released": true }                          // false = 本来就不在槽
// 200 trial-start 另带 "alreadyStarted": false
```

`DeviceSlotInfo.name`：客户端接受可选设备名用于 409 选择器展示；服务端当前无名称来源，恒 null（预留列/来源后续可加）。

**请求约束**：请求体上限 4 KiB（网关中间件强制，超限 413）；`Content-Type: application/json`（网关默认只路由 JSON）；所有响应带 `X-Request-Id`；全部端点 **不开 CORS**（Tauri 原生层请求）。客户端超时 15s、不重试——服务端 P99 预算 < 1s。

### 5.2 `LicenseAdminService`（管理面）

全 RPC 带 `google.api.http` 注解（swagger 自动生成），前缀 `/v1/admin`，与原 licensectl 命令一一对应：

| RPC | HTTP | 对应 licensectl | 说明 |
|---|---|---|---|
| `CreateKey` | `POST /v1/admin/keys` | `key create` | label、slots、grants[]（module:kind[:expiry]）；**响应含明文 key，仅此一次** |
| `ShowKey` | `GET /v1/admin/keys/{keyId}` | `key show` | 状态/权益/在槽设备；绝不含明文 key |
| `ListKeys` | `GET /v1/admin/keys?status=&limit=` | `key list` | |
| `UpdateKey` | `PATCH /v1/admin/keys/{keyId}` | `key set-slots` / `set-label` | label 与 slots 均可选更新 |
| `RevokeKey` | `POST /v1/admin/keys/{keyId}/revoke` | `key revoke` | 软吊销，devices/entitlements 保留 |
| `UnrevokeKey` | `POST /v1/admin/keys/{keyId}/unrevoke` | `key unrevoke` | 槽位原样接回 |
| `DeleteKey` | `DELETE /v1/admin/keys/{keyId}` | `key delete` | 物理删除，应用层事务级联；需 confirm |
| `GrantModule` | `PUT /v1/admin/keys/{keyId}/grants/{module}` | `grant` | upsert；perpetual 禁带过期；`duration_days`（顺延，基于 max(now, 当前 expires_at)）或 `expires_at`（绝对覆盖）二选一 |
| `RevokeModule` | `DELETE /v1/admin/keys/{keyId}/grants/{module}` | `revoke-module` | 下一次签发即消失 |
| `ListKeyDevices` | `GET /v1/admin/keys/{keyId}/devices` | `devices list` | |
| `KickDevice` | `DELETE /v1/admin/keys/{keyId}/devices/{deviceToken}` | `devices kick` | 管理侧驱逐 |
| `ShowTrial` | `GET /v1/admin/trials/{fingerprintId}` | `trials show` | 可选 module 过滤 |
| `ResetTrial` | `POST /v1/admin/trials/{fingerprintId}/{module}/reset` | `trials reset` | reason 必填（进审计日志），唯一人工重置通道 |
| `ShowPubKey` | `GET /v1/admin/signing/pubkey` | `signing show-pubkey` | 当前 seed 派生公钥 base64，供钉客户端 |

`{keyId}` 统一接受 `licenseId`（`lk_...`）或完整 `key_hash`。变更对客户端的下一次 activate（心跳 ≤ 24h）自动生效，无需推送。所有变更 RPC 输出结构化信息供审计管道收集（含 reason 字段）。

## 6. 错误处理管线

**客户端事实**：不解析 `error.code`，按 HTTP 状态码分流（server.rs:113-169）——2xx 成功；activate 409 → 设备选择器；429 与 5xx → NETWORK_ERROR（"服务端故障绝不等于 key 无效"）；其余 4xx → LICENSE_INVALID（透传 `error.message` 给用户）；deactivate 的 404 也算成功（本服务幂等设计恒 200，不触发）。因此**状态码必须精确**，`error.code` 供日志/监控。

三段管线：

1. **`pkg/xcodes/license.go`**（reason 大写对齐栈惯例）：

   | reason | category | gRPC code | HTTP | 触发 |
   |---|---|---|---|---|
   | `BAD_KEY_FORMAT` | BadRequest | InvalidArgument | 400 | 归一化后不合 `^AV1D[0-9A-Z]{20}$`；key/licenseKey 双空；module 非售卖模块 |
   | `KEY_NOT_FOUND` | Unauthorized | Unauthenticated | 401 | key_hash 查无 |
   | `KEY_REVOKED` | Forbidden | PermissionDenied | 403 | status=revoked（含已在槽设备的心跳） |
   | `SLOT_LIMIT` | Conflict | AlreadyExists | 409 | 槽满且未带有效 evict |
   | `ALREADY_ENTITLED` | BadRequest | InvalidArgument | 400 | trial/start 模块已有该 key 有效权益 |
   | `RATE_LIMITED` | TooManyRequests | ResourceExhausted | 429 | 60s 窗口超限 |

   gRPC code → HTTP 状态经 grpc-gateway 默认映射恰好逐项吻合上表。

2. **自有 error interceptor**（license-service 内实现，取代 `grpcx.ErrorInterceptor` 并增强）：`xerr` category → gRPC code 映射不变；status message 保留 `"REASON: message"` 原文（网关层可还原）；支持业务侧附加 proto details（`SlotLimitInfo{maxSlots, devices[]}`、`RetryAfterInfo{seconds}`），经 `status.WithDetails` 透传。

3. **自定义网关错误处理器**（`runtime.WithErrorHandler`，经 go-common `GWMuxOptions` 注入）：输出客户端契约形状——

```json
{
  "error": {
    "code": "slot_limit",          // reason 转小写
    "message": "该密钥的 3 个设备槽位已满，请选择一台设备下线后重试",
    "maxSlots": 3,                 // ← SlotLimitInfo detail 展平
    "devices": [ { "deviceToken": "…", "fingerprintId": "…", "firstSeenAt": "…", "lastSeenAt": "…" } ]
  }
}
```

429 响应另设 `Retry-After: <秒>` 头（来自 RetryAfterInfo）。5xx 走同处理器输出 `{error:{code:"internal",...}}`，不泄漏内部细节。

## 7. 业务规则

### 7.1 activate 状态机（幂等场景表，语义继承原方案 A1–A12）

| # | 场景 | 前置状态 | 行为 | 响应 |
|---|---|---|---|---|
| A1 | 首次激活 | key active；无该 token 行；未满槽 | INSERT 新行 → 签发 | 200 |
| A2 | 日常心跳（启动 + 每 24h） | 行存在；指纹相同 | 复用槽位；刷 last_seen_at；重签（issuedAt=now） | 200 |
| A3 | 指纹变化自动重绑 | 行存在；fingerprint_id 不同 | **token 认得即复用原槽位**，更新 fingerprint_id 与 last_fingerprint_at | 200 |
| A4 | 加购后刷新 | 行存在；entitlements 已变 | 无特殊分支：每次全量重读重签 | 200 |
| A5 | 槽满 | 无行；行数 ≥ max_slots；无 evict | 409 附在槽设备列表 | 409 |
| A6 | 带 evict 重试 | A5 后用户选定一台 | 删目标行 → INSERT 本机 | 200 |
| A7 | evict 目标已被并发释放 | 目标行已不在 | 删 0 行照常继续；有空位成功，仍满再 409 | 200/409 |
| A8 | 订阅过期 | expires_at < now | **照常签发**（过期条目进凭证；过期由客户端判定，通道留给续费） | 200 |
| A9 | key 整体吊销 | status=revoked | 拒绝（含在槽老设备心跳） | 403 |
| A10 | key 不存在 | key_hash 查无 | 拒绝 | 401 |
| A11 | 限速 | 60s 内同键已处理 | 拒绝 | 429 + Retry-After |
| A12 | 重装应用后同 key | 全新 token；旧 token 行占满槽 | 即新设备 → 409 引导（下线旧"自己"）；试用不受影响（按指纹记账） | 409 |

处理顺序：归一化（大写+剔非 `[A-Za-z0-9]`）→ SHA-256 → 限速 → key 查验 → evict 删除 → **同事务**槽位判定（行存在则 UPDATE 复用/重绑；新 token 则 count ≥ max_slots 即 409，否则 INSERT）→ 组装 entitlements ∪ 该指纹 trial 账本（同模块 key 条目优先；**试用必须并入**，否则 key 凭证与试用凭证在客户端单槽存储下互相冲掉）→ 签发 → 200。

服务端时间义务：每次响应用**当前时间**重签（certId 新 UUID、issuedAt=now），永不复用旧时间戳——客户端 issuedAt 单调水位（Advance/Accept/Reject 三态，5 分钟容差）依赖此性质。

### 7.2 槽位生命周期

约束语义是"**同时**几台"：任一时刻该 key 的 devices 行数 ≤ max_slots，由 activate 事务保证（PG 事务内 count+insert；`devices` 复合主键防同 token 并发双插）。指纹变化不产生新槽——只有 device_token 是槽位身份，fingerprint_id 是可变备注。**不做超时回收**（长期关机回来被自动踢槽是最恶劣的付费体验）；槽位紧张的正规出口是 deactivate（本机自助）、evict（换机自助）、KickDevice（客服兜底）。吊销是 key 级冻结：行保留，恢复 active 后槽位原样接回。

### 7.3 deactivate

删除 `(key_hash, device_token)` 行释放槽位；幂等（不在槽 200 `released:false`）；key 已吊销也允许（清理无害）。不负责吊销客户端本地凭证——客户端收 200 后自行删除并退出授权态；离线签名模型下服务端无（也不需要）使已签发凭证失效的手段，吊销靠客户端窗口自然收敛（订阅 30 天/买断 365 天）。

### 7.4 trial/start

1. 查 `(fingerprint_id, module)`：存在 → 幂等返回按既有 `started_at` 组装的凭证，`alreadyStarted:true`（重装应用换 token 不重置）。
2. 不存在 → `INSERT ... ON CONFLICT DO NOTHING`；冲突（并发）→ 重读该行走 1。同一指纹同一模块永远只有一个 `started_at`。
3. `expiresAt = started_at + trial_days`（配置，默认 14 天）。防重置边界：指纹原料是安装级标识，管理员改注册表/克隆 VM 可低于"重装系统"成本重置——接受底线（试用价值低、每轮手工改系统标识性价比低），规模化滥用由限速约束；v2 多信号指纹是指纹侧演进通道，不构成本服务协议变更。
4. **凭证组装**（客户端凭证单槽整体替换，服务端必须一张凭证给全）：
   - keyless：`licenseId:null`，entitlements = 该指纹 trial 账本**全部条目含已过期**（客户端据此推导 trialUsed）；
   - 带 key：先走与 activate 完全相同的 key 校验与槽位纪律（401/403/409 语义一致），然后 `licenseId=lk_...`，entitlements = key 权益 ∪ 账本（同模块 key 条目优先）；
   - 请求模块已有该 key 有效权益 → 400 `ALREADY_ENTITLED`。

### 7.5 限速

- 规则：per `(key_hash 或 fingerprint_id, device_token)` 每 60 秒 1 次，覆盖三个业务端点（activate/deactivate 用 key_hash，trial-start 用 fingerprint_id）。
- 实现：redisx 直用——`INCR license:rate:{id}:{device_token}`，首次 `EXPIRE 60`；`INCR` 结果 > 1 → 429；**最终响应为 409 时 `DECR` 退还**（客户端选完设备必须能立即重试；409 无签发成本、攻击面仅为读自己的设备列表）。
- 这是对正常心跳节律（24h）的事实性护栏，非反 DDoS 边界；粗粒度 IP 限流交给 Caddy 可选模块，服务端逻辑不依赖 IP。

## 8. 凭证管线

### 8.1 canonical JSON（精确规则）

对象组装后按以下规则序列化为**待签名字节**：UTF-8 无 BOM 单行；紧凑无空白；**对象成员按键名 UTF-8 字节序升序递归排序**；字符串转义仅 `" \` \b \f \n \r \t` 与 U+0000–U+001F（`\u00xx` 小写 hex），**其余字符（含非 ASCII）原样输出**；数值仅小整数 `v`（十进制无前导零）；字面量小写；凭证不含数组（entitlements 用对象，成员名即模块名）。在本协议值域（ASCII 的 UUID/hex/RFC3339/枚举/小整数）上与 RFC 8785（JCS）逐字节一致。

**客户端 canonical 自证（硬约束）**：客户端验签后会重算 canonical JSON 并断言与 wire `payload` 逐字节相等（facade.rs:655-657）——服务端下发的 payload 字符串**必须本身已是 canonical 形态**，仅语义等价不行。实现用手写确定性序列化器（cert 子包内，无反射），golden 与属性测试锁死。

### 8.2 凭证 v1 schema（以客户端 cert.rs 为准）

```json
{
  "v": 1,
  "certId":        "3fa85f64-5717-4562-b3fc-2c963f66afa6",
  "licenseId":     "lk_9f86d081884c7d659a2feaa0c55ad015",
  "deviceToken":   "6f9619ff-8b86-d011-b42d-00cf4fc964ff",
  "fingerprintId": "v1.<64 位小写十六进制指纹>",
  "issuedAt":      "2026-08-24T12:00:00Z",
  "signingKeyId":  null,
  "entitlements": {
    "downloads": { "kind": "perpetual",    "expiresAt": null },
    "tools":     { "kind": "subscription", "expiresAt": "2027-08-24T12:00:00Z" }
  }
}
```

| 字段 | 语义 |
|---|---|
| `v` | schema 主版本；客户端不认识的主版本拒绝凭证并走重激活 |
| `certId` | 服务端生成 UUID v4，凭证实例标识（日志关联）；服务端不存凭证 |
| `licenseId` | `"lk_"+key_hash[0:32]`；keyless 试用凭证为 `null`；绝不出现明文 key |
| `deviceToken` | 设备身份锚；客户端与本地快照比对，Foreign（token 不匹配）即整凭证丢弃 |
| `fingerprintId` | 签发时上报指纹；Drift（token 同指纹异）触发客户端后台自动重绑 |
| `issuedAt` | 服务端时钟 RFC3339 UTC **秒精度**；客户端离线容忍计时锚 + 单调水位 |
| `signingKeyId` | 【修订 3】**恒存在**（无默认钥特例）。必须命中客户端公钥表，表外 fail-closed 不回落（防 kid 剥离降级） |
| `entitlements` | 按模块一条，仅售卖模块；perpetual 的 `expiresAt` 恒 null（客户端强校验，违反即 Malformed 拒收）；subscription/trial 恒非 null；未知模块键客户端保留不拒 |

客户端解析规则：未知顶层字段忽略；未知主版本拒绝。

### 8.3 key 格式与 licenseId 算法

1. key 明文：`"AV1D-"` + 4 组 5 字符 Crockford base32（`0123456789ABCDEFGHJKMNPQRSTVWXYZ`，无 I/L/O/U），显示形 28 字符，熵 100 bit，CreateKey 时生成（归一化唯一性由熵保证，碰撞重试）。
2. 归一化（哈希前，双端同规则）：trim → 转大写 → 剔除非 `[A-Za-z0-9]` → 校验 `^AV1D[0-9A-Z]{20}$`（24 字符）。
3. `key_hash = lowercase_hex(SHA-256(归一化串))`；`licenseId = "lk_" + key_hash[0:32]`（前缀截断碰撞概率可忽略，unique 约束兜底）。

### 8.4 签名与传输

- 算法 Ed25519（RFC 8032，标准库），私钥 32 字节 seed，签名 64 字节。
- 被签字节 = canonical payload 字符串的 UTF-8 字节；**分离式签名**，`{payload, signature}` 并行传输，signature 为 base64（标准字母表带 padding）。
- wire 层 payload 原样携带确切字节，客户端零规范化成本验签；客户端自算 canonical 与之逐字节互证。
- keyless 与 key 凭证同一管线，仅 `licenseId:null`、entitlements 为账本全量。

## 9. 私钥管理与轮换

### 9.1 注入与备份

【修订 3，见文首修订记录】：
- 签名钥是**扁平列表**：`signing.keys[]`（`{key_id, seed}`，≥1 条）+ `sign_key_id`（当前签发钥；单钥可省略即隐式唯一，多钥必填，指向不存在的 kid 启动报错）。seed（32 字节，64 hex）经环境变量注入（configx `WithExpandEnv`）；不入库、不入 git、不进日志；**离线副本必须存在**（密码管理器/纸质）——丢 seed = 该钥签过的全部凭证无法续签，比泄露更不可恢复。
- 公钥由 seed 派生，`ShowPubKey` 返回 `{active_key_id, keys[]}`（全部公钥按 kid 排序）供钉客户端公钥表。
- **每张凭证恒带 `signingKeyId`**（无 null 特例）；客户端公钥表 {kid→pubkey} 查不到即拒收（fail-closed）。
- 轮换：新钥加 `keys:` 条目 → 发版客户端公钥表带上新 kid → `sign_key_id` 切换重启。顺序不可反（客户端没有该 kid 时新凭证会被拒收）。

### 9.2 生产密钥策略（已定）

客户端当前钉联调占位公钥 `3yOArrYtbcloP9GWxZD6l+cPwkxfhWrtNXkLRpesiws=`（配对 seed 不入库）。策略：**部署时生成全新生产 seed**（`openssl rand -hex 32`）作默认钥（kid=null）；aividlab 发版前替换 `LICENSE_SIGNING_DEFAULT_PUBKEY_B64` 常量（一行）。app 未发布、无存量凭证，换钥零成本。客户端多钥表继续留空，真正轮换时才启用。

### 9.3 泄露事故预案（runbook）

信任模型：客户端只认钉死公钥，私钥泄露后**只有客户端发版换公钥才能真正止血**。序列：确认泄露 → 生成新 seed 服务端切换（重启加载）→ 客户端发版钉新公钥（旧版存量凭证用至离线窗口耗尽：订阅 30 天/买断 365 天，接受该有界窗口）→ 旧版升级后本地验签失败 = 视为无凭证静默重激活 → 事后审计签发量异常区间。伪造凭证只影响客户端本地，服务端槽位表不受签名伪造影响——分离式离线签名的结构性优点。

## 10. 安全考量

1. **TLS**：Caddy 终结（ACME），域名部署境内节点（大陆可达是硬约束：服务不可达 = 30 天后订阅全锁）。gateway 与 gRPC 端口只绑 127.0.0.1/内网；Caddy 只反代四个客户端路径。
2. **admin 面**【修订 2，见文首修订记录】：服务端不做鉴权（原 Bearer ADMIN_TOKEN 方案已移除）。授权由边缘用户/权限系统负责；**唯一硬边界：gRPC 端口与管理路径绝不暴露公网**。
3. **key 哈希**：库中只有 SHA-256；100 bit 熵离线爆破不可行；明文仅 CreateKey 响应展示一次；`key_prefix`（8 字符）仅供客服比对。
4. **日志脱敏（硬规则）**：明文 key 绝不进日志（记 licenseId 替代）；fingerprint_id 绝不进日志（哈希过的指纹仍是可跨源对账的机器追踪标识），设备排障用 device_token 与 certId；不记录 remote_addr；`payload`/`signature` 不进日志。
5. **注入面**：全部查询经 gorm gen dal（参数化）；protovalidate 字段校验前置（uuid/格式/长度），业务校验（key 归一化形状等）400 优先于一切副作用；请求体 4 KiB 上限。
6. **限速**：见 §7.5；Caddy 可选 IP 粗限，服务端逻辑不依赖 IP。
7. **吊销时效**：靠客户端离线窗口自然收敛（30/365 天）+ activate 403 拒绝新签；服务端无需新增机制。

## 11. 可观测性

- 结构化日志（slog）：每请求一行经网关访问日志中间件——`request_id`、`method`、`path`、`status`、`duration_ms`、`licenseId`（如适用）、`device_token`（如适用）、`error.code`。脱敏规则见 §10.4。
- admin 变更操作记审计日志（操作、目标、reason），供运维管道收集。
- `GET /healthz`：DB 可查（SELECT 1）+ 签名 seed 已加载；任一失败 503。外部拨测 2 分钟一次，连续 3 次失败告警（激活服务不可达是 P1 事件）。gRPC 端口自带标准 health 服务（grpcx 内建）。
- metrics 本期裁剪；磁盘/容器基础告警走宿主机监控。

## 12. 测试策略

1. **golden 向量（cert 包）**：消费与客户端 `cert.rs` 测试模块逐字节相同的常量（密钥为 RFC 8032 测试向量 1，公开值）——
   - `signingSeedHex: 9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60`
   - `signingPubkeyHex: d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a`
   - `expectedCanonicalPayload`: `{"certId":"3fa85f64-5717-4562-b3fc-2c963f66afa6","deviceToken":"6f9619ff-8b86-d011-b42d-00cf4fc964ff","entitlements":{"downloads":{"expiresAt":null,"kind":"perpetual"},"tools":{"expiresAt":"2027-08-24T12:00:00Z","kind":"subscription"}},"fingerprintId":"v1.9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08","issuedAt":"2026-08-24T12:00:00Z","licenseId":"lk_9f86d081884c7d659a2feaa0c55ad015","v":1}`
   - `expectedSignatureB64: VBDWouHSgIv5DbSYiQC880OpDk9gjuZMlpcIcLVDAq0G0Vz7iOexr2hApkTL2aiTEqzcjjtZacrw82hT0IaoDQ==`
   - 断言：seed 签名 === 期望值（Ed25519 确定性，双端必同值）；公钥验签通过；canonical 序列化→解析→重序列化逐字节稳定。另按同法生成两个辅助向量：keyless 试用凭证（licenseId:null + 账本全量形状）、单模块全非 null 最小凭证（对排序遗漏最敏感）。向量常量提交进服务仓库测试代码。
2. **槽位状态机表驱动**：§7.1 A1–A12 逐行落用例 + 边界：evict 自身 token（等价无 evict）、无效 evict token（删 0 行继续）、max_slots=1 全生命周期轮换、同 token 指纹反复变化（行数恒定、last_fingerprint_at 单调）、revoked 下 activate 403 / deactivate 200。
3. **trial 并发**：N≥8 并发同 `(fingerprint, module)`（含不同 device_token 模拟重装首启）→ 恰 1 行、started_at 全体一致、alreadyStarted 恰 N−1；合并组装三类断言（keyless 全量含过期 / 带 key 并集 key 优先 / ALREADY_ENTITLED）；trial_days=1 边界推算。
4. **网关 wire 契约测试**（起真实 gateway + Postgres/Redis 测试设施）：状态码精确分流（400/401/403/409/429）；错误体 `{"error":{code,message,maxSlots,devices}}` 形状与展平字段；`Retry-After` 头；`X-Request-Id`；payload 为 canonical 形态；`key` 与 `licenseKey` 双字段名都接受；4 KiB 超限 413。
5. **限速**：同键 60s 第二次 429；409 路径退还后立即重试不被限。
6. **集成设施**：`dbx.SetupTestDB`（Postgres testcontainer）+ `redisx.NewTestClient`（miniredis）；服务级测试优先走 `NewModule` in-process。
7. **新建服务验收清单**（skill §7）：build/lint 通过、`make proto && git diff --exit-code`、`make generate && git diff --exit-code`、grpcurl + curl 网关 + `NewModule` 三通、service.go facade 与 RPC 一一对应、无 demo 残留。

## 13. 部署

1. **打包**：golang-service-docker `render.sh` 产出标准多阶段 Dockerfile + docker-compose（license + postgres + redis 三容器）；`cmd/server` 单二进制（serve/migrate 子命令），部署先 `migrate` 再 serve。
2. **拓扑**：境内单实例；Caddy TLS 反代只放行 `/v1/activate`、`/v1/deactivate`、`/v1/trial/start`、`/healthz`；admin 路径与 gRPC 端口仅内网/localhost。域名占位 `license.aividlab.app`（客户端 `DEFAULT_BASE_URL` 同源，部署时两端一起定稿）。
3. **配置**：`config.example.yaml` 纯结构（值全 `${VAR}`）+ `.env.example`（compose 取向）；敏感项 `LICENSE_SIGNING_SEED`、`ADMIN_TOKEN`、DB/Redis 凭据全部环境注入。
4. **备份**：每日 `pg_dump` 快照，保留 14 份滚动 + 异机拷贝；备份含 key_hash 与 entitlements（收入数据），按敏感文件管理（0600）。RPO=备份间隔：心跳类写入丢失无感；槽位表回退最多"复活"已驱逐设备，由 activate 幂等语义自愈。
5. **容量**：10 万装机、心跳 24h → 平均 ~1.2 写/秒，发布尖峰 50–100 写/秒——单实例 Postgres 余量两个数量级。

## 14. 实施顺序（高层）

1. go-common：grpcx `GWMuxOptions` + `GWHandlerWrapper`（向后兼容），打 tag。
2. scaffold：`new-service.sh license --db --redis` → module/remote 修正 → 本文档入仓 `docs/design.md`（首个 commit）。
3. proto 双 service + xcodes + 错误管线（interceptor + 网关错误处理器）+ 网关中间件。
4. cert 子包（canonical + 签名 + golden）。
5. 四表 models + dal + migrate；activation 域（activate/deactivate/trial/限速）；admin 域。
6. Health + 测试全量 + 验收清单。
7. docker 打包 + README/CLAUDE.md + push。

## 15. 扩展预留

1. **接支付/订单**：keys 加 `order_id` 列；支付回调内部复用 CreateKey 逻辑；上线前先落 `audit_log` 表（who/what/when 流水，本期由 admin 审计日志替代）。
2. **按模块分槽**：devices 拆 `(key_hash, module, device_token)` 或 entitlements 加 max_slots；activate 协议形状不变，409 列表按模块分组。
3. **签名 key 轮换常规化**：§9.1 的 named key 机制即 §12.3 预留的转正；客户端多钥表已就位（当前空表）。
4. **多实例**：PG 已就位；限速已在 Redis；签名是无状态计算；无其他单写者状态——水平扩展只需在 Caddy/负载均衡层做。
5. **每 key 计费/子功能分层**：凭证条目追加可选字段（features/maxVersion），客户端按"未知字段忽略"平滑升级，`v` 兜底。

---

### 附：勘误与客户端事实备忘（实现时不要踩）

- activate/deactivate 请求字段客户端发 **`key`**；trial/start 可选字段是 **`licenseKey`**——服务端双兼容（§5.1）。
- 客户端**不解析 `error.code`**，状态码分流是硬契约；`error.message` 直接透传给用户（中文文案）。
- 409 的 `devices` 客户端从 `error.devices` 解析（顶层 `devices` 仅兼容回退）。
- `DeviceSlotInfo` 字段：`deviceToken`（必填）、`fingerprintId`/`lastSeenAt`/`name`（可空）。
- deactivate：客户端把 404 也当成功；本服务恒 200。
- 429 与 5xx 都映射 NETWORK_ERROR——服务端 5xx 不会让用户看到"key 无效"，但会提示稍后重试。
- 凭证 perpetual 条目 `expiresAt` 必须为 null、subscription/trial 必须非 null——客户端强校验，违反即 Malformed。
- 客户端会重算 canonical 并与 wire payload 逐字节比对——payload 必须 canonical。
- 客户端心跳 = 幂等 activate（启动 ~15s 一次 + 每 24h），无独立心跳端点。
- 服务端永不复用 issuedAt——每次响应当前时间重签。
