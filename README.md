# license-service

基于 [go-common](https://github.com/servekit/go-common) 的 **纯 gRPC** 许可证服务：密钥签发、Ed25519 离线凭证、设备槽位、keyless 试用与限速。客户端 HTTP 面由将来的独立网关提供（见 [docs/wire-contract.md](docs/wire-contract.md)）。

- 设计文档（业务规则的唯一事实源）：[docs/design.md](docs/design.md)
- 实施计划：[docs/plans/2026-08-30-license-service-plan.md](docs/plans/2026-08-30-license-service-plan.md)
- 面向未来网关的 wire 契约：[docs/wire-contract.md](docs/wire-contract.md)

## 构建与运行

服务与数据库迁移合并为同一个二进制（`cmd/server`），通过子命令区分：

| 命令 | 作用 |
|---|---|
| `bin/license-service` 或 `bin/license-service serve` | 启动 gRPC 服务（默认） |
| `bin/license-service migrate` | 执行 GORM AutoMigrate 后退出 |
| 其他 | 打印用法，exit 2 |

```bash
make build       # 产出 bin/license-service
make run         # 本地启动（auto-cp config.example.yaml -> config.yaml）
make regenerate  # = generate + tidy（改 model 后跑；proto 变更去 ../api 仓库）
make migrate     # 执行数据库迁移
make test        # 测试（race + coverage；DB 测试需 Docker 起 testcontainer）
make lint        # golangci-lint
make docker-up   # 起完整 docker 栈（license + postgres + redis）
```

gRPC 监听 `:19096`（servekit 序列的下一个槽位）。HTTP 面本期不存在：
`server` 只有 `grpc_addr` —— 纯 gRPC，不监听 HTTP；对外 HTTP 面由网关提供（路由契约见 `docs/wire-contract.md`）。

## 配置

`config.example.yaml` 是**纯结构**——每个值都是 `${VAR}` 占位符，由 configx `WithExpandEnv` 从进程环境展开；默认值由 config 结构体上的 `default:` 标签提供（代码里不做兜底）。`.env.example` 是 **docker-compose 取向**的默认值源。

**必填项只有一个来源：签名钥**（见下节）。其余（DB/Redis/端口/限流）都有合理默认。

> admin 面不做服务端鉴权：license-service 是内网 gRPC 服务，授权由边缘的
> 用户/权限系统（网关 + user-service）决定，**gRPC 端口与管理路径绝不暴露公网**。

## 签名密钥：生成、配置与保管

先分清系统里的两种"密钥"，别混：

| | 签名钥（本节） | license key |
|---|---|---|
| 是什么 | Ed25519 密钥对，服务端给凭证签名 | 发给用户的 `AV1D-XXXXX-…` 激活码 |
| 存在哪 | 只在部署环境变量 + 进程内存 | 服务端**只存 SHA-256 hash**，明文仅发 key 时出现一次 |
| 丢了怎样 | 该钥签过的所有凭证**永远无法续签** | 用户拿凭证里的 kid 无法对上，重新发一把即可 |

### 1. 生成

```bash
openssl rand -hex 32     # 输出 64 个 hex 字符 = 32 字节 Ed25519 seed
```

kid（key_id）是给这把钥起的名字，随便取但建议有含义（`k1`、`k2027-release`…）；
凭证里会带上它，客户端按 `{kid → 公钥}` 查表验签。

### 2. 配置（配置文件里只出现占位符，真实值永远在环境变量）

```yaml
# config.example.yaml —— 结构与占位
signing:
  keys:
    - key_id: ${LICENSE_SIGNING_KEY_ID}
      seed: ${LICENSE_SIGNING_SEED}
  sign_key_id: ${LICENSE_SIGNING_SIGN_KEY_ID}   # 单钥可空；多钥必填
```

```bash
# .env（本地）/ 部署环境变量 / 密钥管理器（生产）
LICENSE_SIGNING_KEY_ID=k1
LICENSE_SIGNING_SEED=9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60
LICENSE_SIGNING_SIGN_KEY_ID=
```

多把钥（轮换/按构建分片）就在 `keys:` 下继续加条目，每把一对独立的环境变量，
并把 `LICENSE_SIGNING_SIGN_KEY_ID` 指到当前用来签发的那把。

### 3. 启动后核对公钥，钉进客户端

```bash
grpcurl -plaintext localhost:19096 license.v1.LicenseAdminService/ShowPubKey
# → { "activeKeyId": "k1", "keys": [ { "key_id": "k1", "publicKeyB64": "…" } ] }
```

把 `{kid, publicKeyB64}` 钉进客户端构建的公钥表（编译期常量）。客户端验签规则：
凭证里的 `signingKeyId` 查表命中才验，查不到直接拒收（fail-closed）。

### 4. 保管（核心就三句话）

1. **seed 只存在于三个地方**：部署环境变量（运行时）、进程内存（派生用）、
   **离线副本一份**（密码管理器或纸质，防"丢 seed"灾难）。公钥不是秘密，随便放。
2. **seed 绝不出现于**：git（`.env` 已 gitignore）、日志、数据库、容器镜像层
   （compose 从环境注入，镜像里只有占位结构）、聊天记录/工单。
3. **丢 seed 的后果不可逆**：客户端的凭证到期/心跳时无法续签，等于那批用户
   全部失效——所以离线副本是上线前的 checklist 项，不是可选项。

### 5. 轮换与泄露（简版，详见 docs/design.md §9.3）

- **常规轮换**：新起一把（生成 → `keys:` 加条目 → 发版客户端公钥表带上新 kid →
  `LICENSE_SIGNING_SIGN_KEY_ID` 切过去重启）。切之前客户端表里没有该 kid 的话，
  新凭证会被旧客户端拒收——顺序不能反。
- **泄露**：服务端停用该钥（从 `keys:` 删除）只是止血一半；真正止血靠客户端发版
  换掉公钥表。旧版存量凭证用至离线窗口耗尽（订阅 30 天/买断 365 天）。

**本地跑（`make run`）：**

```bash
cp .env.example .env
# 编辑 .env：LICENSE_SIGNING_SEED 填值；
# LICENSE_SERVICE_DATABASE_HOST: postgres -> localhost
make run            # 需要本机 PostgreSQL + Redis
```

**docker compose 跑（`make docker-up`）：** 无需改 host 名——compose 注入全部 env。敏感项照旧走 `.env`。受限网络在 `.env` 加 `GOPROXY=https://goproxy.cn,direct`。

## 测试调用（gRPC）

```bash
# 发一把 key（内网直连 admin 面；明文 key 仅此一次出现）
grpcurl -plaintext \
  -d '{"label":"order-42","grants":[{"module":"MODULE_TOOLS","kind":"ENTITLEMENT_KIND_PERPETUAL"}]}' \
  localhost:19096 license.v1.LicenseAdminService/CreateKey

# 激活（key 用上一步返回的 plaintextKey）
grpcurl -plaintext \
  -d '{"key":"AV1D-XXXXX-XXXXX-XXXXX-XXXXX","fingerprintId":"v1.<64hex>","deviceToken":"<uuid4>"}' \
  localhost:19096 license.v1.LicenseService/Activate

# 健康检查（gRPC 标准健康服务 + 业务 healthz）
grpc_health_probe -addr=localhost:19096
grpcurl -plaintext localhost:19096 license.v1.LicenseService/Health
```

HTTP 调用等独立网关上线后经其发出（Caddy/nginx → 网关 → 本服务，见 `deploy/Caddyfile.example`）。

## HTTP 服务如何对接本服务（独立网关开发指南）

完整的行为契约在 [docs/wire-contract.md](docs/wire-contract.md)（状态码矩阵、错误体展平、
中间件清单）；本节解决"你的 HTTP 服务怎么把业务跑起来"。三种对接方式按耦合度排列：

| 方式 | 适用场景 | 要点 |
|---|---|---|
| **A. gRPC 客户端**（推荐） | 网关是独立进程/独立仓库 | `pkg.NewClient` 一行拨号，返回同时内嵌 `LicenseServiceClient` 与 `LicenseAdminServiceClient` 的客户端 |
| B. in-process module | 网关也是 Go，想省一跳网络 | `pkg.NewModule` 直接拿 Handler 同进程调用，资源由网关注入并持有生命周期 |
| C. 其他语言 stub | 非 Go 网关 | 用 buf 从契约仓库 `../api` 的 license/v1 生成对应语言的 gRPC stub（api 仓库的 buf.gen.<lang>.yaml） |

### 方式 A：gRPC 客户端

```go
import (
    licpkg "github.com/servekit/license-service/pkg"
)

client, err := licpkg.NewClient("127.0.0.1:19096") // 内网直连；生产建议 mTLS/网络隔离
if err != nil { ... }
defer client.Close()
```

### 端点 → RPC 映射（四个客户端端点）

**`POST /v1/activate` → `client.Activate`**（激活/心跳/重绑/刷新/evict 重试共用，全部幂等）：

```go
var body struct {
    Key              string `json:"key"`         // 客户端实发字段
    LicenseKey       string `json:"licenseKey"`  // 文档兼容字段；与 key 取先非空者
    FingerprintID    string `json:"fingerprintId"`
    DeviceToken      string `json:"deviceToken"`
    EvictDeviceToken string `json:"evictDeviceToken"` // 仅 409 后重试携带
}
// ... 解码 body（4 KiB 上限）...

req := &licensev1.ActivateRequest{
    Key:           firstNonEmpty(body.Key, body.LicenseKey),
    FingerprintId: body.FingerprintID,
    DeviceToken:   body.DeviceToken,
}
if body.EvictDeviceToken != "" { // proto3 optional 字段：不设即不校验
    req.EvictDeviceToken = &body.EvictDeviceToken
}

resp, err := client.Activate(ctx, req) // ctx 带 15s 超时（客户端不重试）
```

响应 JSON：

```jsonc
{
  "payload":   resp.GetPayload(),    // ⚠️ 被签名的 canonical JSON 字符串——原样透传，
                                      //    绝不能解析后重序列化（客户端会逐字节互证）
  "signature": resp.GetSignature(),  // base64 Ed25519 分离式签名
  "slots": { "used": 2, "max": 3, "devices": [ /* 见 deviceInfos */ ] }
}
```

**`POST /v1/deactivate` → `client.Deactivate`**：请求 `{key|licenseKey, deviceToken}`；
恒 200，`released:false` 表示本来就不在槽（key 查无也一样，不要再翻译成 404）。

**`POST /v1/trial/start` → `client.TrialStart`**：请求
`{module, fingerprintId, deviceToken, licenseKey?}`——注意 `module` 是 **string**
（`"downloads"` / `"tools"`，即客户端 wire 名），直接透传即可：

```go
resp, err := client.TrialStart(ctx, &licensev1.TrialStartRequest{
    Module:        body.Module, // "downloads" | "tools"
    FingerprintId: body.FingerprintID,
    DeviceToken:   body.DeviceToken,
    LicenseKey:    body.LicenseKey, // 可选，为空则 keyless
})
// 响应: {payload, signature, alreadyStarted}
```

**`GET /healthz` → `client.Health`**：`{status:"ok", checks:{db,signing}}`；
RPC 返回 Unavailable 时映射 503（见下）。

### 错误翻译（客户端硬契约的核心）

服务端错误经 gRPC 返回：**code 即 HTTP 状态**，message 恒为 `"REASON: message"`，
details 里带结构化信息。网关的翻译逻辑全文如下（可直接抄）：

```go
import (
    "google.golang.org/grpc/codes"
    "google.golang.org/grpc/status"

    licensev1 "github.com/servekit/license-service/gen/license/v1"
)

var grpcToHTTP = map[codes.Code]int{
    codes.InvalidArgument:   400, // bad_key_format / already_entitled / bad_request
    codes.Unauthenticated:   401, // key_not_found
    codes.PermissionDenied:  403, // key_revoked
    codes.AlreadyExists:     409, // slot_limit
    codes.ResourceExhausted: 429, // rate_limited
    codes.Unavailable:       503, // healthz 失败
}

func writeError(w http.ResponseWriter, err error) {
    st := status.Convert(err)
    httpCode := grpcToHTTP[st.Code()]
    if httpCode == 0 {
        httpCode = 500 // 未知 code 一律 5xx（客户端按 NETWORK_ERROR 处理）
    }

    // 5xx 不泄漏内部细节；4xx 透传 "REASON: message"
    body := map[string]any{"code": "internal", "message": "internal server error"}
    if httpCode < 500 {
        if reason, msg, ok := strings.Cut(st.Message(), ": "); ok {
            body["code"] = strings.ToLower(reason)
            body["message"] = msg // 中文文案，客户端直接展示
        }
    }

    for _, d := range st.Details() {
        switch v := d.(type) {
        case *licensev1.SlotLimitInfo: // 409 设备选择器数据，展平进错误体
            body["maxSlots"] = v.GetMaxSlots()
            body["devices"] = deviceInfos(v.GetDevices())
        case *licensev1.RetryAfterInfo: // 429 → Retry-After 头
            w.Header().Set("Retry-After", strconv.Itoa(int(v.GetSeconds())))
        }
    }

    w.WriteHeader(httpCode)
    _ = json.NewEncoder(w).Encode(map[string]any{"error": body})
}

func deviceInfos(ds []*licensev1.DeviceSlotInfo) []map[string]any {
    out := make([]map[string]any, 0, len(ds))
    for _, d := range ds {
        out = append(out, map[string]any{
            "deviceToken":   d.GetDeviceToken(),
            "fingerprintId": d.GetFingerprintId(),
            "firstSeenAt":   d.GetFirstSeenAt().AsTime().UTC().Format(time.RFC3339),
            "lastSeenAt":    d.GetLastSeenAt().AsTime().UTC().Format(time.RFC3339),
            "name":          nil, // 预留字段，服务端恒不设
        })
    }
    return out
}
```

效果示例（409）：

```json
{
  "error": {
    "code": "slot_limit",
    "message": "all device slots are in use, evict one and retry",
    "maxSlots": 3,
    "devices": [ { "deviceToken": "…", "fingerprintId": "…", "firstSeenAt": "…", "lastSeenAt": "…", "name": null } ]
  }
}
```

### admin 面（如网关决定暴露 /v1/admin/*）

admin RPC 走同一个 client，只需附加 Bearer metadata（服务端有 fail-closed 的
拦截器校验；部署层还应保证 admin 路径不出公网）：

```go
actx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+adminToken)

created, err := client.CreateKey(actx, &licensev1.CreateKeyRequest{
    Label: "order-42",
    Grants: []*licensev1.EntitlementInput{{
        Module: licensev1.Module_MODULE_DOWNLOADS,          // 枚举字段用枚举名
        Kind:   licensev1.EntitlementKind_ENTITLEMENT_KIND_PERPETUAL,
    }},
})
// created.GetPlaintextKey() 是明文 key——仅此一次出现，展示后不得落日志/存储
```

### 方式 B：in-process module（Go 网关省一跳）

```go
import (
    "github.com/servekit/license-service/pkg"
    "github.com/servekit/license-service/pkg/config"
    "github.com/servekit/license-service/pkg/option"
)

// cfg 需要 Signing.Seed（必填）等；DB/Redis 可复用网关自己的连接注入，
// 注入的资源由网关持有生命周期，module 不会关它们。
hdl, err := pkg.NewModule(cfg,
    option.WithDB(gatewayDB),
    option.WithRedis(gatewayRDB),
)
// hdl 同时实现两个 ServiceServer，直接 hdl.Activate(ctx, req) 同进程调用
```

### 网关自身的中间件清单（必须项）

1. `X-Request-Id`：入站缺失则生成，响应回带（所有响应）。
2. 请求体上限 4 KiB → 413（`http.MaxBytesReader`；JSON 错误体同上面的形状）。
3. **不开 CORS**（Tauri 原生层请求）。
4. 访问日志红线：明文 key / fingerprint_id / remote_addr / payload / signature
   绝不进日志——排障字段用 licenseId + device_token + error.code。

## 运维要点（摘要，详见 docs/design.md §9/§13）

- **备份**：每日 `pg_dump` 快照，保留 14 份滚动 + 异机拷贝；备份含 key_hash 与
  entitlements（收入数据），按敏感文件管理（0600）。
- **多钥与轮换**：`signing.keys` 是扁平列表——单钥就是一项，多钥（轮换/按构建
  分片）就配多项；`LICENSE_SIGNING_SIGN_KEY_ID` 选当前签发钥（单钥可省，多钥必填）。
  每张凭证恒带 `signingKeyId`，客户端公钥表查不到即拒收（fail-closed）；切钥前
  客户端表里必须已有该 kid（`ShowPubKey` 列出全部公钥供钉入）。泄露事故 runbook
  见 design §9.3（客户端换钉死公钥才真正止血）。
- **日志红线**：明文 key / fingerprint_id / remote_addr / payload / signature
  绝不进日志；排障用 licenseId + device_token + certId。
- admin 审计：所有变更 RPC 输出 `admin_audit` 结构化日志（op/target/reason）。

> 架构规范（分层、枚举、thirdcall、lifecycle 等）见 `CLAUDE.md`。
