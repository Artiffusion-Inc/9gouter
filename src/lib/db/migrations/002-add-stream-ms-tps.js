// Add streamMs (ms) and tps (tokens/sec) columns to usageHistory for streaming
// generation throughput. Partial index excludes NULL rows (non-stream / errors).
export default {
  version: 2,
  name: "add-stream-ms-tps",
  up(db) {
    db.exec(`ALTER TABLE usageHistory ADD COLUMN streamMs INTEGER`);
    db.exec(`ALTER TABLE usageHistory ADD COLUMN tps REAL`);
    db.exec(`CREATE INDEX IF NOT EXISTS idx_uh_tps ON usageHistory(tps) WHERE tps IS NOT NULL`);
  },
};
