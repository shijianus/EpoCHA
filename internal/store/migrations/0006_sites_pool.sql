-- 0006_sites_pool.sql：站点级生成器池（ADR-026）。
--
-- generator_id 保留为兼容/默认题型；generator_pool 为空串表示"仍只用 generator_id"。
-- 非空时是 JSON 字符串数组，由 Go 侧校验（最多 8 个、去重、每个 1..64 字符）。
-- 不在 SQL 里解析 JSON：SQLite 的 JSON 函数不是本项目其他地方依赖的契约，
-- 放进 CHECK 会让迁移与 Go 校验出现两套规则。
ALTER TABLE sites ADD COLUMN generator_pool TEXT NOT NULL DEFAULT '';
