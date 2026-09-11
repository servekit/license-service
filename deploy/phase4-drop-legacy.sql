-- phase4-drop-legacy.sql：④期 T8 —— 窗口关闭（数据侧）：license-service
-- 在 license-service 的 PG 上执行一次（幂等：重复执行无害）。
--
-- 前置（控制器输入，全部满足才执行本脚本）：
--   1. 代码对账：T7/T8 后代码已无 app 表 app_secret 的读写
--      （grep 列名，仅历史注释提及）；
--   2. pg_dump 留档：相关表已备份到 deploy/backups/；
--   3. 行数记录：删前 SELECT count(*) 基准已记录。
--
-- 删除清单（spec §9.1.3：凭据列废弃不迁）：
--   license_apps  app_secret 列（表保留 —— 租户 gate 行/激活门载体；
--                 license key/device 本就是全局凭据，不受影响）
--
-- 对账口径：count(tenant_key) 必须 = count(*)（③ 回填完备）；不等即
-- RAISE EXCEPTION 中止整个事务。
--
-- 备份（执行前手工跑一次，产物不进 git）：
--   pg_dump "postgres://…/testkit" -t license_apps \
--     -Fc -f deploy/backups/phase4-license-pre-drop.dump
--
-- 执行：psql "postgres://…/testkit" -v ON_ERROR_STOP=1 -f phase4-drop-legacy.sql
-- dry-run：将末尾 COMMIT 改为 ROLLBACK。
-- make migrate 的镜像步骤（postMigrateDropLegacy）做同一件事，先跑哪个
-- 都可以。

BEGIN;

-- ── 1. 对账：tenant_key 回填完备才能删凭据列────────────────────
DO $$
DECLARE
  total bigint;
  filled bigint;
BEGIN
  EXECUTE 'SELECT count(*), count(tenant_key) FROM license_apps' INTO total, filled;
  RAISE NOTICE 'phase4 drop-legacy reconcile license_apps: total=% tenant_key_filled=%', total, filled;
  IF filled <> total THEN
    RAISE EXCEPTION 'phase4 drop-legacy: license_apps has % of % rows without tenant_key; refusing to drop app_secret', total - filled, total;
  END IF;
END $$;

-- ── 2. 删列（幂等）─────────────────────────────────────────────
ALTER TABLE license_apps DROP COLUMN IF EXISTS app_secret;

COMMIT;
