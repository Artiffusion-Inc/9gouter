// Add streamMs (ms) and tps (tokens/sec) columns to usageHistory for streaming
// generation throughput. Partial index excludes NULL rows (non-stream / errors).
//
// Idempotency: on fresh DBs, 001-initial.js creates `usageHistory` from the
// declarative TABLES in schema.js (which already includes these columns since
// Task 2), so the ALTER would fail with "duplicate column name". Guard with
// PRAGMA table_info; partial index uses IF NOT EXISTS.
export default {
  version: 2,
  name: "add-stream-ms-tps",
  up(db) {
    const cols = db.all(`PRAGMA table_info(usageHistory)`).map((r) => r.name);
    if (!cols.includes("streamMs")) {
      db.exec(`ALTER TABLE usageHistory ADD COLUMN streamMs INTEGER`);
    }
    if (!cols.includes("tps")) {
      db.exec(`ALTER TABLE usageHistory ADD COLUMN tps REAL`);
    }
    db.exec(`CREATE INDEX IF NOT EXISTS idx_uh_tps ON usageHistory(tps) WHERE tps IS NOT NULL`);
  },
};
