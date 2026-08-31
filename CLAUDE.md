# CLAUDE.md — license-service

## 项目定位

License service — 基于 [go-common](https://github.com/servekit/go-common) 的**纯 gRPC** 微服务（客户端 HTTP 面由将来的独立网关提供，见 `docs/wire-contract.md`；`:18086` 为其预留）。
遵循 servekit `-service` 架构（`pkg/internal/cmd/api/gen` 分层、grpcx、`lifecycle.Manager`）。

## 架构铁律（写代码前必读）

完整规范在 **dev-skills** 仓库的 `golang-service-development` skill（入口 `SKILL.md`，子文档 `architecture.md` / `enum.md` / `jobs.md` / `scaffold.md` 按需加载）。以下是最高频被违反的硬规则：

- **handler 不写业务**：`pkg/handler/license.go` 里每个 RPC 委托给 `h.svc.X(ctx, req)`，业务逻辑归 `internal/service`。不做业务、不做协议转换；横切关注点（日志、metrics、加载与业务无关的资源）允许。
- **service 直接吃 proto**：业务方法接受/返回 proto 类型，proto↔model 转换发生在 store 边界。禁止 handler↔service 之间造中间 struct。
- **一个领域 = 一个子包**：业务在 `internal/service/<domain>/`；`internal/service/service.go` 只放本体 + facade，不写业务，根目录没有 `<domain>.go` 单文件。
- **枚举优先 proto**：枚举在 proto 里定义，DB 存 `int32`，用 proto 内置方法转换（`int32(x)` / `licensev1.X(x)` / `.String()` / `_name` / `_value` map），**不要自己写 helper**（边界用例见 enum.md）。
- **第三方调用（接口 internal）**：接口在 `internal/thirdcall/<name>/`（如 `gid_service`），不在 `pkg/`——**没有 `pkg/thirdcall/`**；`option` 注入 raw `*Handler`（`WithGIDHandler`），service 内部 wrap 成内部接口；切 grpc/module 只改 config（`third_party.<name>.mode`）。
- **资源用 lifecycle.Manager**：不要 `ownX bool`；注入的资源（`WithX`）不注册，自建的注册到 mgr（close-only 资源用 Stopper；有 Start 的下游 Handler 用 `mgr.Add`）。
- **加 RPC 四步**：proto 加方法 → `make proto` → handler 加委托 → `service.go` 加 facade → 子包加业务。
- **scaffold 是 one-shot**：本服务已生成，后续演进手写，**绝不重跑** `new-service.sh`（会覆盖丢代码）。

## 增量能力必看范例（强制）

给本服务**后加**任何资源能力（DB / Redis / 第三方调用 / 消息队列 / 其他 go-common 资源）时，**必须**照抄 demo-service 的 `resolveXxx` 实现 + `architecture.md` 的「用 lifecycle.Manager 而不是 ownX bool」段——**不许**自己发明集成方式：

- **注入的资源**（`option.WithX`）**不注册**到 mgr，调用方拥有生命周期；
- **自建的**注册到 mgr：close-only 资源用 `mgr.AddStopper(name, lifecycle.StopFunc(...))`，自带 Start+Stop 的下游 Handler 用 `mgr.Add(name, hdl)`；cleanup 错误用 `slog.Warn`（不要自造 closer 类型、不要在 service 里返回 cleanup error）；
- 在 `internal/service/service.go` 的 `New()` 里 resolve（仿 `resolveDB` / `resolveRedis`），失败回滚 `mgr.Stop()`。

scaffold 已按生成时的能力开关接好；这里说的是**生成之后**新加能力。

## 技术栈约定

### gRPC / Proto
- Proto 在 `api/proto/license/v1/`（初始版本；破坏性变更开新版本目录 `v2/`、`v3/`，不就地改老版本）
- buf v2 配置在 `buf.yaml` / `buf.gen.yaml`
- 生成代码到 `gen/`（committed），由 `make proto` 生成
- Swagger 2.0 文档生成到 `api/swagger/`（committed，供前端/客户端消费），同样由 `make proto` 产出——openapiv2 插件从 proto 的 `google.api.http` 注解派生

### 数据库 / GORM
- PostgreSQL（通过 `dbx.New`）
- `internal/store/{models,generated,dal}` 遵循 `gorm-cli-development` skill
- 迁移：GORM AutoMigrate，统一入口 `pkg/handler.Migrate`（pkg 顶层 re-export 为 `pkg.Migrate`）。`cmd/server migrate` 子命令与嵌入模块（`pkg.NewModule` + `option.WithDB`，先 `pkg.Migrate(parentDB)` 再构造）都走它，确保 standalone / in-process 两种部署都能建表

### 错误处理
- 错误码在 `pkg/xcodes/license.go`，按域分文件
- 业务错误用 `xcodes.ErrLicenseXxx.Wrap(err)` / `.New()` 包装

### 基础库
- 全部 `github.com/servekit/go-common/*`，API 参考 go-common 仓库 README

## 运行模式

1. **standalone gRPC**: `make run` → listen :19096
2. **HTTP 面**: 本期不启（`server.http_addr` 默认空；grpcx 的 `registerGW` 为 nil）。将来由独立网关承接，proto 的 `google.api.http` 注解就是它的路由契约
3. **in-process module**: 其它服务 `import "github.com/servekit/license-service/pkg"` → `pkg.NewModule(cfg, opts...)`
4. **Docker**: `make docker-up` —— 用 `Dockerfile` + `docker-compose.yaml` 起完整栈（license + postgres + redis，跑 grpc healthcheck）；`make docker-down` 停。Docker 产物由 `golang-service-docker` skill 生成。

## 本服务专属铁律（license 契约，违反即客户端故障）

- **gRPC 状态码即错误契约**：六个业务错误码精确映射（InvalidArgument/Unauthenticated/PermissionDenied/AlreadyExists/ResourceExhausted），未来网关按默认映射恰好得到 400/401/403/409/429。改 `pkg/xcodes/license.go` 的 category = 改客户端行为。
- **凭证 payload 必须 canonical**：`internal/service/cert` 的序列化器是被客户端逐字节互证的（golden 向量锁死）；任何字段增删都要跑 golden。
- **永不复用 issuedAt**：每次响应用当前时间重签、certId 新 UUID——客户端单调水位依赖此性质。
- **perpetual `expiresAt` 恒 null、subscription/trial 恒非 null**：双侧（admin 授予 + activation）都要守住。
- **限流是防滥用配额，不是精确节拍**：go-common `ratelimit` 固定窗口，每 (身份, device_token) 每窗口 `rate_limit.max` 次（默认 10/60s）。409 消耗配额是**有意的**——配额已覆盖 evict 重试；不要重新引入"60s 一次 + 退还"的单发语义（易误杀合法心跳）。
- **试用必须并入凭证**：key 凭证与试用凭证在客户端单槽存储下会互相冲掉——`signCert` 的并集逻辑不能拆。
- **日志脱敏**：明文 key / fingerprint_id / remote_addr / payload / signature 绝不进日志；排障用 licenseId + device_token + certId。trial 审计目标用 `sha256(fp)[:16]`。
- **DB 无软删除是有意的**（spec §4 硬行语义：槽位释放/试用重置/upsert 唯一性），不要"补上" DeletedAt；keys 的吊销是 Status 软状态。
- **表前缀在 struct 名**（LicenseKey → license_keys），不要加 dbx table_prefix。
- **签名钥是扁平列表，凭证恒带 kid**：`signing.keys[]`（无默认钥特例），每张凭证的 payload 都带 `signingKeyId`，客户端公钥表 fail-closed。不要恢复"默认钥/kid=null"路径。
- **配置默认值只在 configx `default:` 标签**：代码不做兜底，不完整配置启动报错（resolveSigner / resolveDomainOptions）。
- **admin 面无服务端鉴权（有意）**：授权归边缘（网关 + 用户/权限系统）；不要重新引入静态 token。

## 常用命令

```bash
# 本地开发
make run         # 启动（auto-cp config.example.yaml -> config.yaml）
make test        # 测试（race + coverage）
make lint        # golangci-lint
make fmt         # gofmt + goimports

# 代码生成（改 proto / model 后跑）
make regenerate  # = proto + generate + tidy

# 数据库迁移
make migrate     # AutoMigrate（本地 PostgreSQL）

# Docker（由 golang-service-docker 生成）
make docker-up       # build + 起完整栈 + 等 healthcheck
make docker-migrate  # 一次性迁移（对 compose DB 跑 migrate 后退出）
make docker-down     # 停（保留 volume）
make docker-logs     # 跟随日志（或 svc=<name>）
```

完整 target（含 `docker-push` / `docker-reset` / `docker-health` / `build` / `vet` 等）见 `Makefile`；docker target 语义见 `golang-service-docker` skill。
