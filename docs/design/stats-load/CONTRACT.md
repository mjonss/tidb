# Stats Load Subsystem Contract

This document records the contract of the TiDB stats-load subsystem as it currently exists in code. Each clause is derived from the current source and cites a file:line in this worktree. PR history is not an input: where a PR established a clause, the citation is the code that survives today.

Status of sections:

- Sync load: drafted (this commit).
- Init stats: pending.
- Pseudo-stats fallback: pending.
- Visible StatsCache interface: pending.
- Delta-collector freshness pipeline: pending.

Cross-cutting open questions are in `CURRENT-USAGE.md` and remain unresolved.

---

## Pseudo-Stats Fallback

The pseudo-stats fallback covers the cases where the planner needs to estimate something but real stats are absent, stale, or definitionally unavailable. It is the safety net that keeps queries successful when the load contract cannot deliver.

Implementation: `pkg/statistics/table.go:1000-1063` (`PseudoTable`, `PseudoHistColl`, `PseudoVersion`), `pkg/statistics/column.go:248-256` (`EmptyColumn`), `pkg/planner/core/stats/stats.go:64-151` (planner pseudo decisions), `pkg/statistics/handle/storage/gc.go:135-...` (DROP STATS soft delete).

### Three pseudo flavors

The contract distinguishes three forms of pseudo:

1. **Full pseudo `*statistics.Table`** via `PseudoTable(tblInfo, allowTriggerLoading, allowFillHistMeta)` (`table.go:1021`). Used when the planner needs a complete table object: `ColAndIdxExistenceMap` with "not analyzed" entries for every public+non-hidden column / index, `Version = PseudoVersion = 0`, `HistColl.Pseudo = true`, `RealtimeCount = PseudoRowCount`. If `allowFillHistMeta=true`, the columns/indices maps are also populated with stub histograms.

2. **Lightweight pseudo `HistColl`** via `PseudoHistColl(physicalID, allowTriggerLoading)` (`table.go:1004`). Just the `HistColl` value with `Pseudo=true`; no per-column or per-index entries; no existence map. Used for cost calculations where only `RealtimeCount` and `Pseudo` matter.

3. **`EmptyColumn` tombstone** via `EmptyColumn(tid, pkIsHandle, colInfo)` (`column.go:249-256`). A column-level placeholder with `Info` set, empty `Histogram`, no TopN, no CMSketch. Installed in the stats cache by sync load when a column is not analyzed. The cache hit prevents future sync-load requests from re-firing for the same column (because `IsLoadNeeded` would return false against a present-but-empty column).

### Two coexisting pseudo signals

Code branches on pseudo via either of two equivalent (but not identical) signals:

- `*statistics.Table.Pseudo` (struct field on `HistColl`).
- `StatsVersion == PseudoVersion` (numeric, where `PseudoVersion = 0` at `table.go:36`).

`PseudoTable` sets both: `Version = PseudoVersion = 0` and `HistColl.Pseudo = true`. The planner's `property.StatsInfo` propagates the `StatsVersion` field; some sites check `StatsVersion`, others check `HistColl.Pseudo`. Open Q14 in `CURRENT-USAGE.md` flags this duplication; the contract today does NOT require the two signals to be in lockstep, only that any place that constructs a pseudo state sets both.

### `CanNotTriggerLoad` flag

`HistColl.CanNotTriggerLoad` (`table.go:1011`) is set to `!allowTriggerLoading` when the pseudo HistColl is constructed. Its purpose: prevent `ColumnStatsIsInvalid` / `IndexStatsIsInvalid` from inserting items into `AsyncLoadHistogramNeededItems` for tables that definitionally have no stats.

Contract:

1. When `allowTriggerLoading=false`, `CanNotTriggerLoad=true`, and the cardinality estimator side-effect calls are no-ops. This is the "do not waste async-load budget" gate.
2. Production callers that pass `allowTriggerLoading=false`:
   - `logical_mem_table.go:189` `PseudoTable(p.TableInfo, false, false)` for memtables.
   - `tables.go:1924` `PseudoTable(tblInfo, false, false)` for temporary tables.
   - `planbuilder.go:1803` `PseudoHistColl(physicalID, false)` for `ADMIN CHECK INDEX` plan construction.
3. When `allowTriggerLoading=true` (the planner's `stats.GetStatsTable` path), the pseudo HistColl can trigger async loads via the cardinality estimator side effects.

### When the planner constructs a pseudo state

`stats.GetStatsTable` (`pkg/planner/core/stats/stats.go:64-151`) is the entry point. It returns either the cached `*statistics.Table` or a pseudo variant. The decision tree:

1. **`StatsHandle == nil`** (no domain stats handle): return `PseudoTable(tblInfo, false, true)` (`stats.go:64`). No load triggered, but histogram-meta filled for downstream estimators.
2. **`statsTbl == nil`** (cache miss): return `PseudoTable(tblInfo, true, true)`. Load IS triggered (`allowTriggerLoading=true`) to fill the cache asynchronously.
3. **`statsTbl.IsInitialized() == false`** OR **`statsTbl.IsOutdated() == true && EnablePseudoForOutdatedStats=true`**: return a cloned table marked `Pseudo=true` (`stats.go:111-114`). Increment a metrics counter (`PseudoEstimationNotAvailable` or `PseudoEstimationOutdate`).
4. **`RealtimeCount == 0`** (table has stats meta but zero rows): return `PseudoTable(tblInfo, allowPseudoTblTriggerLoading, true)` (`stats.go:105`). Whether load is triggered depends on `allowPseudoTblTriggerLoading`, which is set true only when there is at least one analyzed column/index per `:94` check.
5. **OptObjectiveDeterminate path** (`:85`): rewrite `RealtimeCount` to `GetAnalyzeRowCount()` so the planner uses the at-analyze count rather than the live delta. May then force pseudo if the resulting count is 0.
6. **Otherwise**: return the cached `*statistics.Table` unchanged.

### `EmptyColumn` install during sync load

Inside `handleOneItemTaskWithSCtx` (`pkg/statistics/handle/syncload/stats_syncload.go:399-403`):

    if !analyzed {
        wrapper.col = statistics.EmptyColumn(item.TableID, isPkIsHandle, wrapper.colInfo)
        s.updateCachedItem(item, wrapper.col, wrapper.idx, task.Item.FullLoad)
        return nil
    }

Contract:

1. Called when `ColumnIsLoadNeeded` returns `analyzed=false` for a column. This means stats meta exists but no histogram has ever been written.
2. The cache write installs the `EmptyColumn` placeholder. Subsequent `tbl.ColumnIsLoadNeeded(...)` returns `loadNeeded=false` for this column because `GetCol(id) != nil`.
3. **The async-load side effect from cardinality estimators DOES still fire** even after `EmptyColumn` is installed: `ColumnStatsIsInvalid(colStats, ...)` checks `IsStatsInitialized()` on the column, which is false for `EmptyColumn`, so it enqueues. This is a documented Open Q16 issue: the EmptyColumn does NOT serve as a "stop trying" signal for the async queue, only for the sync path.
4. The same `EmptyColumn` helper is used by the async path (`pkg/statistics/handle/storage/read.go` `loadNeededColumnHistograms`) for symmetric behavior.

### `Column.StatsAvailable()` and the analyzed-vs-synthesized distinction

`column.go:241-246`:

    func (c *Column) StatsAvailable() bool {
        return IsColumnAnalyzedOrSynthesized(c.GetStatsVer(), c.NDV, c.NullCount)
    }

Contract:

1. A column is "stats available" if either: (a) `StatsVer >= Version1` (explicitly analyzed), or (b) `NDV > 0 || NullCount > 0` (synthesized from DDL default values when a column is added with a default).
2. The synthesized case (b) covers `ALTER TABLE ADD COLUMN ... DEFAULT ...`: the new column has no histogram but does have `NDV` and `NullCount` derived from the default value, so it counts as having usable stats.
3. `EmptyColumn` has `StatsVer=0`, `NDV=0`, `NullCount=0`, so `StatsAvailable() == false`. It is a pure placeholder.

### DROP STATS post-state

`DROP STATS` is a soft delete. Implementation: `pkg/executor/simple.go:3121` calls `h.DeleteTableStatsFromKV(statsIDs, true)` (the soft flag), routing into `pkg/statistics/handle/storage/gc.go:67-71` then `:135-...`.

The soft-delete contract (`gc.go:135-150`):

1. Get a new `startTS` from the session.
2. `UPDATE mysql.stats_meta SET version = startTS, last_stats_histograms_version = startTS WHERE table_id IN (statsIDs)`. Only the version columns are bumped; the row remains.
3. The `mysql.stats_histograms` / `stats_buckets` / `stats_top_n` rows are deleted (the actual histogram data is gone), but `mysql.stats_meta` remains.
4. Subsequent `StatsCache.Update` reads the new version, calls `TableStatsFromStorage`. Because the histogram rows are gone, `TableStatsFromStorage` produces a `*statistics.Table` with no per-column / per-index histograms but with valid `RealtimeCount` and `ModifyCount`.

Read-back semantics (per Open Q referenced in PR #59031):

- `tbl.Pseudo == false` (the meta row exists).
- `tbl.StatsVer == Version0` (no version was set during the soft delete; the previous version is overridden by the bump).
- `tbl.IsAnalyzed() == false` for all columns and indexes (no histogram data).
- `tbl.RealtimeCount` is preserved (the soft delete does not reset count).
- Auto-analyze will treat the table as never-analyzed and re-analyze on the next pass.

### Auto-GC (`GCStats`)

`gc.go:76-131` is the eventual-consistency reconciler. It runs in `Domain.gcStatsWorker` (`pkg/domain/domain.go:2205-2233`, owner-only).

Contract:

1. Operates on rows in `mysql.stats_meta` with `version IN [lastGC, lastGC + 10*max(statsLease, ddlLease))` window. The 10-lease offset ensures all TiDB nodes have observed the version before GC.
2. For each such row, calls `gcTableStats`. If the table is no longer in InfoSchema (`is.TableByID` returns false), additionally removes the historical-stats record.
3. **Hard delete path** (`soft=false`): used by auto-GC, removes the `mysql.stats_meta` row outright.
4. Old historical stats are pruned via `ClearOutdatedHistoryStats` (`:124`), bounded by `HistoricalStatsDuration` sysvar.

### When the EmptyColumn / Pseudo combination is reset

A pseudo state is not permanent. The transitions out:

1. **Analyze runs**: `mysql.stats_histograms` is populated; next `StatsCache.Update` tick refreshes the cache; `ColumnIsLoadNeeded` now returns `analyzed=true`; `EmptyColumn` is replaced by the loaded histogram on the next sync-load request.
2. **Cache cleared**: `Clear` or `Replace` wipes any `EmptyColumn` placeholders. Subsequent reads either hit pseudo (if no analyze has run) or re-load.
3. **DROP STATS**: explicitly resets the state. Cache `Update` produces a table with no per-column histograms.

### Failure model

Pseudo is not an error path; it is a designed-in degraded state. The contract for what the planner can rely on when seeing pseudo:

| Planner observation | What pseudo guarantees |
|---------------------|------------------------|
| `tbl.Pseudo == true` | `RealtimeCount` is a synthetic value (`PseudoRowCount`); column/index estimates use `pseudoEqualRate`, `pseudoLessRate`, etc. |
| `tbl.StatsVersion == PseudoVersion (0)` | Same; redundant with the boolean flag in practice. |
| `EmptyColumn` in cache | The column is not analyzed; cardinality estimators should fall through to per-row pseudo defaults; future sync-load requests for this column at the same `FullLoad` level will return immediately. |
| `IsStatsInitialized()` false | No useful per-column stats available; only schema info is reliable. |

### Resource bounds

| Bound | Source | Default |
|-------|--------|---------|
| Pseudo row count constant | `statistics.PseudoRowCount` | (fixed in code) |
| Pseudo equal-rate | `statistics.pseudoEqualRate` | (fixed) |
| GC version window | `10 * max(statsLease, ddlLease)` | 10 * 3s = 30s |
| `EnablePseudoForOutdatedStats` | sysvar | `true` |
| Soft vs hard delete | `DeleteTableStatsFromKV(_, soft)` | soft=true from DROP STATS, soft=false from auto-GC |

### Test obligations

1. `PseudoTable(_, false, _)` produces a table with `CanNotTriggerLoad=true`; cardinality side effects do not enqueue async loads.
2. `PseudoTable(_, true, _)` produces a table with `CanNotTriggerLoad=false`; side effects DO enqueue.
3. Memtables (`logical_mem_table.go`) always pseudo with no async-load enqueue.
4. Temp tables (`tables.go`) always pseudo with no async-load enqueue.
5. `EmptyColumn` installed by sync-load prevents future sync-load requests for the same column.
6. `EmptyColumn` does NOT prevent async-load side-effect enqueue from `ColumnStatsIsInvalid` (current behavior; documented as Open Q16).
7. `stats.GetStatsTable` decision tree: each of the 5 numbered branches reached by a focused test.
8. DROP STATS post-state: `Pseudo=false`, `StatsVer=0`, `IsAnalyzed()=false` per column, `RealtimeCount` preserved.
9. DROP STATS triggers a cache `Update` on the next tick; the table's histograms are gone but the meta row remains.
10. Auto-GC removes `mysql.stats_meta` rows for tables outside the 10-lease window.
11. `EnablePseudoForOutdatedStats=false`: `IsOutdated()` does NOT trigger the pseudo branch in `stats.GetStatsTable`.
12. `StatsAvailable()` returns true for analyzed columns AND for synthesized-from-default columns; false for `EmptyColumn`.
13. The two pseudo signals (`HistColl.Pseudo` and `StatsVersion == PseudoVersion`) agree at every pseudo construction site (tested by inspecting the resulting `*statistics.Table` after each `PseudoTable` call).

### Known cross-cutting issues

- Q14 (two pseudo signals): `HistColl.Pseudo` vs `StatsVersion == PseudoVersion` used inconsistently in callers.
- Q16 (self-enqueueing): `EmptyColumn` does not stop async-load side effects.
- DROP STATS soft delete behavior depends on the cache `Update` cycle to refresh; race conditions during the gap are not specified.

## Delta-Collector Freshness Pipeline

The delta-collector is the "data has changed since last analyze" signal pipeline. It feeds into the stats cache via `mysql.stats_meta.modify_count`, `count`, and `version` columns. The pipeline does NOT load histograms; it only updates the meta row. The cache's `Update` call (see "Visible StatsCache Interface" -> `Update`) is the consumer.

Implementation: `pkg/statistics/handle/usage/session_stats_collect.go`.

### The chain

    1. DML in a session  -> SessionStatsItem.Update(tableID, delta, count)
    2. Session commits   -> delta stays in the session-local mapper (SessionStatsItem.mapper)
    3. DumpStatsDeltaToKV ticks (timer in Domain.deltaUpdateTickerWorker, statsLease interval)
       -> SweepSessionStatsList merges all sessions' mappers into the global SessionTableDelta
       -> GetDeltaAndReset extracts the merged map; pending merges go back on the deferred call (`:113-115`)
       -> needDumpStatsDelta filters per table (see "Dump-trigger conditions")
       -> dumpStatsDeltaToKV (private) writes mysql.stats_meta in batches of 100_000 with statsVersion = txn.startTS
       -> deltaMap entries are removed for successful writes
       -> RecordHistoricalStatsMeta for unlocked tables
    4. StatsCache.Update ticks (Domain.loadStatsWorker, statsLease interval)
       -> reads mysql.stats_meta rows with version > checkpoint
       -> refreshes count/modify_count in the cache (meta-only path) or reloads histograms
    5. Planner reads updated count/modify_count from cache via GetPhysicalTableStats

The end-to-end staleness bound is: `dumpTicker interval + statsLease + LFU admission delay`, plus the 5-lease `LeaseOffset` window the cache's `Update` consults.

### Per-session accumulation

`SessionStatsItem` (`session_stats_collect.go:445-453`) is one node in a global linked list owned by `SessionStatsList`. Each session attaches its own item via `NewSessionStatsItem` at session creation (`pkg/session/session.go:4042-4045`).

Contract:

1. `SessionStatsItem.Update(id, delta, count)` (`:463-467`) is per-session, mutex-protected, and is the only writer. Concurrent sessions never share an item; one item per session.
2. `SessionStatsItem.Delete()` (`:456-460`) marks the item for removal. The actual unlink happens in the next `DumpStatsDeltaToKV` sweep. Sessions are released back to the pool after `Delete`, but their unfreed deltas survive until the next sweep merges them.
3. `SessionStatsItem.UpdateColStatsUsage(colItems, updateTime)` (`:480-484`) is the predicate-column usage flush channel; tracked separately from delta but in the same sweep.

`SessionStatsList` (`:503-513`):

- `tableDelta`: global `TableDeltaMap` aggregating across all sessions.
- `statsUsage`: global `StatsUsage` for predicate columns.
- `listHead`: linked-list head; new sessions prepend.

`SweepSessionStatsList` (called inside `DumpStatsDeltaToKV` at `:111`) walks the list, calls `merge(item, &tableDelta, &statsUsage)` for each (which itself drains the session's mappers), and unlinks items marked `deleted=true`.

### `DumpStatsDeltaToKV(forceDump bool, tableIDs ...int64) error`

Defined at `session_stats_collect.go:103-218`. The single entry point for flushing accumulated delta to KV.

Contract:

1. **Sweep precedes any work** (`:111`). All session-local deltas are merged into the global map before filtering.
2. **The deferred `Merge(deltaMap)` at `:113-115`** ensures that tables left in the map (failed dump, not eligible) are returned to the global aggregator for the next attempt. This is the "no loss" guarantee.
3. **Selection of table IDs** (`:123`):
   - Empty `tableIDs`: dump every table in `deltaMap`. Sorted lexicographically before dispatch (`:227`).
   - Non-empty: intersect with `deltaMap` keys, deduplicate, sort.
4. **Batching at `dumpDeltaBatchSize = 100_000`** (`:95, :126-129`). Each batch runs in its own transaction.
5. **Inside each batch transaction** (`:135-194`):
   - Get `is := sctx.GetLatestInfoSchema()`. The whole batch sees one InfoSchema snapshot.
   - For each table ID, initialize `InitTime` if zero (`:142-145`) so the "1h max" check works deterministically.
   - Call `needDumpStatsDelta(is, forceDump, id, item, batchStart)` (see below). Skip if `false`.
   - Build `*storage.DeltaUpdate` and append to `batchUpdates`.
   - Empty batch: return nil (`:158-160`).
   - Non-empty: call `s.dumpStatsDeltaToKV(is, sctx, batchUpdates)` which returns `(statsVersion uint64, updated []*storage.DeltaUpdate, err error)`. `statsVersion` is the transaction's start TS.
   - The `updated` slice may have additional entries appended for partition tables' parent tables (see "Partitioning"). The function asserts the slice is sorted by `TableID` after `dumpStatsDeltaToKV` (`:173-180`).
   - On success, remove processed entries from `deltaMap` (`:183-185`). Tables remaining in `deltaMap` will be re-attempted next call (deferred `Merge` adds them back).
6. **After the batch transaction commits**, the function calls `s.statsHandle.RecordHistoricalStatsMeta(statsVersion, "flush stats", false, unlockedTableIDs...)` (`:208`) for tables not locked. Locked tables do not get a historical-stats entry; they will be recorded when unlocked.
7. **Failure handling**: errors from inside the batch return early (`:195-197`). Successful prior batches stay committed; the deferred `Merge` returns the remaining tables to the global aggregator. Repeat calls eventually drain the queue.
8. **Slow-batch logging** at three points (`:116, :152, :187`): if any phase exceeds `tooSlowThreshold = 20s`, log a warning with table count and duration. Diagnostic only; no behavioral effect.

### Dump-trigger conditions: `needDumpStatsDelta`

Defined at `session_stats_collect.go:68-92`. The per-table eligibility filter.

Contract (in order, first matching wins):

1. Table not in InfoSchema -> `false`. Stale entries for dropped tables are not flushed; they linger until the next call also returns false and they remain in deltaMap, eventually cleaned by external GC paths (e.g. `GCStats`).
2. Table is in a memtable / system schema (`metadef.IsMemOrSysDB(tableItem.DBName.L)`) -> `false`.
3. `forceDump == true` -> `true`. Overrides all other checks. Used by `FlushStats` on shutdown and by `FLUSH STATS_DELTA` (`pkg/executor/simple.go:2990`).
4. `currentTime - item.InitTime > 1 hour` (`dumpStatsMaxDuration`) -> `true`. The "at least hourly" guarantee.
5. Get stats via `GetNonPseudoPhysicalTableStats(id)` (note: explicitly avoids creating a pseudo entry). If `not found || statsTable == nil || RealtimeCount == 0 || (modify_count / RealtimeCount) > DumpStatsDeltaRatio` -> `true`. The ratio threshold `DumpStatsDeltaRatio = 1/10000` (`:46`).
6. Otherwise -> `false`.

So a table that has stats and a low change rate (under 1/10000 of rows changed) and a recent dump (within the last hour) does NOT contribute to a flush.

### Lock-aware write: `dumpStatsDeltaToKV`

Private function at `:256-351`. Writes the batch to `mysql.stats_meta`.

Contract:

1. **Zero-delta items are skipped** (`:274-276, :294-296`). A table with `Delta.Count == 0` is not written.
2. **Bulk lock status lookup** (`:286-289`) fetches the lock status for all involved table IDs (and parent IDs for partitions) in one round trip.
3. **Partition handling** (`:298-326`):
   - If the entry is a partition (`is.TableIDByPartitionID(update.TableID)` returns ok):
     - Set `update.IsLocked` to `(isTableLocked || isPartitionLocked)`.
     - If **both** parent and partition are unlocked: append a NEW `*storage.DeltaUpdate` for the parent table with the same delta. The parent's global stats are kept in sync.
     - If either is locked: only the partition entry is written; the parent's global stats are NOT updated. The deferred merge in `DumpStatsDeltaToKV` does NOT re-enqueue this case; instead, the lock-stash mechanism in `mysql.stats_table_locked` accumulates the delta. The comment at `:313-323` explains the four cases.
   - Else (non-partition): set `IsLocked` and proceed.
4. **Re-sort if appended** (`:336-341`). After potential parent-table appends, sort by `TableID` for deterministic write order. The intest assertion later requires this sort (`:173-180`).
5. **One bulk SQL write**: `storage.UpdateStatsMeta(ctx, sctx, statsVersion, updates...)` (`:344`). Writes `mysql.stats_meta` rows with `version = statsVersion = txn.startTS`, `count = realtimeCount + delta.Count`, `modify_count = oldModifyCount + delta.Modify`, plus `snapshot` and `last_stats_histograms_version` as applicable.
6. **The `updated` return slice** is the (potentially re-sorted and parent-appended) batch. Callers MUST use this slice for downstream processing (`:348-350`).

### Lock interaction

The delta-collector defers writes for locked tables but never drops the delta. The flow when a table is locked:

1. `DumpStatsDeltaToKV` sweeps and selects the table.
2. `dumpStatsDeltaToKV` detects `IsLocked` for that table.
3. The bulk SQL write `UpdateStatsMeta` writes the delta into `mysql.stats_table_locked` (the lock-aware path inside `storage.UpdateStatsMeta`) rather than `mysql.stats_meta`.
4. The historical-stats recording skips locked tables (`:199-207`).
5. When the table is later unlocked, the accumulated delta is flushed.

This interaction is implemented in `storage.UpdateStatsMeta` (outside this file's scope) but is part of the delta-collector's contract surface.

### Caller sites

Three production callers of `DumpStatsDeltaToKV`:

1. **`Domain.deltaUpdateTickerWorker`** (`pkg/domain/domain.go:2276-2283`): timer-driven, calls `DumpStatsDeltaToKV(false)` (no force) on each `statsLease` tick. The primary flush path.
2. **`Domain.deltaUpdateTickerWorkerExitPreprocessing`** (`pkg/domain/domain.go:2179-2185`): shutdown path, calls `statsHandle.FlushStats()` which internally calls `DumpStatsDeltaToKV(true, ...)` with force.
3. **`pkg/executor/simple.go:2990` `executeFlushStatsDeltaOnCurrentInstance`**: SQL-level `FLUSH STATS_DELTA` admin command. Calls with `forceDump=true` and optionally a specific table list.

`pkg/dxf/importinto/scheduler.go:735` writes a `DeltaUpdate` directly via `storage.UpdateStatsMeta` without going through `DumpStatsDeltaToKV`. This bypasses the in-memory sweep + filter and is a parallel write path; see `CURRENT-USAGE.md` open question on whether the bypass is intentional.

### Failpoints

- `panic-when-record-historical-stats-meta` (`:202-204`): panics during the unlocked-tables historical-stats record. Tests the abort-mid-flush recovery path.

### Resource bounds

| Bound | Source | Default |
|-------|--------|---------|
| Dump ratio threshold | `DumpStatsDeltaRatio` | 1/10000 |
| Max time between dumps for a table | `dumpStatsMaxDuration` | 1 hour |
| Batch size per transaction | `dumpDeltaBatchSize` | 100_000 |
| Slow-phase warning threshold | `tooSlowThreshold` | 20 seconds |
| Slow `RecordHistoricalStatsMeta` warning | hard-coded | 15 minutes |
| Predicate-column LastUsedAt throttle | `colStatsUsageLastUsedThrottleInterval` | 12 hours |
| SQL insert batch | `batchInsertSize` | 2048 |
| Dump timer interval | `statsLease` | 3 seconds (default lease) |

### Test obligations

1. Per-session `Update` accumulates correctly under concurrent writes within the same session.
2. `SweepSessionStatsList` merges and resets each session's mapper.
3. Deleted session items are unlinked on the next sweep.
4. `needDumpStatsDelta`: each of the 5 branches is exercised by a focused test.
5. `forceDump=true` overrides all other filters.
6. `dumpStatsMaxDuration` triggers a dump after ~1 hour even with a tiny change rate (use a clock injection or failpoint).
7. `DumpStatsDeltaRatio` boundary: at threshold-equal, dump happens; just below, does not.
8. Batching at 100_000: a deltaMap larger than the batch size produces multiple transactions; partial-failure in batch N leaves batches 1..N-1 committed.
9. Zero-delta items are filtered out before SQL write.
10. Partition + both unlocked: parent table receives a separate update entry with the same delta.
11. Partition + locked (any): parent is NOT updated; partition delta is stashed via lock-aware path.
12. `panic-when-record-historical-stats-meta` failpoint: the batch transaction is already committed before the panic, so the write survives.
13. The 5-clause `dumpStatsDeltaToKV` zero-delta skip, partition fanout, lock check, sort, bulk write should each be testable in isolation.
14. End-to-end freshness chain: DML, commit, force-flush, verify stats cache `Update` picks up the new version within `statsLease + LeaseOffset*Lease`.
15. `RecordHistoricalStatsMeta` is called only with unlocked tables.

### Known cross-cutting issues

- The `dxf/importinto` bypass writes `mysql.stats_meta` without going through this pipeline. Documented but not formally addressed.
- The `colStatsUsageLastUsedThrottleInterval` (12h) is a separate sub-flow for predicate-column usage; not part of the delta flush contract per se but shares the same dump cycle.

## Init Stats

The init-stats subsystem builds the in-memory stats cache from `mysql.stats_*` at server start (and on `RefreshStats` admin command). Two modes exist; both produce a populated cache when they finish, but they differ in what gets loaded and what is left for lazy filling.

Implementation: `pkg/statistics/handle/bootstrap.go` (entry points at `:827, 894`); paging worker at `pkg/statistics/handle/initstats/load_stats_page.go`; concurrency policy at `pkg/statistics/handle/initstats/load_stats.go`. Orchestration: `pkg/domain/domain.go:2038-2068` (`Domain.initStats`).

### Two modes

`InitStatsLite(ctx, tableIDs...)` (`bootstrap.go:833`) and `InitStats(ctx, is, tableIDs...)` (`bootstrap.go:894`). The selector is `Performance.LiteInitStats` (default `true` since v8.4, `config.go:1140`). A third path `Performance.SkipInitStats` bypasses both and closes `InitStatsDone` immediately (`domain.go:2042-2046`).

| Aspect | Lite | Full |
|--------|------|------|
| Meta loaded (count, modify count, version) | yes | yes |
| `ColAndIdxExistenceMap` populated | yes | yes |
| Index histograms loaded | no (existence only) | yes |
| Column histograms loaded | no | yes (full or stub depending on type) |
| TopN loaded | no | yes |
| Buckets loaded | no | yes |
| FMSketch loaded | no | yes via pre-scalar |
| Listener gated on completion | only if `ForceInitStats=true` (`server.go:483`) | same |
| Concurrency | sequential (lite path uses one session) | per `GetConcurrency()` |
| Failpoint | `beforeInitStatsLite` (`bootstrap.go:852`) | `beforeInitStats` (`bootstrap.go:920`) |

After a lite init, the cache holds meta and existence maps; per-table histograms / TopN / buckets are loaded lazily on demand by sync load or by the async-load drainer. After a full init, the cache is fully primed.

### Shared phase: `initStatsMeta`

Both modes call `initStatsMeta(ctx, sctx, tableIDs...)` (`bootstrap.go:102`) first. It returns a freshly constructed `StatsCache` (not yet swapped into the global handle) plus `maxTableID` for paging.

Contract:

1. Reads `mysql.stats_meta` for all tables (`tableIDs` empty) or the specified ones. Returns count, modify_count, version, snapshot, `last_stats_histograms_version`.
2. Populates the new cache via `initStatsMeta4Chunk` (`bootstrap.go:56`). Each row produces a `*statistics.Table` with empty `ColAndIdxExistenceMap` to be filled in subsequent phases.
3. The returned cache is **separate from the global cache**. The handle's global cache is updated only after all phases finish (via `Replace` or per-table `Put`).
4. After `initStatsMeta` returns, BOTH callers call `cache.WaitForAsyncUpdates()` (`:860, :929`) before proceeding. Comment at `:858-859, :927-928`: "initStatsMeta adds new tables without an internal wait; histogram loading reads them immediately." Without this barrier, the histogram-load phase can miss the just-inserted meta.

### Lite mode contract

`initStatsLiteWithSession` (`bootstrap.go:841`):

1. Open a transaction, call `initStatsMeta`, drain async updates (see above).
2. `initStatsHistogramsLite(ctx, sctx, cache, tableIDs...)` reads `mysql.stats_histograms` rows but does NOT load histogram payloads. It only marks `ColAndIdxExistenceMap` entries.
3. Publish the result:
   - If `len(tableIDs) == 0` (full-instance init): `h.Replace(cache)` atomically swaps the global cache. The old cache is closed.
   - Else (targeted refresh, e.g. `RefreshStats`): iterate `cache.Values()`, `h.Put(table.PhysicalID, table)` for each, `h.StatsCache.WaitForAsyncUpdates()`, then `cache.Close()` on the temporary cache.
4. Errors from `initStatsHistogramsLite` close the temporary cache (`:865`) and propagate. The global cache is not touched on failure.
5. Transaction is committed in a deferred `util.Exec(sctx, "commit")` (`:843-847`); commit errors are surfaced if the function would otherwise return nil.

The lite mode does NOT call `WaitForAsyncUpdates` between `initStatsMeta` and `initStatsHistogramsLite` for itself (it does call `cache.WaitForAsyncUpdates()` at `:860` between meta and histograms, plus `h.StatsCache.WaitForAsyncUpdates()` at `:880` in the targeted-refresh publish path).

### Full mode contract

`initStatsWithSession` (`bootstrap.go:902`):

1. **Progress tracking is mandatory.** `initstats.InitStatsPercentage.Store(0)` at start; deferred `Store(100)` at end (`:903-904`). The HTTP `/stats/priority-queue` and other observability surfaces read this atomic.
2. Read system memory via `memory.MemTotal()` (`:905`). Used by per-phase paging to choose page sizes that fit in memory.
3. Begin transaction, fire `beforeInitStats` failpoint.
4. Call `initStatsMeta`, drain async updates. Progress -> `initStatsPercentageInterval` (33%) at `:931`.
5. Compute `concurrency := initstats.GetConcurrency()` (see "Concurrency calculation").
6. Build a `loadStrategy` from `maxTableID` and `tableIDs`. Two strategies are documented: `maxTidStrategy` (scan all table IDs up to max, useful for full instance) and `tableListStrategy` (scan specified tables, useful for targeted).
7. **Three histogram phases run in sequence**, each parallel via `RangeWorker`:
   - `initStatsHistogramsConcurrently(is, cache, totalMemory, concurrency, strategy)` (`:936`): load histogram metadata + (for index types and full-load columns) histogram data. Progress advance is internal to the phase.
   - `initStatsTopNConcurrently(cache, totalMemory, concurrency, strategy)` (`:943`): load TopN. Progress -> 66% at `:947`.
   - `initStatsBucketsAndCalcPreScalar(cache, totalMemory, concurrency, strategy)` (`:951`): load buckets and compute per-bucket pre-scalars. Calls `cache.WaitForAsyncUpdates()` (`:956`) before the next step.
8. **Publish the result identically to lite mode** (`Replace` for full-instance, `Put`-then-`Close` for targeted; `:961-973`).
9. Transaction commit in deferred function.

Each phase is independent at the SQL level (they query different system tables), but they share the in-memory cache. Phase ordering matters: histograms before TopN, TopN before buckets. Out-of-order processing would leave the cache in an inconsistent state for a query landing between phases (which cannot happen for `Replace`-style publishing, but can for `Put` targeted refresh).

### Concurrency calculation

`initstats.GetConcurrency()` (`load_stats.go:29-38`):

- `Performance.ForceInitStats == true`: `concurrency = min(max(2, GOMAXPROCS-2), 16)`. Comment: "-2 is to ensure that the system has enough resources to handle other tasks. such as GC and stats cache internal."
- `Performance.ForceInitStats == false`: `concurrency = min(max(2, GOMAXPROCS/2), 16)`. Comment: "ensure that concurrency doesn't affect the performance of customer's business."

Range: `[2, 16]`. Default `Performance.ForceInitStats` is `false`, so the typical concurrency is `GOMAXPROCS/2`.

### Paging: RangeWorker

`pkg/statistics/handle/initstats/load_stats_page.go`:

`RangeWorker` is a fan-out worker pool:

- `concurrency` worker goroutines (`LoadStats` at `:90`).
- One buffered channel `taskChan` with capacity 1 (`:80`).
- `processTask(task Task)` function pointer set by the caller.
- `taskCnt` total work units; `completeTaskCnt` atomic counter.
- Progress is updated per task: `taskPercentage = completeTaskCnt/taskCnt * totalPercentageStep + totalPercentage` (`:105`). The `InitStatsPercentage` atomic global is updated after each task.

`Task` is a half-open table-ID range: `[StartTid, EndTid)` (`:44-47`). The caller's `processTask` query reads `mysql.stats_*` rows where `table_id >= StartTid AND table_id < EndTid`.

Lifecycle:
1. Caller constructs the worker: `NewRangeWorker(taskName, processTask, concurrency, totalTaskCnt, totalPercentageStep)`.
2. `LoadStats()` spawns `concurrency` goroutines, each reading from `taskChan`.
3. Caller calls `SendTask(task)` for each range (typically driven by the `loadStrategy`).
4. Caller calls `Wait()`, which closes `taskChan` and joins all workers.

The channel capacity of 1 means the caller is throttled by worker availability. With `concurrency` workers, at most `concurrency + 1` tasks are in flight.

### `InitStatsDone` channel and bootstrap gates

`statsHandle.InitStatsDone` is a `chan struct{}` that is closed when `Domain.initStats` returns (success or failure). Several places gate on it:

1. **`Domain.initStats` itself** (`domain.go:2038-2068`):
   - If `Performance.SkipInitStats=true`: close `InitStatsDone` immediately and return (no cache populated). Used when `--skip-grant-table` is set.
   - Else: branch on `Performance.LiteInitStats` and call `InitStatsLite` or `InitStats`. Errors are logged but not propagated; in all cases, `InitStatsDone` is closed in a `defer` (`:2053`).
   - Wrapped in `defer recover()` (`:2049-2052`) so a panic during init does not prevent the channel close.

2. **`Domain.loadStatsWorker`** (`domain.go:2087-2101`): calls `initStats(ctx)` then enters a `statsLease` ticker loop calling `statsHandle.Update(ctx, do.InfoSchema())`. The worker IS the goroutine that runs init.

3. **`Domain.waitStartTask`** (`domain.go:1970`): blocks on `<-do.StatsHandle().InitStatsDone` (or `<-do.exit`). Used to gate GC / auto-analyze / cleanup workers.

4. **`Domain.asyncLoadHistogram`** (`domain.go:2115`): blocks on the same channel before starting its drain loop.

5. **`Server.run`** (`pkg/server/server.go:483`): if `Performance.ForceInitStats=true`, blocks on `<-dom.StatsHandle().InitStatsDone` before opening the TiDB listener. This is the only place that gates **inbound query traffic** on stats init; the default (`ForceInitStats=false`) opens the listener immediately.

Contract:

1. `InitStatsDone` is closed **exactly once**, regardless of init success, failure, panic, or `SkipInitStats`.
2. After close, all `<-InitStatsDone` selects unblock.
3. Production code never reopens or replaces this channel; it is created during `Handle` construction.

### Error model

Init failure is partially silent by design:

1. Errors from `InitStatsLite` / `InitStats` are logged at `Error` level in `Domain.initStats` (`:2064`) but the function returns normally and closes `InitStatsDone`.
2. The cache may be partially populated: tables loaded before the error are in the cache; tables after are not.
3. The next `loadStatsWorker` tick will call `Update`, which is version-driven and will eventually load missing tables once their `mysql.stats_meta.version` is below the current `MaxTableStatsVersion + LeaseOffset*Lease`. Until then, the planner sees pseudo for unloaded tables.
4. Mid-phase panic recovery is **only at the `Domain.initStats` level** (`:2049`); a panic inside an `initStatsHistogramsConcurrently` worker propagates up and aborts the whole init.

This is one of the open issues recorded in `CURRENT-USAGE.md` Open Q20: should `InitStats` propagate errors instead of swallowing them?

### Failpoints

- `beforeInitStatsLite` at `bootstrap.go:852`: empty inject, used by tests to hook before lite init begins.
- `beforeInitStats` at `bootstrap.go:920`: same for full init.
- `mockBucketsLoadMemoryLimit` (referenced in the cross-reference notes but to be re-confirmed in code): bounds memory during bucket paging.

### Resource bounds

| Bound | Source | Default |
|-------|--------|---------|
| Concurrency | `initstats.GetConcurrency` | 2-16 (see formula) |
| `Performance.ForceInitStats` | config | `false` |
| `Performance.LiteInitStats` | config | `true` (since v8.4) |
| `Performance.SkipInitStats` | config | `false` |
| Paging memory bound | `memory.MemTotal()` used inside each phase | system RAM |
| RangeWorker channel buffer | `taskChan` cap | 1 |
| Progress atomic | `InitStatsPercentage` | 0..100 |

### Test obligations

1. Lite mode produces meta + existence-only map; no histograms / TopN / buckets in the cache.
2. Full mode produces all four (meta + histograms + TopN + buckets + pre-scalars).
3. `SkipInitStats=true` closes `InitStatsDone` and leaves the cache empty.
4. `ForceInitStats=true` causes `Server.run` to block on `InitStatsDone` before listening.
5. `ForceInitStats=false` opens the listener without waiting.
6. Targeted refresh (`tableIDs` non-empty) does NOT replace the global cache; it `Put`s individual entries and closes the temporary.
7. Full-instance refresh (`tableIDs` empty) replaces the global cache atomically; old cache is closed.
8. `WaitForAsyncUpdates` is called after `initStatsMeta`, before subsequent histogram phases read the meta; without it, histogram phases miss recent meta inserts.
9. `WaitForAsyncUpdates` is called after the final phase, before publishing the cache.
10. Mid-init failure leaves the partial cache discarded (temporary cache `Close()`-d on error path); the global cache is unchanged.
11. Concurrency formula bounds: with `GOMAXPROCS=1`, concurrency floors at 2; with `GOMAXPROCS=64` and `ForceInitStats=false`, concurrency caps at 16.
12. RangeWorker progress monotonicity: `InitStatsPercentage` only increases during init.
13. Idle workers exit cleanly when `taskChan` is closed (`Wait` returns).
14. `beforeInitStatsLite` / `beforeInitStats` failpoints fire before any DB read; tests can inject state via these hooks.
15. `LiteInitStats=true` (default): cache is correctly populated such that sync-load can lazy-load any column / index on first reference.

### Known cross-cutting issues

- Q20 (InfoSchema seam): `InitStats` takes `is`; `InitStatsLite` does not (per PR #54514). The two modes differ in their InfoSchema coupling.
- Q21 (cache lifecycle): the targeted refresh path uses `h.Put` which writes to the LFU and depends on `WaitForAsyncUpdates` for visibility.

## Visible StatsCache Interface

The `StatsCache` interface is the only sanctioned way for code outside `pkg/statistics/handle/cache/` to read, mutate, and react to the in-memory stats cache. Internal LFU mechanics (Ristretto admission, sharded keyset, eviction policy) are deliberately out of scope; this section pins down the visible API, its ordering / visibility guarantees, and the modes the cache operates in.

Implementation: `pkg/statistics/handle/cache/statscache.go` (`StatsCacheImpl`). Interface: `pkg/statistics/handle/types/interfaces.go:217-271`.

### Mode selection

Two implementations exist behind the interface; selection is by `Performance.EnableStatsCacheMemQuota` (default `true`).

- **Quota mode** (`EnableStatsCacheMemQuota=true`): the inner `*StatsCache` is mutated in place via Ristretto. `UpdateStatsCache` calls `Load().Update(updated, deleted, skipMoveForward)` (`statscache.go:292-293`). Memory is bounded by `tidb_stats_cache_mem_quota` (default 0, which resolves to ~20% of system RAM).

- **Non-quota mode** (`EnableStatsCacheMemQuota=false`): `UpdateStatsCache` calls `Load().CopyAndUpdate(updated, deleted)` to produce a new cache and atomically swaps it via `replace` (`:294-298`). The old cache is `Close()`-d after swap. A TODO at `:295` says this branch will be removed once quota is mandatory.

Tests must not assume quota mode unless they set `Performance.EnableStatsCacheMemQuota = true` explicitly. The contract applies to both modes; clauses below note where they differ.

### Public methods

#### `Get(tableID int64) (*statistics.Table, bool)`

Defined at `statscache.go:323`. Returns the table's stats if cached.

Contract:

1. Returns `(nil, false)` when the table is not currently in the cache (whether never loaded, evicted, or pending async admission). The two states are not distinguishable by the caller.
2. Returns `(*statistics.Table, true)` when the table is in the cache. The returned pointer is to an immutable snapshot: callers MUST NOT mutate it directly; mutation requires a `CopyAs` or going through `UpdateStatsCache`.
3. The `StatsCacheGetNil` failpoint (`:324-326`) injects `(nil, false)` unconditionally; used by tests that need to simulate cache miss.
4. `Get` is non-blocking and lock-free at the visible level; concurrent calls are safe.

#### `Put(tableID int64, t *statistics.Table)`

Defined at `statscache.go:331-333`. Inserts or replaces a single table entry.

Contract:

1. Visibility is **not guaranteed synchronously**. The underlying LFU (Ristretto) buffers admissions; a subsequent `Get(tableID)` may return `(nil, false)` until the buffer is drained.
2. Callers that need visibility ordering MUST call `WaitForAsyncUpdates` before relying on a subsequent `Get`.
3. Rejection by LFU admission (insufficient frequency to outrank existing items) is silent. `Put` does not return success/failure; the caller cannot tell whether the item was admitted.
4. `Put` updates the inner cache's max-version tracking if the table's `Version` exceeds the current max (subject to `SkipMoveForward`).

#### `UpdateStatsCache(update CacheUpdate)`

Defined at `statscache.go:291-299`. Batch update with delete and update lists.

Contract:

1. `CacheUpdate{Updated, Deleted, Options}` is the unit of atomicity for the caller's intent. In quota mode, both lists are applied to the same inner cache instance. In non-quota mode, both lists are applied to a freshly cloned inner cache that then replaces the old one.
2. `Options.SkipMoveForward` controls whether the cache's max-version tracker advances. Set to `true` by `Update` when a specific table list was passed (`statscache.go:140-142`), ensuring partial updates do not advance the global checkpoint.
3. The same async-admission caveat as `Put` applies to the `Updated` list; `Deleted` removes are synchronous (Ristretto's `Del` is not buffered).
4. Concurrent `UpdateStatsCache` calls from multiple callers are safe; the inner cache serializes its own internal state.

#### `Update(ctx context.Context, is infoschema.InfoSchema, tableAndPartitionIDs ...int64) error`

Defined at `statscache.go:122-254`. Version-driven refresh from storage.

Contract:

1. Reads `mysql.stats_meta` rows with `version > lastVersion` where `lastVersion = GetNextCheckVersionWithOffset()` (`:129`). The offset of `LeaseOffset * Lease` (5 leases, `:265`) accounts for cross-table commit-order skew: a smaller version may commit later than a larger version, so the checkpoint trails the observed max by 5 leases.

2. **Full-instance mode** (no `tableAndPartitionIDs`): queries all rows past the checkpoint. The checkpoint is advanced after the update (`SkipMoveForward=false`).

3. **Targeted mode** (with `tableAndPartitionIDs`): queries only those IDs, AND keeps the checkpoint frozen (`SkipMoveForward=true`, `:141-152`). The targeted mode is used by `pkg/executor/analyze.go:395` to refresh stats right after ANALYZE completes; using `SkipMoveForward=true` ensures other tables that committed during the analyze are not silently skipped on the next full refresh.

4. **InfoSchema resolution is required for each row.** `TableInfoByID(is, physicalID)` (`:188`) is called per row. If the table is not in InfoSchema, the entry is added to the delete list (`:194`). This is the version-driven half of the InfoSchema seam (see `CURRENT-USAGE.md` Open Q20).

5. **Skip-when-unchanged optimization.** If `oldTbl.Version >= version` and `tableInfo.UpdateTS == oldTbl.TblInfoUpdateTS` (`:201-203`), the row is skipped. Otherwise, the table is reloaded.

6. **Meta-only refresh optimization** (`:209-213`): if `latestHistUpdateVersion > 0` and `oldTbl.LastStatsHistVersion >= latestHistUpdateVersion`, the table is recreated via `CopyAs(MetaOnly)` and `needLoadColAndIdxStats = false`. Only `count`, `modify_count`, `version`, and `tblInfo update_ts` are refreshed from storage; histograms are reused. This is the cheap path for delta-driven updates.

7. **Full reload path** (`:214-234`): `TableStatsFromStorage(tableInfo, physicalID, false, 0)` loads the row plus its histograms. Errors are logged at Warn but not propagated; the table is skipped (`:228`). A nil result is treated as "table dropped" and added to the delete list (`:231`).

8. **LastAnalyzeVersion bootstrap** (`:247-249`): if `tbl.LastAnalyzeVersion == 0 && snapshot != 0`, initialize `LastAnalyzeVersion = snapshot`. This handles the predicate-columns-only ANALYZE case where `mysql.stats_meta.snapshot` is non-zero but `LastAnalyzeVersion` was never recorded.

9. Updates are batched in chunks of 10 (`batchSizeOfUpdateBatch`, `:84`) via `cacheOfBatchUpdate` to amortize the `UpdateStatsCache` overhead.

10. Context cancellation is checked once per row (`:184-186`); the function returns the context error immediately.

#### `WaitForAsyncUpdates()`

Defined at `statscache.go:341-343`. Blocks until LFU's buffered admissions are visible to subsequent `Get` calls.

Contract:

1. After `Put` or `UpdateStatsCache.Updated`, a `Get` may miss until `WaitForAsyncUpdates` is called. This is documented at `interfaces.go:267-270`.
2. The barrier is per-cache; tests that exercise admission ordering need to call this between writes and reads.
3. `init stats` (Phase 0b task 3) relies on this barrier between paged histogram chunks; see that section.

#### `TriggerEvict()`

Defined at `statscache.go:336-338`. Asks the LFU to evict items to free memory.

Contract:

1. Called only by `Domain.gcStatsWorker` (`pkg/domain/domain.go:2231`) after `memory.ForceReadMemStats()` detects pressure. No other production caller.
2. Eviction policy is internal; the contract does not specify which items are evicted, only that some are.
3. Map-cache backend (`pkg/statistics/handle/cache/internal/mapcache/map_cache.go:135-136`) implements this as a no-op.
4. Eviction is silent: callers cannot observe which items were evicted. A subsequent `Get` for an evicted table returns `(nil, false)`; the caller must reload via `Update` or sync load.

#### `SetStatsCacheCapacity(capBytes int64)`

Defined at `statscache.go:362-364`. Resizes the cache's memory budget.

Contract:

1. Called by the `tidb_stats_cache_mem_quota` sysvar handler in `pkg/domain/domain_sysvars.go:55-59`. Live resize is supported.
2. Shrinking the capacity may trigger immediate eviction inside the LFU; the contract does not guarantee a specific eviction count or set.
3. `capBytes = 0` means use the auto-detected default (~20% of system RAM).

#### `Len()`, `Values()`, `MemConsumed()`, `MaxTableStatsVersion()`

Defined at `statscache.go:347-359, 317-320`. Read-only observables.

Contract:

1. All four are snapshot-style: they may not reflect concurrent writes still in flight.
2. `Values()` allocates a new slice; callers should not call it in hot paths.
3. `MemConsumed()` returns the LFU's accounting, not the true Go heap consumed by the entries. In non-quota mode it returns 0 historically (TODO).
4. `MaxTableStatsVersion()` is the version checkpoint used by `Update`'s "what to fetch" logic; combined with `GetNextCheckVersionWithOffset`, it determines which `mysql.stats_meta` rows are read.

#### `GetNextCheckVersionWithOffset() uint64`

Defined at `statscache.go:257-273`. Returns `MaxTableStatsVersion()` minus a 5-lease grace window.

Contract:

1. Guarantees: `result = max(0, MaxTableStatsVersion - 5*Lease)`. The 5-lease offset is the bound on cross-table version skew the system tolerates.
2. The 5-lease constant is fixed in code (`LeaseOffset = 5` at `:44`); changing it has correctness implications and is not a sysvar.

#### `Replace(cache StatsCache)`

Defined at `statscache.go:276-279`. Atomically swaps in a new inner cache; closes the old one.

Contract:

1. The argument MUST be of type `*StatsCacheImpl`; the function does a type assertion (`:277`).
2. After `Replace` returns, all subsequent operations on the receiver use the new inner cache.
3. The old cache's `Close()` is invoked; any pending operations on it must have completed (the contract does not enforce this; it is the caller's obligation).

#### `Close()`, `Clear()`

Defined at `statscache.go:302-304, 308-315`. Lifecycle controls.

Contract:

1. `Close()` is called from `Domain.Close()` (`pkg/domain/domain.go:526`); not safe to call concurrently with reads.
2. `Clear()` constructs a fresh inner cache and swaps it in via `replace`. After `Clear()`, the cache is empty but operational.

#### `UpdateStatsHealthyMetrics()`

Defined at `statscache.go:370-397`. Computes and emits the stats-healthy gauge distribution.

Contract:

1. Reads `Values()`. Tables are categorized into one of: `Pseudo`, `UnneededAnalyze` (analyzed but below auto-analyze min count threshold), or one of the healthy buckets based on `GetStatsHealthy()`.
2. The gauges satisfy `total = pseudo + unneededAnalyze + sum(healthy buckets)` (documented at `:368-369`).

### Concurrency and visibility model

1. The `StatsCacheImpl` wraps an `atomic.Pointer[StatsCache]`. All visible operations dispatch through `s.Load()` so the inner cache can be atomically replaced (e.g., in non-quota `UpdateStatsCache` or in `Replace` / `Clear`).
2. There is no global lock on the visible API. Concurrent `Get`, `Put`, `UpdateStatsCache`, `Update` are safe; the inner cache serializes internally as needed.
3. **Write-then-read visibility requires `WaitForAsyncUpdates` between the two.** Without it, the reader may miss the write due to LFU buffered admission. This is documented in the interface comment and is critical for the init-stats paging logic.

### Failure model

Visible failures the caller can observe:

| Outcome | Cause |
|---------|-------|
| `Get` returns `(nil, false)` | Table not cached. Includes evicted, pending-admission, or never-loaded. |
| `Update` returns context error | Caller's context was canceled during the row loop. |
| `Update` skips a row silently | InfoSchema lookup failed (added to delete list), `TableStatsFromStorage` returned an error (logged, skipped), or the row's `version`/`update_ts` shows it is already current. |
| `Put` silently rejected | LFU admission policy denied the new item. Caller has no signal. |

### Resource bounds

| Bound | Source | Default |
|-------|--------|---------|
| Cache memory | `tidb_stats_cache_mem_quota` | 0 (resolves to ~20% RAM, cap 1TB) |
| Quota mode toggle | `Performance.EnableStatsCacheMemQuota` | `true` |
| Version-offset lag | `LeaseOffset * Lease` | 5 * 3s = 15s (with default lease) |
| Update batch size | `batchSizeOfUpdateBatch` | 10 |

### Test obligations

1. `Get` after `Put` without `WaitForAsyncUpdates` may miss; with `WaitForAsyncUpdates` MUST hit.
2. `Update` with empty `tableAndPartitionIDs` advances `MaxTableStatsVersion`.
3. `Update` with non-empty `tableAndPartitionIDs` does NOT advance `MaxTableStatsVersion` (SkipMoveForward path).
4. `LeaseOffset` window: an entry committed within the last 5 leases is reloaded; one outside is not.
5. `Update` with InfoSchema missing the table: table is removed from cache.
6. `Update` meta-only path: when `latestHistUpdateVersion > 0` and `oldTbl.LastStatsHistVersion >= latestHistUpdateVersion`, histograms are NOT reloaded (verify storage round-trip count).
7. `Update` full-reload path: histograms ARE reloaded otherwise.
8. `TriggerEvict` actually evicts items from a full cache; subsequent `Get` for evicted IDs misses.
9. `SetStatsCacheCapacity` live-resize: shrinking below current consumption triggers eviction.
10. `Replace` atomically swaps caches; in-flight reads against the old cache do not observe the new one mid-call.
11. Quota mode vs non-quota mode: `UpdateStatsCache` semantics are equivalent at the visible API level (modulo memory accounting).
12. `LastAnalyzeVersion` bootstrap: a row with `LastAnalyzeVersion=0` and `snapshot != 0` results in `LastAnalyzeVersion = snapshot` after `Update`.

### Known cross-cutting issues

See `CURRENT-USAGE.md`:

- Q19: cache-too-small steady state.
- Q20: InfoSchema seam (every `Update` row consults InfoSchema; missing -> delete).
- Q21: stats cache can outlive InfoSchema LRU.

## Sync Load

The sync-load subsystem accepts a per-statement batch of column / index histogram requests with a deadline, dispatches them to a worker pool, and lets the planner block until all are loaded or the deadline elapses. It is the only stats-load path that a query can wait on.

Implementation: `pkg/statistics/handle/syncload/stats_syncload.go`. Public interface: `pkg/statistics/handle/types/interfaces.go:443-459` (`StatsSyncLoad`). Planner-side wrappers: `pkg/planner/core/rule/rule_collect_plan_stats.go:336-388` (`RequestLoadStats`, `SyncWaitStatsLoad`).

### Public interface

#### `SendLoadRequests(sc *stmtctx.StatementContext, neededHistItems []model.StatsLoadItem, timeout time.Duration) error`

Defined at `stats_syncload.go:114`. Enqueues a batch of requested histograms for asynchronous loading and stages per-item result channels on the StatementContext.

Contract:

1. **Pre-filtering MUST happen first.** Items already present in the cache as fully loaded (column) or loaded (index) MUST be filtered out before any task is dispatched. `removeHistLoadedColumns` at `stats_syncload.go:205-225` consults `statsHandle.Get(item.TableID)`, then `tbl.IndexIsLoadNeeded(item.ID)` or `tbl.ColumnIsLoadNeeded(item.ID, item.FullLoad)`. Items for tables not in the cache are silently dropped (`:209` `continue`).

2. **Empty batch returns nil with no side effects.** If `remainedItems` is empty after filtering, the function returns `nil` and `sc.StatsLoad` is not mutated (`:117-119`).

3. **Batch state is written to the StatementContext.** Three fields are populated on `sc.StatsLoad`: `Timeout` (the per-batch deadline), `NeededItems` (the post-filter item list), `ResultCh` (a slice of singleflight result channels, one per item) (`:120-122, 147, 149`). The `LoadStartTime` is set to `time.Now()` (`:149`).

4. **Singleflight deduplicates concurrent requests** for the same `(TableID, columnOrIndexID, isIndex, FullLoad)`. The key is `localItem.Key()` (`:125`). A second concurrent caller asking for the same item receives the same result channel and does not enqueue a duplicate task. Meta-load (`FullLoad=false`) and full-load (`FullLoad=true`) for the same column are deduplicated **separately**: they have distinct keys.

5. **Each item gets its own singleflight goroutine.** Inside `DoChan`, a `NeededItemTask` is constructed with `ToTimeout = now + timeout` and a buffered `ResultCh` of size 1 (`:128-131`). The goroutine pushes the task to `s.neededItemsCh` with a deadline-respecting select (`:133-145`).

6. **Three terminal outcomes for the inner goroutine, each surfaced via singleflight:**
   - Task accepted into the channel and worker delivered a result before the deadline: returns `(StatsLoadResult, nil)` (`:139-141`).
   - Task accepted but no result before the deadline: returns `(nil, errors.New("sync load took too long to return"))` (`:137-138`).
   - Channel full and deadline reached before send succeeded: returns `(nil, errors.New("sync load stats channel is full and timeout sending task to channel"))` (`:143-144`).

7. **The function itself never blocks the caller.** It returns `nil` after staging all singleflight channels. Blocking happens later in `SyncWaitStatsLoad`. `SendLoadRequests` returning is not a guarantee of any task progress.

8. **Caller responsibilities:** the planner is responsible for calling `SyncWaitStatsLoad` to consume the result channels. If the planner skips the wait, the singleflight goroutines still run, eventually deliver into `ResultCh`, and the buffered `task.ResultCh` (size 1) absorbs the worker's write without blocking.

#### `SyncWaitStatsLoad(sc *stmtctx.StatementContext) error`

Defined at `stats_syncload.go:154`. Blocks until every staged result channel has delivered or the batch deadline elapses.

Contract:

1. **Empty NeededItems is a no-op.** Returns `nil` immediately if `len(sc.StatsLoad.NeededItems) == 0` (`:155-157`). The wait is unconditionally safe to call even when no sync request was emitted (e.g., the async-only path at `rule_collect_plan_stats.go:99-103`).

2. **One timer governs the whole batch.** `timer := time.NewTimer(sc.StatsLoad.Timeout)` (`:170`). The same `Timeout` that was passed to `SendLoadRequests` bounds the total wait. Per-item timeouts are not separately tracked here; they were already encoded in `task.ToTimeout` at enqueue time.

3. **Result channels are drained in order.** For each `resultCh` in `sc.StatsLoad.ResultCh`, the function does a select on `resultCh` and `timer.C` (`:172-195`).

4. **Three outcomes per channel:**
   - Channel delivers a value: record its `Err` (singleflight-level error) or `Val.HasError()` (worker-level error). Successful items are removed from `resultCheckMap` (`:174-190`).
   - Channel closed unexpectedly: return `"sync load stats channel closed unexpectedly"` (`:177`).
   - Timer fires first: return `"sync load stats timeout"` immediately, skipping any remaining channels (`:191-194`).

5. **Errors aggregate but do not abort.** Per-item errors are collected into `errorMsgs` and logged at `Warn` level in a deferred function (`:158-163`). The function only returns an error when the whole batch times out or a channel closes unexpectedly. Individual item failures do not abort the wait; the planner sees the affected items as not-loaded and falls back to pseudo via the cardinality estimators' own pseudo branches.

6. **NeededItems is cleared on return.** `sc.StatsLoad.NeededItems = nil` runs in the deferred function (`:164`). This is the signal that a wait has happened; re-calling `SyncWaitStatsLoad` is a no-op.

7. **Successful completion records latency.** When `resultCheckMap` is empty after the drain loop, `metrics.SyncLoadHistogram` is observed with the duration since `sc.StatsLoad.LoadStartTime` (`:197-200`). Partial completions (some items delivered, some pending when timeout fires) do not record latency.

#### `SubLoadWorker(exit chan struct{}, exitWg *util.WaitGroupEnhancedWrapper)`

Defined at `stats_syncload.go:242`. The main loop of a single worker goroutine. Domain bootstrap calls this `concurrency` times (see "Worker pool sizing").

Contract:

1. **Each worker is a long-lived goroutine** that processes tasks until `exit` is signaled. Returns only on `errExit` (`:254-256`).

2. **A task that fails with a retryable error is retried by the same worker.** The worker passes the failed task back to the next loop iteration via `lastTask` (`:248, 251, 314`). This is per-worker retry; another worker does not pick up the task.

3. **Per-task retry budget is `RetryCount = 1`** (`stats_syncload.go:53`). `isVaildForRetry` increments `task.Retry` and returns true while `task.Retry <= RetryCount` (`:317-320`). After the budget is exhausted, the task's `ResultCh` receives a result with `Error` set (`:309-312`) and the worker continues with the next task.

4. **Inter-retry sleep is randomized to avoid thundering herd.** `time.Sleep(Lease()/10 + rand.Intn(500)*µs)` (`:270-273`). Two consecutive failures on the same task produce different sleep durations within the bound `[Lease/10, Lease/10 + 500µs]`.

5. **Panics inside `HandleOneTask` and `handleOneItemTask` are recovered and converted to errors.** `defer recover` at `:286-291` and `:323-329`. The worker does not crash on a panicking task; it logs and continues.

#### `HandleOneTask(lastTask *NeededItemTask, exit chan struct{}) (task *NeededItemTask, err error)`

Defined at `stats_syncload.go:284`. Exposed on the interface for tests.

Contract:

1. Returns `(nil, nil)` on success (`:307`).
2. Returns `(task, nil)` if the task should be retried without sleep (the comment at `:282` says "if the task is timeout, return the task and nil"; current code treats retry-exhaustion as terminal at `:309-312`, so the "retry without sleep" branch is exercised only when `task.Retry < RetryCount`).
3. Returns `(task, err)` if the task failed and should be retried with sleep.
4. Returns `(nil, errExit)` when the domain is shutting down.

#### `AppendNeededItem(task *NeededItemTask, timeout time.Duration) error`

Defined at `stats_syncload.go:228`. Test-only API for injecting tasks directly into the queue. Used by tests in `stats_syncload_test.go`. The interface comment says it is for test only.

### Internal invariants

#### Worker pool sizing

`StartLoadStatsSubWorkers` at `pkg/domain/domain.go:2017-2024` is called once at bootstrap from `pkg/session/session.go:4366-4376`. The concurrency is:

- `Performance.StatsLoadConcurrency` if > 0 (operator override; configured via `tidb_stats_load_concurrency`).
- `GetSyncLoadConcurrencyByCPU()` otherwise, returning 5 (cores <= 8), 6 (<= 16), 8 (<= 32), or 10 (> 32) (`stats_syncload.go:55-66`).

Default config value `Performance.StatsLoadConcurrency = 0` means "auto" (`pkg/config/config.go:1132`). Validation range is `[1, 128]` enforced via `DefStatsLoadConcurrencyLimit` and `DefMaxOfStatsLoadConcurrencyLimit`, but `0` is permitted as the sentinel for auto-mode. Negative values are also accepted; `-1` is the documented "no worker" sentinel for tests (referenced from PR history; current behavior is to spawn zero workers).

#### Two-channel priority model

Two channels of identical buffered capacity `Performance.StatsLoadQueueSize`:

- `neededItemsCh`: tasks that have not yet exceeded `ToTimeout`. Higher priority.
- `timeoutItemsCh`: tasks that exceeded `ToTimeout` while sitting in `neededItemsCh`. Lower priority.

Initialization: `stats_syncload.go:101-102`.

`drainColTask` at `stats_syncload.go:520-557` implements the priority:

1. If `exit` is signaled, return `errExit`.
2. Pull from `neededItemsCh`. If the popped task is past its `ToTimeout`, move it to `timeoutItemsCh` via `writeToTimeoutChan` and loop (`:530-535`).
3. If `neededItemsCh` is empty, pull from `timeoutItemsCh`. **But:** before returning a timeout task, do a non-blocking re-check of `neededItemsCh`; if a fresh task is now available there, return that one and put the timeout task back. This prevents starvation of fresh tasks (`:537-554`).

`writeToTimeoutChan` is non-blocking with a `default` case (`:559-565`); if `timeoutItemsCh` is full, the timed-out task is **dropped**. The accompanying singleflight goroutine times out on its own (`:137-138`) and the caller sees the timeout error.

#### Singleflight deduplication

A single package-level `singleflight.Group` is used: `globalStatsSyncLoadSingleFlight` (`stats_syncload.go:95`).

Key shape: `model.StatsLoadItem.Key()` which encodes `(TableID, ID, IsIndex, FullLoad)`. Two distinct queries asking for the same `(table, column, isIndex, fullLoad)` four-tuple share one result channel. Meta-load (`FullLoad=false`) and full-load (`FullLoad=true`) of the same column have distinct keys and are not deduplicated against each other.

Scope is **global to the process**, not per-statement. Concurrent statements share dedup state.

#### Per-task session and priority

Each item task gets its own session, acquired from the pool via `s.statsHandle.SPool().WithSession(...)` (`stats_syncload.go:331`). Sessions are not shared across concurrent tasks.

Inside the session callback (`:333-336`):

- `sctx.GetSessionVars().StmtCtx.Priority = mysql.HighPriority` before the work.
- `sctx.GetSessionVars().StmtCtx.Priority = mysql.NoPriority` in `defer` after.

This ensures every internal SQL emitted for sync load uses high priority. Additionally, the storage-layer calls explicitly pass `kv.PriorityHigh` (`stats_syncload.go:444, 464, 469, 474`): `HistMetaFromStorageWithHighPriority`, `HistogramFromStorageWithPriority(..., kv.PriorityHigh)`, `CMSketchAndTopNFromStorageWithHighPriority`.

#### Stats cache update is serialized

`mutexForStatsCache` at `stats_syncload.go:92` is held during `updateCachedItem` (`:569-619`). Multiple workers may complete loads for the same table; the mutex serializes the `CopyAs` + `SetCol` / `SetIdx` + `UpdateStatsCache` sequence so the cache does not see torn updates.

The update re-reads the cached table at `:573` after acquiring the lock; if the table has been removed (e.g., DDL drop) the update is silently skipped.

#### Two write paths within one task

For a not-analyzed column (`!analyzed` per `ColumnIsLoadNeeded`), the task installs an `EmptyColumn` placeholder and returns success **without contacting storage** (`stats_syncload.go:399-403`):

    if !analyzed {
        wrapper.col = statistics.EmptyColumn(item.TableID, isPkIsHandle, wrapper.colInfo)
        s.updateCachedItem(item, wrapper.col, wrapper.idx, task.Item.FullLoad)
        return nil
    }

For an analyzed item, `readStatsForOneItem` (`:434-516`) is called. If `HistMetaFromStorageWithHighPriority` returns `(nil, nil)`, the task returns `errGetHistMeta` (`:455`) which is **swallowed** by the caller (`:409-411`): the task returns `nil` as if successful. The rationale is documented at `:449-454`: "Histogram not found, possibly due to DDL event is not handled, please consider analyze the table." A warning is logged but the planner sees the item as "not-loaded" (no cache update happens) and uses pseudo.

#### Failpoints

Defined inside the package:

- `handleOneItemTaskPanic` (`stats_syncload.go:405`): injects a panic at the start of `handleOneItemTask`'s storage-loading section. Used by tests to verify the panic-recovery path.
- `mockReadStatsForOnePanic` (`:435`): injects a panic at the start of `readStatsForOneItem`. Tests the inner panic recovery.
- `mockReadStatsForOneFail` (`:436-440`): returns an error from `readStatsForOneItem`. Tests the failure-and-retry path.

Failpoint names are stable identifiers; tests rely on the exact strings.

#### Item normalization

`removeHistLoadedColumns` (`stats_syncload.go:205-225`) is the only place that decides "what is already cached." Its decisions:

- Item for table not in cache: dropped (no error to the caller). The cache must be populated for the table before sync load can do anything.
- Index item: if `IndexIsLoadNeeded` returns false (already fully loaded), dropped.
- Column item: if `ColumnIsLoadNeeded(id, FullLoad)` returns false, dropped. A column already meta-loaded that the caller requested as meta is dropped; a column already meta-loaded that the caller requested as full is kept.

### Failure model

For a single batch with N items, the possible terminal outcomes for the caller (after `SyncWaitStatsLoad` returns):

| Caller outcome | Cause |
|----------------|-------|
| `nil` error, all items loaded | Workers delivered results for all items before batch timeout. |
| `nil` error, some items failed | Per-item errors logged at Warn, planner sees not-loaded items, uses pseudo. Aggregated in `errorMsgs`. |
| `"sync load stats timeout"` | Batch timer fired before all channels delivered. Remaining items effectively not-loaded. |
| `"sync load stats channel closed unexpectedly"` | Programming error or unclean shutdown. Should not occur in healthy operation. |

The planner-side wrapper at `rule_collect_plan_stats.go:336-388` adds a gate: if `tidb_stats_load_pseudo_timeout` is `true` (the default per `tidb_vars.go:1633`), any error from `SyncWaitStatsLoad` is downgraded to a warning, the plan-cache is skipped (`SetSkipPlanCache(skipPlanCacheReasonSyncLoadFallback)`), and the query proceeds with pseudo. If `tidb_stats_load_pseudo_timeout` is `false`, the error propagates and the query fails.

### Resource bounds

| Bound | Source | Default |
|-------|--------|---------|
| Worker concurrency | `Performance.StatsLoadConcurrency` then `GetSyncLoadConcurrencyByCPU()` | 5-10 by CPU |
| `neededItemsCh` buffer | `Performance.StatsLoadQueueSize` | Validated `[1, ...]` |
| `timeoutItemsCh` buffer | Same as `neededItemsCh` | Same |
| Per-task retry | `RetryCount` | 1 |
| Per-batch deadline | Caller-provided `timeout` | Planner uses `tidb_stats_load_sync_wait` |
| Pseudo on timeout | `tidb_stats_load_pseudo_timeout` | `true` |
| Inter-retry sleep | `Lease/10 + rand[0..500)µs` | Lease default 3s, so ~300ms |

Tests must not hard-code `RetryCount`. The contract is "at most some bounded number of retries"; the constant is implementation choice.

### Test obligations

Each clause above implies one or more tests. These are obligations for Phase 1; they are listed here so the contract is testable without external context.

1. Singleflight dedup: N concurrent `SendLoadRequests` for the same item produce one task delivery to the worker, N caller receives via the same singleflight result.
2. Meta vs full dedup separation: concurrent meta and full requests for the same column produce two tasks, not one.
3. Pre-filter for cache hit: requesting an already-fully-loaded column produces no task and `SyncWaitStatsLoad` returns immediately.
4. Pre-filter drops uncached tables: requesting a column for a table not in the stats cache produces no task; caller sees no error and no result.
5. Per-batch timeout: batch with deadline T returns from `SyncWaitStatsLoad` within T + small slack, even when workers are blocked.
6. Per-task timeout drains to timeout channel: a task that ages past `ToTimeout` in `neededItemsCh` is moved to `timeoutItemsCh` and only handled when `neededItemsCh` is empty.
7. Queue-full timeout: filling `neededItemsCh` to capacity and then issuing a new `SendLoadRequests` produces `"sync load stats channel is full and timeout sending task to channel"` and the query falls into pseudo (with `tidb_stats_load_pseudo_timeout=true`).
8. Retry budget: a task failing via `mockReadStatsForOneFail` is retried at most `RetryCount` times, then `ResultCh` receives a result with `Error` set.
9. Panic recovery: `handleOneItemTaskPanic` causes the worker to log and continue; subsequent tasks succeed.
10. Inter-retry sleep is randomized: two consecutive failures produce sleeps in `[Lease/10, Lease/10 + 500µs]`, non-equal in expectation. Statistical test, not strict.
11. Per-task `mysql.HighPriority` is set and reset: inspect a task's session priority during `handleOneItemTask` and after.
12. Internal SQL uses `kv.PriorityHigh`: verify the storage-layer call sites use the high-priority variants.
13. `errGetHistMeta` swallow: a task whose `HistMetaFromStorageWithHighPriority` returns `(nil, nil)` succeeds without writing to the cache; only error types other than `errGetHistMeta` propagate.
14. `EmptyColumn` install for not-analyzed: a task whose column is `!analyzed` writes an `EmptyColumn` placeholder and does not contact storage.
15. Cache-update serialization: concurrent updates to the same table do not produce torn cache state.
16. Pseudo-on-timeout pathway: with `tidb_stats_load_pseudo_timeout=true`, batch-timeout produces a planner-visible pseudo result with no error and `SetSkipPlanCache` triggered. With `tidb_stats_load_pseudo_timeout=false`, the query errors.

### Deferred renames

Naming proposals captured during contract drafting. These are not yet decided and are deferred to a future refactor PR (Phase 4+, after the test safety net exists).

- `SendLoadRequests` -> `SendStatsLoadRequests`. Brings the method in line with `SyncWaitStatsLoad`, which already carries the "Stats" qualifier. Reads more clearly via the `Handle` proxy (`statsHandle.SendStatsLoadRequests(...)`), where the receiver name does not provide the missing context. Suggested 2026-05-15 by mjonss.

### Known cross-cutting issues

These are captured in `CURRENT-USAGE.md` Open Questions and remain unresolved:

- **Q13**: the gap between request and wait is CPU-only; the wait is the correctness mechanism, the overlap is a latency optimization.
- **Q14**: `PredicateSimplification` can shrink the predicate set after enqueue; affected stats requests become unused.
- **Q15**: the inverse, new column references introduced post-enqueue, is unverified.
- **Q16**: `ColumnStatsIsInvalid` keeps enqueueing into the async queue during the pseudo path, including after sync-load timeout.
- **Q17**: sync queue and async queue are independent with shared storage.
- **Q18**: async drainer is single-goroutine, no admission control; sync pool has retry/dedup/priority.
- **Q19**: no "cache too small" specialized handling; graceful-degradation primitives only.
- **Q20**: stats cache <-> InfoSchema seam fragility.
- **Q21**: stats cache lifetime can exceed InfoSchema LRU lifetime.
