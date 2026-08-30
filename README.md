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
make regenerate  # = proto + generate + tidy（改 proto/model 后跑）
make migrate     # 执行数据库迁移
make test        # 测试（race + coverage；DB 测试需 Docker 起 testcontainer）
make lint        # golangci-lint
make docker-up   # 起完整 docker 栈（license + postgres + redis）
```

gRPC 监听 `:19096`（servekit 序列的下一个槽位）。HTTP 面本期不存在：
`server.http_addr` 默认空 = 不启网关；`:18086` 为未来独立网关预留。

## 配置

`config.example.yaml` 是**纯结构**——每个值都是 `${VAR}` 占位符，由 configx `WithExpandEnv` 从进程环境展开。`.env.example` 是 **docker-compose 取向**的默认值源。

**必填项：**

- `LICENSE_SIGNING_SEED` — 64 hex Ed25519 seed（`openssl rand -hex 32` 生成）。
  **丢失 = 全部已发凭证无法续签**，请保留离线副本；不入库、不入 git、不进日志。
- `ADMIN_TOKEN` — LicenseAdminService 的 Bearer token（`openssl rand -hex 32`）。
  为空时全部 admin RPC 拒绝（fail-closed）。

**本地跑（`make run`）：**

```bash
cp .env.example .env
# 编辑 .env：LICENSE_SIGNING_SEED / ADMIN_TOKEN 填值；
# LICENSE_SERVICE_DATABASE_HOST: postgres -> localhost
make run            # 需要本机 PostgreSQL + Redis
```

**docker compose 跑（`make docker-up`）：** 无需改 host 名——compose 注入全部 env。敏感项照旧走 `.env`。受限网络在 `.env` 加 `GOPROXY=https://goproxy.cn,direct`。

## 测试调用（gRPC）

```bash
# 发一把 key（admin，需 Bearer；明文 key 仅此一次出现）
grpcurl -plaintext -H "authorization: Bearer $ADMIN_TOKEN" \
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

HTTP 调用等独立网关上线后经其发出（Caddy → 网关 → 本服务，见 `deploy/Caddyfile.example`）。

## 运维要点（摘要，详见 docs/design.md §9/§13）

- **备份**：每日 `pg_dump` 快照，保留 14 份滚动 + 异机拷贝；备份含 key_hash 与
  entitlements（收入数据），按敏感文件管理（0600）。
- **密钥轮换**：`LICENSE_SIGNING_SEED_SECONDARY` + `LICENSE_SIGNING_KEY_ID`
  过渡期双钥；泄露事故 runbook 见 design §9.3（客户端换钉死公钥才真正止血）。
- **日志红线**：明文 key / fingerprint_id / remote_addr / payload / signature
  绝不进日志；排障用 licenseId + device_token + certId。
- admin 审计：所有变更 RPC 输出 `admin_audit` 结构化日志（op/target/reason）。

> 架构规范（分层、枚举、thirdcall、lifecycle 等）见 `CLAUDE.md`。
