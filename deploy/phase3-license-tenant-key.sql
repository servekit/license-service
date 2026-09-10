-- phase3-license-tenant-key.sql：③期 T7 —— license-service tenant_key 列化
-- 在 license-service 的 PG 上执行一次（幂等：重复执行无害）。
--
-- 步骤（D-③4 顺序）：加列 → 按映射回填 → 建新索引（名字与 GORM 模型 tag 一致，
-- AutoMigrate 不会重复建）→ 对账（回填行数达标，不等即中止）。
-- license_apps 是唯一被触碰的表：LicenseApp 在本服务纯属门卫（Activate/
-- Deactivate/TrialStart 校验调用方），license keys/devices/trials/entitlements
-- 本身全局（无 app/tenant 维度），门卫行即唯一的每租户工件 —— 因此没有旧组合
-- 唯一索引需要删除，也没有其余表需要回填。`make migrate`（pkg/handler/migrate.go
-- 的 postMigrateTenantKey）内嵌同流程。
--
-- 语义：
--   license_apps   tenant_key = app_key 字面量（③期映射即 app_key；T10 总装把
--                  存量行重映射到 ten_*）。唯一索引 → 一个 tenant 一行门卫行
--                  （Trusted 首见懒建 + 存量映射共用该唯一性）。
--
-- 对账口径（控制器输入）：执行前记录 SELECT count(*) FROM license_apps 基准；
-- apps：count(tenant_key) 必须 = count(*)。不满足即 RAISE EXCEPTION 中止整个
-- 事务（可排查后安全重试）。
--
-- 执行：psql "postgres://…/testkit" -v ON_ERROR_STOP=1 -f phase3-license-tenant-key.sql
-- dry-run：将末尾 COMMIT 改为 ROLLBACK。
--
-- 警告：compose 栈若配置表前缀（LICENSE_SERVICE_DB_TABLE_PREFIX），裸表名解析不到
--       目标 —— 执行前先确认 search_path 下的 license_* 就是目标表
--       （dev testkit 栈为无前缀裸表名）。

BEGIN;

-- ── 1. 加列（幂等）────────────────────────────────────────────
ALTER TABLE license_apps ADD COLUMN IF NOT EXISTS tenant_key VARCHAR(16);

-- ── 2. 回填（幂等：仅回填 NULL 行）────────────────────────────
-- apps：映射列 = app_key 字面量（③期目录键即 app_key）。
UPDATE license_apps SET tenant_key = app_key WHERE tenant_key IS NULL;

-- ── 3. 建新索引（名字与 GORM 模型 tag 一致）────────────────────
-- apps：一个 tenant 一行门卫行（Trusted 懒建 + 存量映射共用该唯一性）。
CREATE UNIQUE INDEX IF NOT EXISTS uq_license_apps_tenant_key ON license_apps(tenant_key);

-- ── 4. 对账：不等即中止（可排查后重跑）──────────────────────────
DO $$
DECLARE
  total bigint;
  filled bigint;
BEGIN
  EXECUTE 'SELECT count(*), count(tenant_key) FROM license_apps' INTO total, filled;
  RAISE NOTICE 'phase3 license tenant_key reconcile license_apps: total=% filled=%', total, filled;
  IF filled <> total THEN
    RAISE EXCEPTION 'phase3 license_apps backfill incomplete: % of % rows filled', filled, total;
  END IF;
END $$;

COMMIT;
