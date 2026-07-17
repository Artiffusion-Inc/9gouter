-- Add streamMs (ms) and tps (tokens/sec) columns to usageHistory for streaming
-- generation throughput. The partial index excludes NULL rows (non-stream/errors).
-- On fresh DBs the initial migration already creates these columns, so the
-- ALTER statements are guarded by the migrations version table. The partial
-- index is created with IF NOT EXISTS.

ALTER TABLE usageHistory ADD COLUMN streamMs INTEGER;
ALTER TABLE usageHistory ADD COLUMN tps REAL;
CREATE INDEX IF NOT EXISTS idx_uh_tps ON usageHistory(tps) WHERE tps IS NOT NULL;
