# Current Usage of TiDB Statistics

Phase 0a deliverable. Comprehensive map of every place TiDB reads statistics, organized by what is read and when. Source data: four parallel directory-scope investigations preserved under `notes/caller-map-*.md` (planner-core, planner-cardinality, executor, external). This file is the synthesis; the notes files retain the per-call-site detail.

Scope: read sites only. Write paths (ANALYZE, LOAD STATS, LOCK STATS, DROP STATS, FLUSH STATS, RefreshStats, DML deltas) are listed for context but not analyzed for the Phase 0a contract effort. Stats history / time-travel is out of scope.

## Catalog of stat kinds the optimizer reads

What the code asks of the stats subsystem. Each entry lists the field or method, what it means, and the callers that depend on it.

### Table-level fields

`RealtimeCount` (current row count, includes uncommitted DML).
Universally read. Drives `CountAfterAccess` seeding (`pkg/planner/core/stats.go:169, 214, 272, 344, 358`), pseudo gating (`stats/stats.go:105`), sample-rate selection for ANALYZE (`pkg/executor/builder.go:3267`), TiFlash late-materialization size gate (`tiflash_predicate_push_down.go:266`), broadcast/exchange thresholds (`exhaust_physical_plans.go:1720`), cardinality `Selectivity` denominators (`pkg/planner/cardinality/selectivity.go:61`, many sites), `GetIncreaseFactor` growth-aware scaling, MV-index selectivity (`selectivity.go:484`), index estimation pseudo branches (`row_count_index.go:51`), out-of-range estimation, analyze-status remaining-duration estimation (`infoschema_reader.go:2647`), and remaining-rows estimation in pseudo paths.

`ModifyCount` (uncommitted modify delta).
Read for: high-modify penalty in cost ver2 (`plan_cost_ver2.go:1212, 1237`), `GetIncreaseFactor` scaling for column/index estimation (`row_count_column.go:161, 179`, multiple sites in `selectivity.go`), `GetScaledRealtimeAndModifyCnt`, plan-cache version matching (`logical_plan_builder.go:4710` indirectly), explain/slow-log emission (`stmtctx.go:1460-1528`), `getColumnRowCount` (`row_count_column.go:64`), cross-validation selectivity (`selectivity.go:1199`).

`Version` (table-level stats version timestamp).
Read by: plan-cache LastUpdateVersion comparison (`logical_plan_builder.go:4720-4727` iterates `ForEachColumnImmutable/ForEachIndexImmutable` LastUpdateVersion), point-get warm-up `LoadTableStats` (`stats/stats.go:149`), explain/slow-log (`stmtctx.UsedStatsInfoForTable`), `index_join_path.go:411` (`StatsVersion != PseudoVersion`).

`LastAnalyzeVersion` (the version stored at the last successful ANALYZE).
Read at: SHOW STATS_META (`show_stats.go:69-81`), used by stats Healthy calculation. Referenced as a TODO replacement for the LastUpdateVersion scan in `logical_plan_builder.go:4719`.

`Pseudo` flag (no real stats; estimates are synthesized).
Read everywhere there's a pseudo-tolerant branch. Examples: `stats/stats.go:94`, `find_best_task.go:865`, `find_best_task.go:1719`, `exhaust_physical_plans.go:530-540`, `plan_cost_ver2.go:1208`, `optimizer.go:1305-1306, 1336-1337`, `cross_estimation.go:83, 135`, `cardinality/row_size.go:65, 97`, `cardinality/selectivity.go` (multiple). The HistColl-level `Pseudo` is the most-consulted; the table-level `Pseudo` plus the `StatsVersion == PseudoVersion` check do the same job and are read interchangeably in different files (see Non-obvious below).

`IsInitialized()` / `IsAnalyzed()` / `IsOutdated()` (table-level capability flags).
Read at `stats/stats.go:111-112` (`stats.GetStatsTable` decides pseudo on un-initialized or outdated tables, gated by `EnablePseudoForOutdatedStats`) and `logical_plan_builder.go:4993` (`tblStats.IsAnalyzed()` decides dynamic vs static partition prune mode under `tidb_skip_missing_partition_stats`).

`GetAnalyzeRowCount()` (analyze-time row count, not live delta).
Read at `stats/stats.go:85` and `logical_plan_builder.go:4712` (OptObjectiveDeterminate path that uses analyze-time count instead of RealtimeCount), and at `plan_cost_ver2.go:1202` (`GetTableScanPenalty`).

`GetStatsHealthy()` (derived from RealtimeCount / ModifyCount).
Read at SHOW STATS_HEALTHY (`show_stats.go:484-496`).

`ColAndIdxExistenceMap` (which columns/indexes were analyzed at all).
Read by: `stats/stats.go:78` (panic-assert non-nil), `rule_collect_plan_stats.go:134, 162` (`HasAnalyzed(colID, false)` gate for sync load), `find_best_task.go:963, 970` (`HasAnalyzed(idxID, true)` in skyline pruning), `cardinality/trace.go:80` (distinguish "missing" vs "uninitialized"), `stats.go:555-557` (used-stats record).

`GetStatsHealthy`, `GetIncreaseFactor`, `GetScaledRealtimeAndModifyCnt`, `IsLastBucketEndValueUnderrepresented` are synthesized methods; included here because they are the call surface, not the underlying primitive fields.

### Column-level fields

`Histogram.NDV` (number of distinct values).
Equality-selectivity primary input (`row_count_column.go:74-89`, `selectivity.go:86-94` correlated columns, `row_count_index.go:298, 303-308` for single-col index out-of-range), V2 stale-bucket heuristic input (`selectivity.go:583`), `EstimateColumnNDV` (`ndv.go:40-50`, fallback to `RealtimeCount * 0.8`), `getColsNDVLowerBoundFromHistColl` (`exhaust_physical_plans.go:971-1016`, gated by `IsStatsInitialized`), SHOW INDEX cardinality column (`show.go:797`).

`Histogram.NullCount`.
`row_count_column.go:74-89`, `row_size.go:72, 110, 119-144` (avg row size includes null-aware adjustment), `selectivity.go:931-948, 1038` (TopN-assisted filter), `optimizer.go:1315` (overlong-type chunk sizing).

`Histogram.Buckets[i]` (Count, Repeat, Lower, Upper bounds).
V1 equal-row-count via bucket Repeat (`row_count_column.go:74-89`), V2 between-row-count (`row_count_column.go:243`, `betweenRowCountOnIndex`), out-of-range estimation (`row_count_column.go:224-230`), bucket-locate (`row_count_index.go:241-296`), TopN-assisted filter (`selectivity.go:964-1027` reads bucket Bounds and Repeat), SHOW STATS_BUCKETS (`show_stats.go:283-295`).

`Histogram.Correlation` (column vs handle order correlation).
Only read at `cross_estimation.go:273-277` (`getMostCorrCol4Handle`) for cross-estimation row-count adjustment, and SHOW STATS_HISTOGRAMS output.

`TopN`.
V2 equality (`row_count_column.go:95-117`, `equalRowCountOnIndex`), between-count (`row_count_column.go:243`, `row_count_index.go:241-296`), TopN-assisted filter eval (`selectivity.go:921-948, 964-1027`), out-of-range (`row_count_column.go:224-230`), SHOW STATS_TOPN (`show_stats.go:346-358`).

`CMSketch`.
V1 equality estimation for column (`row_count_column.go:74-89`) and index (`getEqualCondSelectivity` in `selectivity.go:1071`, V1 path in `row_count_index.go:73-174`). V2 has demoted CMSketch in favor of TopN+Histogram but the V1 path still reads it.

`TotalRowCount()` (sum of histogram buckets + TopN counts, used for stats-time scaling).
`ndv.go:56-79` (siblings matching by LastUpdateVersion), `row_count_index.go:73-174` (V1 estimation), `selectivity.go:1071` (V1 equal-cond), `row_size.go:119-144, 169-187`, `row_count_column.go:74-89` (V1).

`StatsVer` (per-column stats version 1 vs 2 vs 0).
Branches code paths between V1 (CMSketch-based) and V2 (Histogram+TopN). Read at every estimator entry: `row_count_column.go:74-89, 224-230, 243`; `row_count_index.go:64-69` (`idx.StatsVer == Version2`); `selectivity.go:931-948`. `StatsVer == Version0` is a synonym for "not yet analyzed" used as the pseudo-fallback trigger in async load (`PR #57723` per cross-reference notes).

`LastUpdateVersion`.
`logical_plan_builder.go:4720-4727` (plan-cache validation, max over all columns/indexes), `ndv.go:56-79` (`getTotalRowCount` matches siblings on this), SHOW STATS_HISTOGRAMS.

`IsStatsInitialized()`.
`getColsNDVLowerBoundFromHistColl` (`exhaust_physical_plans.go:971-1016` gates per-col/index), `EstimateColumnNDV` (`ndv.go:42`), `optimizer.go` overlong-type sizing.

`IsFullLoad()`.
Hard precondition for TopN-assisted filter (`selectivity.go:921-948, 1048, 1054-1057`), for the index-join NDV bound (`exhaust_physical_plans.go:971-1016` indirectly via IsStatsInitialized which on Stats V2 implies fullLoad), `getTotalRowCount` sibling match (`ndv.go:56-79`), and the "already loaded, skip request" gate (`rule_collect_plan_stats.go:165, 184`).

`IsEssentialStatsLoaded()`.
`stats.go:521-525` for getGroupNDVs.

`IsHandle`.
`row_size.go:72, 119-144`, `optimizer.go:1310`, `selectivity.go:115` (handle column gets special path).

`NotNullCount()`, `TotColSize`, `StatsLoadedStatus`, `OutOfRange()`, `OutOfRangeRowCount()`, `EqualRowCount()`, `BetweenRowCount()`.
All read inside the per-column / per-index estimators (`row_count_column.go`, `row_count_index.go`). The status field is also rendered by `trace.go:33-46` and `stmtctx.UsedStatsInfoForTable`.

### Index-level fields

Mirror most of the column-level set: `NDV`, `NullCount`, `Histogram` (buckets, NDV, NullCount, OutOfRangeRowCount, Bounds, Len), `TopN`, `CMSketch`, `StatsVer`, `LastUpdateVersion`, `IsStatsInitialized`, `IsFullLoad`. Read by the V1 / V2 index range estimators and SHOW commands.

Index-only: `Info.Unique`, `Info.MVIndex`, `Info.Columns`, `Info.ConditionExprString`, `Info.Columns[0].Length` (gates TopN-assisted filter eligibility). These are schema fields ridden along on the `*statistics.Index` object; they are not "stats" but they live on the stats struct.

### HistColl-level fields and methods

`HistColl` is the planner-local stats container (columns + indexes for one physical table, keyed by UniqueID). Plan trees carry it as `TableStats.HistColl`, `TblColHists`, or `StatsInfo.HistColl`.

`ColNum()`, `IdxNum()`.
Header gates in `selectivity.go:69`, `row_size.go:65, 97`, `optimizer.go:1305-1306, 1336-1337`.

`GetCol(uniqueID)`, `GetIdx(idxID)`.
Universal in cardinality. The lookup that resolves a planner-side column/index ID to its stats payload.

`Idx2ColUniqueIDs`, `ColUniqueID2IdxIDs`, `UniqueID2colInfoID`, `MVIdx2Columns`.
Inverted maps used to walk the column-vs-index relationships during estimation (`row_count_index.go:73-174, 445`, `selectivity.go:153, 1178-1199`, `cross_estimation.go:167`). `Idx2ColUniqueIDs` is also mutated in place at `pkg/planner/core/stats.go:190-191` (the only "write" in the read map; mutating a locally-constructed HistColl, not the shared cache).

`ForEachColumnImmutable`, `ForEachIndexImmutable`, `StableOrderColSlice`, `StableOrderIdxSlice`.
Iteration helpers used to scan all loaded stats for plan-cache validation, SHOW commands, and stable display order.

`GetScaledRealtimeAndModifyCnt(idx)`.
Scaled real-time and modify count for an individual index (compensates for index lag behind table updates).

`Pseudo`, `RealtimeCount`, `ModifyCount` (same shapes as table-level, replicated on HistColl).

### Synthesized helpers used at the call site

`AvgColSize`, `AvgColSizeChunkFormat`, `AvgColSizeDataInDiskByRows`, `GetAvgRowSize`, `GetTableAvgRowSize`, `GetIndexAvgRowSize`, `GetAvgRowSizeDataInDiskByRows`, `EstimateColumnNDV`, `EstimateColsNDVWithMatchedLen`, `EstimateColsDNVWithMatchedLenFromUniqueIDs`, `EstimateFullJoinRowCount`, `Selectivity`, `CalcTotalSelectivityForMVIdxPath`, `AdjustRowCountForTableScanByLimit`, `AdjustRowCountForIndexScanByLimit`, `GetRowCountByColumnRanges`, `GetRowCountByIndexRanges`, `PseudoAvgCountPerValue`, `PseudoTable`, `PseudoHistColl`, `AnalyzeVersionMatchesForTableStats`.

These are reads via a function call. They each internally read some subset of the primitives above.

## Stats-load entry points

Every code path that triggers a stats load, grouped by mechanism. Eight categories. The first two ("Sync load" and "Async load enqueue") are the heaviest in volume; the rest fire less often or at well-defined times. Sections later in this document expand on init/bootstrap, background workers, and auxiliary surfaces.

### 1. Sync load (planner blocking)

- `pkg/planner/core/rule/rule_collect_plan_stats.go:350` `SendLoadRequests` (rule position 16). Batched request emission with a deadline from `StatsLoadSyncWait` (optionally capped by `max_execution_time`).
- `pkg/planner/core/rule/rule_collect_plan_stats.go:375` `SyncWaitStatsLoad` (rule position 22). Blocking wait. Returns after all items load, on timeout (pseudo fallback if `StatsLoadPseudoTimeout`), or on error.

Outside these two rules, no caller issues sync-load requests. The worker pool lives in `pkg/statistics/handle/syncload/`.

### 2. Async load enqueue (fire-and-forget into `asyncload.AsyncLoadHistogramNeededItems`)

The async-load queue is a global package-level map: `pkg/statistics/asyncload/async_load.go:25`. Two kinds of enqueue sites:

**Explicit from the planner rule:**
- `pkg/planner/core/rule/rule_collect_plan_stats.go:101` when `syncWait <= 0`, every collected item is pushed to the async queue.
- `pkg/planner/core/rule/rule_collect_plan_stats.go:195` `markAtLeastOneFullStatsLoadForEachTable` unconditionally enqueues its picked column (regardless of sync mode).

**Side-effect from cardinality estimators.** Every call to `statistics.ColumnStatsIsInvalid` / `IndexStatsIsInvalid` reaches `pkg/statistics/column.go:149` / `pkg/statistics/index.go:128`, which calls `AsyncLoadHistogramNeededItems.Insert` when the stats are absent or not full-loaded. Call sites:

- `pkg/planner/cardinality/row_count_column.go:45`
- `pkg/planner/cardinality/row_count_index.go:46, 471, 488, 634`
- `pkg/planner/cardinality/cross_estimation.go:208, 216`
- `pkg/planner/cardinality/selectivity.go:88, 140, 1048, 1055, 1186`
- `pkg/planner/cardinality/pseudo.go:53, 87` (nil-arg form; pure side-effect)

The first argument to `ColumnStatsIsInvalid` / `IndexStatsIsInvalid` does not change the enqueue semantics; nil and non-nil both enqueue if the stats are missing or partial. The comment at `selectivity.go:140-141` notes the path could be removed if async load were removed.

### 3. Async load drain (single background worker)

- `pkg/domain/domain.go:2124` `statsHandle.LoadNeededHistograms(do.InfoSchema())`. The `asyncLoadHistogram` worker ticks on `statsLease` and drains `AsyncLoadHistogramNeededItems` via this call.

Drain mechanics in `pkg/statistics/handle/storage/read.go:574-620`: walks the queue, loads each item, deletes it from the queue on success.

### 4. Init load (server start and admin)

- `pkg/domain/domain.go:2059` `InitStatsLite(ctx)` (lite mode at bootstrap; only meta + col/idx existence).
- `pkg/domain/domain.go:2061` `InitStats(ctx, do.InfoSchema())` (full mode at bootstrap).
- `pkg/executor/simple.go:2862-2870` admin `RefreshStats` command. Branches on lite vs full and on whole-instance vs specific table list.

Conditions and gating: see "Init / bootstrap" section.

### 5. Incremental refresh from `mysql.stats_meta` version bumps

- `pkg/domain/domain.go:2093` `statsHandle.Update(ctx, do.InfoSchema())`. `loadStatsWorker` ticker, runs every `statsLease`.
- `pkg/executor/analyze.go:395` `statsHandle.Update(ctx, infoSchema, tableAndPartitionIDs...)` after ANALYZE completes.

`Update` reads `mysql.stats_meta` versions and reloads tables whose versions bumped. Not a "load histograms" call exactly, but it pulls fresh data from storage when a version mismatch is detected.

### 6. JSON-based loads (bypass queues, write directly into cache)

- `pkg/executor/load_stats.go:95` `LoadStatsFromJSON` (LOAD STATS SQL).
- `pkg/executor/plan_replayer.go:493` `LoadStatsFromJSON` (PLAN REPLAYER LOAD).
- `pkg/testkit/testkit.go:794` `LoadStatsFromJSON` (test helper).

### 7. Targeted single-row reads from storage

- `pkg/executor/builder.go:3160` `StatsMetaCountAndModifyCount(tid)` (ANALYZE sample-rate setup). Also called from various internal callers in `pkg/statistics/handle/ddl/` and `pkg/statistics/handle/globalstats/`.
- `pkg/executor/show_stats.go:538` `LoadColumnStatsUsage(loc)` reads `mysql.column_stats_usage`.
- `pkg/executor/infoschema_reader.go:656, 715, 1325-1326, 1372-1373` `cache.TableRowStatsCache.UpdateByID` / `GetTableRows` / `GetDataAndIndexLength` / `EstimateDataLength`. The parallel cache for `INFORMATION_SCHEMA.TABLES` / `PARTITIONS`. `UpdateByID` does a synchronous batch read from `mysql.stats_meta` and `mysql.stats_histograms`.

### 8. Snapshot / historical reads

- `pkg/server/handler/optimizor/statistics_handler.go:74` `DumpStatsToJSON` (HTTP `/stats/dump/{db}/{table}`).
- `pkg/server/handler/optimizor/statistics_handler.go:132` `DumpHistoricalStatsBySnapshot` (HTTP `/stats/dump/.../{snapshot}`).
- `pkg/server/handler/optimizor/plan_replayer.go:240` `DumpHistoricalStatsBySnapshot` (capture file download).
- `pkg/domain/plan_replayer_dump.go:858, 860` `DumpHistoricalStatsBySnapshot` / `DumpStatsToJSON` (replayer dump).

## Pseudo-tolerant cardinality reads

By far the bulk of all read sites. Every caller in `pkg/planner/cardinality/` and most callers in `pkg/planner/core/` operate on a `*statistics.HistColl` they have already obtained, and have an internal branch for the pseudo case (`coll.Pseudo`, `StatsVersion == PseudoVersion`, or a nil per-column lookup). Output: either a real estimate using histograms/TopN/CMSketch/NDV, or a `pseudoSelectivity` / `pseudoColSize` / `SelectionFactor` fallback that consumes only `RealtimeCount` and schema info.

This is the access pattern that defines "what the planner needs from stats": every estimator can degrade gracefully, but every estimator becomes more accurate when full histograms are loaded.

## Must-be-loaded gates

A small set of sites that branch on a hard "is this fully loaded" check and either use the stats or skip the optimization entirely.

- `pkg/planner/core/exhaust_physical_plans.go:971-1016` (`getColsNDVLowerBoundFromHistColl`): each column/index requires `IsStatsInitialized()`. Missing items contribute -1, dropping the NDV upper-bound clamp.
- `pkg/planner/cardinality/selectivity.go:921-948, 1048, 1054-1057` (`GetSelectivityByFilter` / `findAvailableStatsForCol`): requires `IsFullLoad` because it walks histogram buckets and TopN entries directly. Otherwise returns `ok=false` and the caller uses a default string-match selectivity.
- `pkg/planner/cardinality/ndv.go:56-79` (`getTotalRowCount`): walks siblings for one with `IsFullLoad` and matching `LastUpdateVersion`. Returns 0 if no match.
- `pkg/planner/core/stats.go:521-525` (`getGroupNDVs`): requires `IsEssentialStatsLoaded`.
- `pkg/planner/core/rule/rule_collect_plan_stats.go:165, 184`: reads `IsFullLoad` to decide "is this column/index already loaded; if so, do not request it."

Each of these is a place where the cardinality model could be improved if the data were present, but the lack of data does not stall the query.

## Admin / observability surfaces

User-visible "what does TiDB know about my stats" commands.

`SHOW INDEX` (`show.go:797`), `SHOW STATS_META` (`show_stats.go:69-81`), `SHOW STATS_HISTOGRAMS` (`:202-210`), `SHOW STATS_BUCKETS` (`:283-295`), `SHOW STATS_TOPN` (`:346-358`), `SHOW STATS_HEALTHY` (`:484-496`), `SHOW STATS_LOCKED` (`:169`), `SHOW COLUMN_STATS_USAGE` (`:538`), `SHOW HISTOGRAMS_IN_FLIGHT` (`:519`), `SHOW ANALYZE STATUS` / `IS.ANALYZE_STATUS` (`infoschema_reader.go:2580, 2647`).

`INFORMATION_SCHEMA.TABLES` / `PARTITIONS` row-count/data-length columns (`infoschema_reader.go:656, 715, 1325-1326, 1372-1373`): served from a **separate** `cache.TableRowStatsCache`, parallel to the main `StatsHandle`. This is one of the more interesting findings; the contract for "reading stats" needs to either cover this second cache or explicitly delegate it.

`INFORMATION_SCHEMA.TIDB_INDEX_USAGE` (`infoschema_reader.go:4022`): served by `GetIndexUsage(tbl.ID, idx.ID)`. Auxiliary stats, independent of histograms.

`SHOW HISTOGRAMS_IN_FLIGHT` reads via `statsStorage.CleanFakeItemsForShowHistInFlights`, which sweeps stale singleflight tombstones and returns a count. Function name says "Clean" but the executor uses it as a read.

## Stmt-context snapshots (UsedStatsInfo)

A separate "session-local" stats surface: the planner captures `Version`, `RealtimeCount`, `ModifyCount`, `ColumnStatsLoadStatus`, `IndexStatsLoadStatus` per accessed table into `stmtctx.UsedStatsInfoForTable` (`pkg/sessionctx/stmtctx/stmtctx.go:1460-1528`). After the statement executes, this snapshot is read by:

- The slow-log builder (`adapter_slow_log.go:263`).
- The plan-replayer continuous-capture dumper (`adapter.go:2594`).
- The index-usage reporter (`builder.go:4778`, `internal/exec/indexusage.go:120`), which uses `RealtimeCount` to bucket index hits.
- Explain output (`UsedStatsInfoForTable.FormatForExplain`).

The contract guarantee callers depend on, which is not currently documented anywhere, is: the snapshot is stable from the moment the planner finishes until the executor returns, even if the underlying cache rotates during execution. Today this is enforced because the snapshot lives on the `StatementContext` and is immutable post-planning.

## Init / bootstrap

`pkg/domain/domain.go:1930` `(*Domain).UpdateTableStatsLoop` is called once per server start from `BootstrapSession`. It constructs the `Handle`, calls `StartWorker`, registers with DDL.

`pkg/domain/domain.go:2038` `(*Domain).initStats` is the single decision point for full vs lite init:

- If `config.Performance.SkipInitStats`: close `InitStatsDone` immediately, return. No cache populated.
- Else if `config.Performance.LiteInitStats`: call `statsHandle.InitStatsLite(ctx)` (meta + col/idx existence only, no histograms).
- Else: call `statsHandle.InitStats(ctx, do.InfoSchema())` (full load).
- On error: log and continue. `InitStatsDone` is closed in the deferred recover.

`pkg/domain/domain.go:2087` `loadStatsWorker` runs `initStats` then enters a lease-tick loop calling `statsHandle.Update`. Launched only if `do.statsLease >= 0`.

`pkg/server/server.go:483` is the only place outside domain that gates startup: if `Performance.ForceInitStats`, the server `run()` blocks on `<-dom.StatsHandle().InitStatsDone` before opening the listener. Without `ForceInitStats`, early queries can land on pseudo or partially-loaded stats and incur sync-load on first reference.

`pkg/domain/domain.go:1970, 2115` `waitStartTask` and `asyncLoadHistogram` gate background workers (gc, auto-analyze, cleanup) on `InitStatsDone`.

## Background workers (operate on the cache, no per-query reads)

Listed for context; these are not query-time read sites.

- `loadStatsWorker` (`domain.go:2087`): periodic `statsHandle.Update` to refresh from `mysql.stats_meta` version bumps.
- `asyncLoadHistogram` (`domain.go:2115`): drains `LoadNeededHistograms`.
- `indexUsageWorker` (`domain.go:2137`): GC index-usage counters.
- `gcStatsWorker` (`domain.go:2205-2233`): `GCStats`, `StatsCache.TriggerEvict` on memory pressure, `UpdateStatsHealthyMetrics`, `CheckAutoAnalyzeWindows`. The TriggerEvict is the one place outside the handle that directly touches the `StatsCache` field (everywhere else goes through `Handle` methods).
- `dumpColStatsUsageWorker`, `deltaUpdateTickerWorker`, `autoAnalyzeWorker`, `analyzeJobsCleanupWorker`, `HistoricalStatsWorker` (out of scope but listed).

## Auxiliary stats surfaces

Stats-shaped data that lives outside the main `*statistics.Table` cache.

`cache.TableRowStatsCache` for `INFORMATION_SCHEMA.TABLES` / `PARTITIONS`. Parallel to `StatsHandle`. Pulls row-count / data-length / index-length aggregates from `mysql.stats_meta` and `mysql.stats_histograms` on demand.

Index-usage counters (`GetIndexUsage`, `IndexUsageReporter`). Read at `INFORMATION_SCHEMA.TIDB_INDEX_USAGE` and after every executor Close.

Column-stats usage (`LoadColumnStatsUsage`, `DumpColStatsUsageToKV`). Read at `SHOW COLUMN_STATS_USAGE`; written by `dumpColStatsUsageWorker`. Auxiliary to autoanalyze.

Lock status (`GetLockedTables`). Read at `SHOW STATS_LOCKED` and to filter ANALYZE targets at `pkg/executor/analyze.go:496`.

## Stats-bypass call sites

Places that synthesize stats locally without consulting the cache.

`pkg/planner/core/planbuilder.go:1803-1875`: `buildPhysicalIndexLookUpReader` constructs `statistics.PseudoHistColl(physicalID, false)` for `ADMIN CHECK INDEX` / `INDEX LOOKUP` outside the normal DataSource path. Deliberate bypass.

`pkg/planner/core/operator/logicalop/logical_mem_table.go:189`: `statistics.PseudoTable(...)` for memtables. Definitional, memtables never have real stats.

`pkg/table/tables/tables.go:1924`: `TemporaryTable.stats = statistics.PseudoTable(tblInfo, false, false)`. Only direct `*statistics.Table` materialization outside `pkg/statistics/**` and `pkg/planner/**`.

## Non-obvious findings

1. **Two pseudo signals coexist.** `HistColl.Pseudo` (struct field) and `StatsVersion == statistics.PseudoVersion` (numeric comparison) are used interchangeably depending on the file. `find_best_task.go:865` uses the first; `index_join_path.go:411` uses the second with a comment referencing issue #63869. Tests should treat both as equivalent; refactors should consolidate. The `*property.StatsInfo` carries the version, the `*statistics.HistColl` carries the bool, and `stats.go:545-547` sets both on the pseudo path.

2. **`GetPhysicalTableStats` is called twice per visited table** in `rule_collect_plan_stats.go:130, 152`. Two snapshots can in principle differ if the cache rotates between calls. Worth a test.

3. **`getLatestVersionFromStatsTable` is a parallel implementation** of `stats.GetStatsTable` for plan-cache version matching (`logical_plan_builder.go:4690-4729`). It does not increment pseudo metrics, does not copy the table, and uses `ForEachColumnImmutable`/`ForEachIndexImmutable` over `LastUpdateVersion` instead of the documented-as-replacement `LastAnalyzeVersion`. Candidate for unification.

4. **`cache.TableRowStatsCache` is a parallel cache** queried only by INFORMATION_SCHEMA. It reads `mysql.stats_meta`/`mysql.stats_histograms` directly and lives alongside the main `StatsHandle` cache. A "stats reading" contract needs to either cover or explicitly exclude it.

5. **`ddlCtx.statsHandle` and `executor.statsHandle` are stored but never dereferenced** in non-test code. DDL/stats interaction routes through the `pkg/ddl/notifier/` subscription registered by `pkg/statistics/handle/...`. Whether `RegisterStatsHandle` should remain on the `DDL` interface is an open question.

6. **`Domain.gcStatsWorker` reaches into `StatsCache.TriggerEvict()` directly**. Every other call site outside the handle goes through `Handle` methods. The encapsulation has one documented hole.

7. **Server listener does not gate on stats by default.** Only `Performance.ForceInitStats=true` blocks the listener; without it, early queries see pseudo stats and incur sync load on first reference. Production deployments should be aware.

8. **Side-effect-only invalidity probes.** `pseudo.go:53, 87` and `selectivity.go:140-141` call `ColumnStatsIsInvalid(nil, ...)` / `IndexStatsIsInvalid(... nil ...)` purely to enqueue sync-load requests for missing columns. The first argument is intentionally nil. An inline comment notes this path "could be removed if async load were removed."

9. **`HistColl.Idx2ColUniqueIDs` is mutated by the planner** at `stats.go:190-191`. The HistColl is locally constructed by `GenerateHistCollFromColumnInfo`, so this is read-side enrichment, not a cache write, but it muddies the "stats are immutable to readers" claim.

10. **`InitStats` errors are swallowed.** `Domain.initStats` logs and continues on failure. Partial init leaves some tables pseudo and others not. Whether `Handle.Update` later self-repairs missing histograms is unverified.

11. **`Performance.SkipInitStats` (skip-grant-table mode) leaves the cache empty.** All queries serve pseudo until manual `RefreshStats`.

12. **`PointGetPlan.LoadTableStats` warm-up** at `stats/stats.go:149` runs at executor build time for plan-cache point-gets that bypassed the planner stats-load step. Point-get also passes `loadStats=false` to `buildIndexUsageReporter`, the only such divergence in executor code.

## Open questions for the contract phase

Carried forward from per-directory open questions, deduplicated:

1. Contract for `GetPhysicalTableStats` return: always non-nil with `Pseudo=true` fallback, or callers must nil-check? Different files do different things.
2. Are `HistColl.Pseudo` and `StatsVersion == PseudoVersion` guaranteed equivalent in all code paths, or is the divergence at `index_join_path.go:411` load-bearing?
3. Should `cache.TableRowStatsCache` be in the contract or out?
4. Should `LoadColumnStatsUsage` and `GetIndexUsage` (auxiliary stats) be in the same contract as the histogram cache, or a separate one?
5. Does the contract guarantee `UsedStatsInfo` snapshot stability across executor lifetime?
6. Should `Domain.initStats` propagate errors instead of swallowing them?
7. What is the documented behavior of partial init failure (some tables loaded, others not) for downstream readers?
8. Is the dual `GetPhysicalTableStats` call in `rule_collect_plan_stats.go:130, 152` intentional?
9. Can `getLatestVersionFromStatsTable` be unified with `stats.GetStatsTable`?
10. Should `LastAnalyzeVersion` replace the manual `LastUpdateVersion` scan in `logical_plan_builder.go:4719`?
11. Is the `gcStatsWorker.TriggerEvict` direct-field access a layering violation worth fixing?
12. Should `RefreshStats`, `LOAD STATS`, and `DROP STATS` cache-invalidation semantics be in the contract or a sibling document?
13. The planner gap between `CollectPredicateColumnsPoint` (rule position 16, emits the load request) and `SyncWaitStatsLoadPoint` (rule position 22, blocks) is occupied by five intermediate rules (`AggregationPushDownSolver`, `DeriveTopNFromWindow`, `PredicateSimplification`, `PushDownTopNOptimizer`, `OrderAwareJoinReorder`), all of which are pure in-memory tree rewrites with no IO, no stats-handle calls, no channel waits, no `time.Sleep`. The "overlap" between request and wait is therefore bounded by local CPU only, not by IO. Consequences: (a) the sync-load TiKV roundtrip dominates and `SyncWaitStatsLoadPoint` almost always genuinely blocks; (b) the overlap benefit scales with plan complexity (significant for many-way joins, near-zero for simple queries); (c) no invariant guarantees that stats are loaded by the time the wait rule runs, the wait itself is the correctness mechanism. Question for the contract: should it codify "the blocking wait is the load-completion mechanism, the overlap is a latency optimization with no guarantee," and should anything about the gap composition (CPU-only) be part of the contract?

14. Of the five intermediate rules between 16 and 22, only `PredicateSimplification` (`pkg/planner/core/rule/rule_predicate_simplification.go`) can shrink the set of columns/indexes that drive stats requests. It simplifies predicates inside DataSource `PushedDownConds`/`AllConds` (`pkg/planner/core/operator/logicalop/logical_datasource.go:312-313`) via `mergeInAndNotEQLists`, `pruneEmptyORBranches`, `shortCircuitLogicalConstants` (line 422 can produce `FALSE`). However, `CollectPredicateColumnsPoint` keys stats requests at the **column** level, not the predicate-expression level: a column triggers one request regardless of how many predicates reference it. So the shrinkage is "wasted" only in the narrow case where the simplified-away predicate was the **only** predicate touching that column. Subtree elimination to `LogicalTableDual` does not happen in this gap; `PredicatePushDown` already ran at position 13, and the rules that turn `FALSE` predicates into `LogicalTableDual` run later (after the wait). Question for the contract: should it explicitly say "stats load requests are best-effort; some may become unused due to later simplification, and that is acceptable"? Implementation today already behaves this way; the question is whether to commit to it.

15. The inverse of question 14: can the five intermediate rules introduce **new** column or index references for which no stats request was sent? `AggregationPushDownSolver` is the main candidate, since pushing aggregation below a join elevates the importance of join-key NDV; but the join keys are already in the predicate set from the join condition itself, so the new emphasis lands on columns already requested. This is plausible-but-unverified. If verified, the contract should either (a) commit to "the set of columns needing stats can only shrink across the gap" as an invariant, or (b) note that planning past the wait point may consult stats for columns that were never sync-loaded and explicitly accept the pseudo path for those.

16. `ColumnStatsIsInvalid` (`pkg/statistics/column.go:149`) and `IndexStatsIsInvalid` (`pkg/statistics/index.go:128`) **always enqueue** into `AsyncLoadHistogramNeededItems` when the stats are absent or not full-loaded, regardless of whether the first argument is nil or a real pointer. Consequence: when a query times out on sync load and the planner falls into the pseudo path, every cardinality estimator that runs during pseudo estimation re-enqueues the same items into the async queue. The next `LoadNeededHistograms` drain then loads them, possibly after the query that needed them has already returned. Net effect: the failed sync load contributes to the next query's success, but at the cost of repeated enqueue churn for the failing query. Question for the contract: is this intended (graceful self-healing across queries), redundant work (the queue already had the items from `rule_collect_plan_stats.go:101`), or a latent issue (uncontrolled enqueue under timeout pressure)? Tests should pin the behavior.

17. Two stats-load queues exist with **independent semantics but shared storage**: the sync-load queue with its own worker pool in `pkg/statistics/handle/syncload/` (5-10 workers, two-channel priority model, singleflight dedup, retry, timeout), and the async-load queue `asyncload.AsyncLoadHistogramNeededItems` drained by the single `asyncLoadHistogram` goroutine at `pkg/domain/domain.go:2124` (no priority, no explicit retry, one worker, ticks on `statsLease`). Both ultimately read from `mysql.stats_*` system tables. Question for the contract: are the two queues intentionally distinct (sync = blocking with deadlines, async = best-effort background), and if so should the contract explicitly state their roles and forbid cross-queue interaction? Or should they eventually be unified? Reading `cardinality/selectivity.go:140-141` comment ("could be removed if async load were removed") suggests the async queue is historical and partial removal has been considered.

18. The async drainer is a **single goroutine doing sequential work**, and the queue is **unbounded with no admission control**. `pkg/domain/domain.go:2103-2132` runs one `asyncLoadHistogram` goroutine that ticks on `statsLease` and calls `LoadNeededHistograms`. The drain at `pkg/statistics/handle/storage/read.go:574-598` is a plain `for` loop over `AsyncLoadHistogramNeededItems.AllItems()`, one item at a time, no parallelism. No retry: `read.go:629` explicitly says "load the histogram for each column at most once in async load, as we already have a retry mechanism in the sync load," and items are deleted from the queue unconditionally (`:630`). Meanwhile, every `ColumnStatsIsInvalid` / `IndexStatsIsInvalid` call (13+ planner cardinality sites) keeps inserting into the queue with no backpressure. Combined with the self-enqueueing loop in Open Q16 (sync-load timeout -> pseudo path -> cardinality re-enqueues the same items), the queue can grow arbitrarily and the single drainer can fall arbitrarily behind. Contrast with sync load: 5-10 workers, parallel consumption, retry, deadlines, singleflight dedup, two-channel priority, `kv.PriorityHigh` for internal SQL. The async path uses `kv.PriorityNormal` (`read.go:571`). Question for the contract: should it bound the async queue depth, the async drain latency, or both? Or formalize "async load is best-effort with no completion guarantee" and accept that under sustained pressure the queue becomes unbounded? The contract should at least name the asymmetry so future refactors do not silently assume the two paths have similar throughput characteristics.

19. **No explicit "cache too small" handling**, only generic graceful-degradation primitives that compose into a stable-but-degraded steady state. What exists: bounded sync-load queue (`StatsLoadQueueSize`, `stats_syncload.go:101-102`) with queue-full -> pseudo fallback at `:143-145`; per-query sync-load timeout (`tidb_stats_load_sync_wait`) with pseudo fallback governed by `tidb_stats_load_pseudo_timeout` (default `true`, `tidb_vars.go:1633`); singleflight dedup of concurrent demand on the same item (`stats_syncload.go:125`); memory-driven eviction by `gcStatsWorker` calling `StatsCache.TriggerEvict()` after `memory.ForceReadMemStats()` (`domain.go:2231`); LFU memory ceiling via `tidb_stats_cache_mem_quota` (default 0 = ~20% of RAM, `enable-stats-cache-mem-quota=true`); LFU admission control via Ristretto (new items may be rejected); `Performance.LiteInitStats=true` at boot to reduce initial memory; `CanNotTriggerLoad` flag on `*statistics.Table` for definitionally-no-stats tables (`table.go:232, 1011`). What is missing: no coalescing between LFU eviction and in-flight loads (a freshly-loaded item can be evicted before the next reader observes it, forcing reload); no thrashing detector (nothing measures "most queries miss the cache and reload the same items"); no per-item failure backoff (a column that fails to load is retried by every query touching it after `RetryCount` exhausts); no throttle on the `markAtLeastOneFullStatsLoadForEachTable` supplement (`rule_collect_plan_stats.go:195` enqueues one column per visited table every query, regardless of queue depth); no quota-aware admission on the sync queue (capacity is a fixed channel buffer; under pressure the queue fills and individual tasks fail without prioritization). Net behavior under "cache too small + many concurrent queries": sync queue fills -> queue-full errors -> pseudo for affected queries; async queue grows unbounded (see Open Q18) -> single drainer cannot catch up; LFU admission rejects new items -> successful loads vanish before reuse -> next query reloads; reactive eviction by `gcStatsWorker` further reduces hit rate. The fallback is graceful (queries succeed with pseudo) but there is no positive feedback loop that would stabilize the situation; operator intervention (grow `tidb_stats_cache_mem_quota`, reduce concurrent load) is the only path back to a hit-cache regime. Question for the contract: should it name the steady-state guarantees under sustained cache pressure (e.g. "queries always complete with pseudo at worst; load throughput becomes operator-bounded, not system-bounded"), or should it specify a positive-feedback mechanism (admission control, per-item backoff, thrashing detection) that future versions must implement?

20. **The stats cache has a non-trivial dependency on InfoSchema; the seam is fragile and has motivated four PRs in nine months** (#51911, #54514, #54531, #57803). The connections: (a) DDL notifier subscription at `pkg/statistics/handle/handle.go:183` wires the stats subscriber to `notifier.StatsMetaHandlerID`; events like CREATE / TRUNCATE / DROP / ADD COLUMN / EXCHANGE PARTITION mutate the stats cache via `pkg/statistics/handle/ddl/subscriber.go:49-...` (`insertStats4PhysicalID`, `delayedDeleteStats4PhysicalID`, `updateGlobalTableStats4ExchangePartition`, etc.). (b) Several stats-handle methods take `infoschema.InfoSchema` as a parameter: `InitStats`, `Update`, `LoadNeededHistograms`, `loadNeededIndexHistograms`, `GCStats`, `LoadStatsFromJSON`; only `InitStatsLite` was reworked (PR #54514) to avoid this. (c) The planner sync-load worker falls back to `GetLatestInfoSchema().TableInfoByID` when `ColAndIdxExistenceMap` lacks the column (`rule_collect_plan_stats.go:359-388`, PR #54531). (d) Stats handle keeps a schema-version-pinned cache `pid2tid` at `pkg/statistics/handle/util/table_info.go:46-93`, invalidated when `is.SchemaMetaVersion()` changes; the field name `forInitStatsAndInfoSchemaV1Only` suggests this is a v1 workaround. (e) `ColAndIdxExistenceMap` is a stats-side mirror of "which columns/indexes exist and were analyzed"; drift between this and InfoSchema is the root cause of the four cited PRs. (f) Memtables bypass the cache entirely via `PseudoTable(tblInfo, false, false)` in `logical_mem_table.go:189`. (g) `GCStats(is, ddlLease)` (`storage/gc.go:53`) is the reactive reconciliation path: it scans InfoSchema and removes stats for IDs no longer present, defensively handling missed DDL events. InfoSchema does not subscribe to or query the stats handle; the dependency is one-directional. Question for the contract: should it specify ordering / completeness invariants for this seam: DDL events processed in commit order; `ColAndIdxExistenceMap` is at least as complete as InfoSchema for analyzed columns; every read of the existence map must have an InfoSchema fallback path; `pid2tid` invalidates on every schema version change; `GCStats` eventually converges stats with InfoSchema even if the subscriber misses an event; memtables and temp tables must not have stats cache entries?

21. **The stats cache and the InfoSchema v2 LRU have independent lifecycles; stats entries can outlive table-metadata cache entries, and a fresh stats load only needs InfoSchema to know the table exists, not to have it currently cached.** Specifically: (a) **Can we load a fresh stats entry without the table being in the table cache?** Partially yes. Every load path resolves metadata via `statsHandle.TableInfoByID(is, id)` -> InfoSchema v2's `TableByID` at `pkg/infoschema/infoschema_v2.go:860-902`. The flow is: `searchTableItemByID` checks whether the table is **registered** in the schema (true / false). If true and `tableCache.Get(key)` misses (LRU evicted), `loadTableInfo` (line 893) lazy-loads from storage; the metadata is returned for the current request but, per the comment at line 858-859, **the LRU is not refilled** unless the caller passes `WithRefillOption`. So a stats load via `TableInfoByID` can succeed against an evicted-from-LRU table, but cannot succeed if the table is not registered in the schema at all. Once a stats entry exists in the LFU cache, `h.Get(physicalTableID)` at `handle.go:215` serves it without touching InfoSchema. (b) **Can we evict from the table cache and still keep the stats entry?** Yes, trivially. The two caches share no coordination: InfoSchema's LRU eviction is invisible to the stats handle, and the stats LFU's eviction is invisible to InfoSchema. After InfoSchema evicts a `TableInfo`, subsequent stats reads either bypass InfoSchema (cache hit on the stats LFU) or trigger a lazy reload of the metadata. Subtle drift scenarios: (b1) DDL between InfoSchema eviction and the next stats access mutates the stats entry's `ColAndIdxExistenceMap` via the subscriber; the InfoSchema lazy-reload returns the new `TableInfo`. Between DDL commit and subscriber processing, the stats entry can have an existence map inconsistent with the new schema. (b2) InfoSchema v2's LRU is keyed by `(tableID, schemaVersion)` (line 886), so a schema bump creates a new LRU slot and the old one becomes evictable; stats cache entries are **not** keyed by schema version, so they persist across schema bumps without explicit invalidation. (b3) The stats-handle-local `pid2tid` map is the only stats-side structure that invalidates on schema version change (`pkg/statistics/handle/util/table_info.go:92`); `ColAndIdxExistenceMap` and the stats payloads themselves rely entirely on the DDL subscriber for schema updates. Questions for the contract: should it specify (i) "no stats entry exists for a table not registered in InfoSchema" as a strong invariant, enforced eagerly rather than only via `GCStats`'s reactive reconciliation; (ii) "stats entries can outlive InfoSchema LRU entries; any tableID present in the stats cache must always be resolvable via InfoSchema lazy reload" as a stated cross-cache property; (iii) "a schema version bump does not invalidate stats entries; column-level drift relies on the DDL subscriber, and `GCStats` does not reconcile column-level differences"? Test target: a chaos scenario that (1) loads a stats entry, (2) forces InfoSchema LRU eviction of the table, (3) issues a DDL that drops a column, (4) verifies the stats entry's `ColAndIdxExistenceMap` matches the new schema after the subscriber runs and not before.

22. **Phase 0a only mapped what the optimizer currently reads (supply side). We have not analyzed what it would need if we could provide anything (demand side), and the current load API and cache shape have several bake-ins that would block adding new stat kinds.** Deferred to Phase 0c. Candidate future stat kinds worth considering during the ideal sketch: multi-column histograms / joint distributions for correlated predicates; column-to-column correlations beyond the existing `Histogram.Correlation` (column vs handle only); data-derived functional dependencies; alternative sketches (HyperLogLog for cardinality, KMV for distinct counts, t-digests for quantiles); sortedness / clustering signals; held sample rows; cross-table key-NDV alignment for join cardinality; explicit staleness signals beyond `LastUpdateVersion` / `LastAnalyzeVersion`; richer heavy-hitter structures beyond fixed-N TopN; cheap index-only stats independent of full ANALYZE. Bake-ins in the current design that would block extension: (1) load API `SendLoadRequests([]model.StatsLoadItem, timeout)` where `StatsLoadItem` is `(TableID, columnOrIndexID, isIndex, FullLoad)` -- anything that is not "one column or one index" does not fit; (2) cache key is physical ID with columns and indexes as nested maps; multi-column or cross-table stats have no obvious home; (3) storage schema (`mysql.stats_meta`, `mysql.stats_histograms`, `mysql.stats_buckets`, `mysql.stats_top_n`, `mysql.stats_fm_sketch`, `mysql.column_stats_usage`) requires a new system table per new stat kind plus migration; (4) singleflight key `"%d#%d#%t"` plus FullLoad bool will not dedupe new request shapes; (5) `StatsLoadedStatus` and `IsFullLoad` are binary per-column or per-index, no room for multi-component load state; (6) `ColAndIdxExistenceMap` only tracks columns and indexes. Phase 0c should: (a) catalog candidate future stat kinds plus their consumer fit (which planner stage would use each, at what latency tolerance, with what cache pressure); (b) identify which of the six bake-ins above are intentional contract clauses to keep vs accidental constraints to relax; (c) propose the minimal extension points the Phase 0b contract should specify so future stat kinds can be added without a full subsystem rewrite (e.g., a generalized `StatsLoadItem` type, an opaque stat-kind registry, per-kind storage table conventions, an extensible existence map). Produce a short `IDEAL-SKETCH.md` as the Phase 0c output. Do not implement anything; this is a design exercise to shape the Phase 0b contract.

These questions should be revisited as inputs to Phase 0b (current implementation contract) and Phase 0c (ideal sketch). They are not resolved here.
