# license-service HTTP 网关契约（wire contract）

- 日期：2026-08-30
- 读者：未来独立 HTTP 网关（grpc-gateway 或等价实现）的维护者
- 事实源：`docs/design.md` §5–§7；本文件是其实施规格的可执行摘要
- 状态：license-service 本期以纯 gRPC 交付（`:19096`）；本契约描述网关必须提供的
  HTTP 面。路由与请求/响应形状以 `api/proto/license/v1/license.proto` 的
  `google.api.http` 注解与 `api/swagger/license/v1/license.swagger.json` 为准，
  本文件补充 proto 无法表达的**行为性**要求。

## 1. 拓扑

```text
Caddy（境内，TLS/ACME，license.aividlab.app 占位域名）
  │ 只放行: /v1/activate /v1/deactivate /v1/trial/start /healthz
  ▼
独立 HTTP 网关（本期未建；预留 :18086）
  │ gRPC（内网）                    ┌────────────────┐
  ▼                                 │ Postgres Redis │
license-service :19096 (gRPC-only) └────────────────┘
admin 面（/v1/admin/*）与 gRPC 端口只在内网可达，Bearer <ADMIN_TOKEN> 双保险。
```

## 2. 客户端硬契约（aividlab Rust 客户端的行为约束）

1. **客户端不解析 `error.code`，按 HTTP 状态码分流**：
   - 2xx → 成功；activate 409 → 设备选择器（从 `error.devices` 解析）；
   - 429 与 5xx → NETWORK_ERROR（服务端故障绝不等于 key 无效）；
   - 其余 4xx → LICENSE_INVALID（`error.message` 中文文案直接透传给用户）；
   - deactivate 的 404 也当成功（本服务恒 200，不会触发）。
2. 客户端超时 15s、不重试；服务端 P99 预算 < 1s。
3. 全部端点**不开 CORS**（Tauri 原生层请求）。
4. 心跳 = 幂等 activate（启动 ~15s 一次 + 每 24h），无独立心跳端点。
5. 请求体上限 4 KiB（网关强制，超限 413）；`Content-Type: application/json`。
6. 所有响应带 `X-Request-Id`（入站缺失则网关生成，原样回带）。

## 3. 状态码映射（gRPC → HTTP，grpc-gateway 默认映射恰好吻合）

| license 错误（gRPC code） | HTTP | reason（error.code，小写） | 触发 |
|---|---|---|---|
| InvalidArgument | 400 | `bad_key_format` / `already_entitled` / `bad_request` | 归一化后不合 `^AV1D[0-9A-Z]{20}$`；key/licenseKey 双空；module 非售卖模块；trial 模块已有有效权益 |
| Unauthenticated | 401 | `key_not_found` | key_hash 查无 |
| PermissionDenied | 403 | `key_revoked` | status=revoked（含在槽设备心跳） |
| AlreadyExists | 409 | `slot_limit` | 槽满且未带有效 evict |
| ResourceExhausted | 429 | `rate_limited` | 60s 窗口超限 |
| Unavailable | 503 | `service_unavailable` | healthz 任一检查失败 |

## 4. 错误体形状（网关必须还原）

gRPC status message 恒为 `"REASON: message"`（在第一个 `": "` 处切分：前段转小写
得 `error.code`，后段为 `error.message`）。`google.rpc.Status.details` 中的
`SlotLimitInfo` / `RetryAfterInfo`（license.v1 类型）**展平**进错误体：

```jsonc
// 409 slot_limit
{
  "error": {
    "code": "slot_limit",
    "message": "该密钥的 3 个设备槽位已满，请选择一台设备下线后重试",
    "maxSlots": 3,                  // ← SlotLimitInfo.maxSlots 展平
    "devices": [                    // ← SlotLimitInfo.devices 展平
      { "deviceToken": "…", "fingerprintId": "…",
        "firstSeenAt": "…", "lastSeenAt": "…", "name": null }
    ]
  }
}
```

- 429 响应另设 `Retry-After: <秒>` 头（来自 RetryAfterInfo.seconds，恒 60）。
- 5xx 走同一处理器输出 `{"error":{"code":"internal","message":"…"}}`，
  **不泄漏内部细节**（堆栈、SQL、依赖拓扑）。
- `DeviceSlotInfo.name` 恒 null（预留字段，服务端无名称来源）。

## 5. 请求字段勘误（双名兼容）

- activate/deactivate：客户端实发 `key`；文档历史形态 `licenseKey`——两者都接受，
  服务端取先非空者（gRPC 层是两个 proto 字段，网关 JSON 层天然双名透传即可）。
- trial/start 可选字段是 `licenseKey`。
- `module` 的 wire 名是小写模块名（`"downloads"` / `"tools"`）——proto 中该字段
  是带 `in` 集合校验的 string，网关直接透传。

## 6. 成功响应形状

见 proto（`ActivateResponse` / `DeactivateResponse` / `TrialStartResponse` /
`HealthResponse`）。要点：

- `payload` 是**被签名的 canonical JSON 字符串原样**（客户端会重算 canonical 并
  逐字节比对——网关不得重序列化/转义该字段）；
- `signature` 是 base64（标准字母表带 padding）的 64 字节 Ed25519 分离式签名；
- activate 响应含 `slots{used,max,devices[]}`；trial-start 含 `alreadyStarted`；
- deactivate 恒 200（`released:false` = 本来就不在槽）。

## 7. 网关中间件清单

1. `X-Request-Id` 生成/回带（§2.6）。
2. 请求体 4 KiB 上限 → 413（JSON 错误体同 §4 形状，code=`request_too_large`）。
3. 访问日志一行：`request_id`、`method`、`path`、`status`、`duration_ms`、
   `licenseId`（如适用）、`device_token`（如适用）、`error.code`。
   **脱敏红线**：明文 key / fingerprint_id / remote_addr / payload / signature
   绝不进日志。
4. 粗粒度 IP 限流可选（Caddy 层亦可）；服务端业务限速不依赖 IP。

## 8. 部署边界

- Caddy 只反代四个客户端路径；admin 路径与 gRPC 端口不暴露公网。
- 外部拨测 `/healthz` 2 分钟一次，连续 3 次失败告警（P1）。
- gRPC 端口自带标准 grpc health 服务（grpc_health_probe 兼容），供容器探活。
