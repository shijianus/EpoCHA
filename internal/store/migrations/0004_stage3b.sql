-- 0004_stage3b.sql：阶段 3B 的图片资产列（DESIGN §5.5，ADR-012）。
--
-- 只增不减的增量迁移：阶段 1/2/3A 的数据全部保留；旧行 asset_blob 为 NULL、asset_mime 为 ''，
-- 因此 arithmetic-baseline-v1 与 pow_only 两条路径的行为完全不变。
--
-- 图片为什么放在 SQLite BLOB 而不是文件/ObjectStore：
--   1. 生命周期与题目行**天然同生同死**——清理任务删掉 pool 行，资产一起消失，不存在"孤儿文件"要扫；
--   2. 单一权威存储（ADR-001/D2 的故障域纪律），备份与恢复不新增一套东西；
--   3. 领取与判分都已经在同一个库里，多一个存储就多一个一致性证明。
-- 代价（如实记录）：库体积随图片增长，因此**字节预算与行数上限是两个独立的量**，
-- 判定都必须在写入的同一条 SQL 内完成（与 0003 的行数上限同一手法）。

-- 单题 PNG 字节上限 65536：与 Go 侧 store.MaxAssetBytes 必须一致，有测试钉住（越界写入必须被拒）。
ALTER TABLE pool ADD COLUMN asset_blob BLOB CHECK (asset_blob IS NULL OR length(asset_blob) <= 65536);

-- 只有 image/png 一种媒体形态。把 MIME 写进库里（而不是由生成器当前代码推断）是为了：
-- 资产自描述，且已签发题目不会因为后来改了生成器而换掉类型声明。CHECK 把可选值封成一项，
-- 浏览器拿不到 data:<任意 MIME> 的空间。
ALTER TABLE pool ADD COLUMN asset_mime TEXT NOT NULL DEFAULT '' CHECK (asset_mime IN ('', 'image/png'));

-- 领取查询的形态不变（仍是 generator_id/version/difficulty/status/expires_at），
-- 这里不新增索引：字节统计走聚合函数，不产生按资产字节的检索需求。
