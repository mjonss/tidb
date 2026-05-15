# pkg/planner/core stats call sites

## Summary

`pkg/planner/core` is the primary consumer of the stats subsystem. It (a) materializes per-DataSource snapshots of `*statistics.Table` via `stats.GetStatsTable` (which calls `StatsHandle.GetPhysicalTableStats`), (b) requests synchronous histogram loading via `CollectPredicateColumnsPoint` / `SyncWaitStatsLoadPoint` plus the `RequestLoadStats` / `SyncWaitStatsLoad` helpers, and (c) reads `StatisticTable`, `TableStats.HistColl`, and `TblColHists` from `logicalop.DataSource` (and copies of `TblColHists` into physical scans) throughout access-path generation, skyline pruning, cardinality estimation, cost computation, and TiFlash late-materialization. The package is read-only on stats: it never writes back into `*statistics.Table`; it mutates only the planner-side `property.StatsInfo` copy. A pseudo-stats branch exists everywhere a numeric estimate could fail; sync-load failure falls back to pseudo with a warning + plan-cache skip.

## Read sites

### pkg/planner/core/stats/stats.go (the entry wrapper into the stats handle)

- pkg/planner/core/stats/stats.go:64 `GetStatsTable` reads StatsHandle via `domain.GetDomain(ctx).StatsHandle()` [API]
  Stage: PlanBuilder.deriveStats4DataSource via `initStats` (also called by `LoadTableStats` for point-get plans)
  Tolerance: any (returns pseudo table if handle is nil)
  Fallback: returns `statistics.PseudoTable(tblInfo, false, true)`

- pkg/planner/core/stats/stats.go:74 / :76 `GetStatsTable` reads table snapshot via `statsHandle.GetPhysicalTableStats(tblInfo.ID|pid, tblInfo)` [API]
  Stage: same as above
  Tolerance: any (uses whatever the cache returns; dynamic-prune branch swaps physical id for the logical id)
  Fallback: see :105 below

- pkg/planner/core/stats/stats.go:78 asserts `statsTbl.ColAndIdxExistenceMap != nil`
  Stage: same; sanity check on the existence map invariant
  Tolerance: must be present
  Fallback: panic via intest.Assert

- pkg/planner/core/stats/stats.go:85 reads `statsTbl.GetAnalyzeRowCount()` (OptObjectiveDeterminate path)
  Stage: same; rewrites RealtimeCount/ModifyCount to the analyze-time count
  Tolerance: tolerant of pseudo; explicitly ignores realtime delta
  Fallback: sets RealtimeCount = max(GetAnalyzeRowCount, 0); may trigger `allowPseudoTblTriggerLoading`

- pkg/planner/core/stats/stats.go:87 reads `statsTbl.RealtimeCount` and `statsTbl.ModifyCount`
  Stage: same; decides whether to CopyAs and rewrite real-time stats
  Tolerance: any
  Fallback: copies the table and zeroes ModifyCount

- pkg/planner/core/stats/stats.go:94 reads `statsTbl.Pseudo` and `statsTbl.RealtimeCount`
  Stage: same; corner case for `allowPseudoTblTriggerLoading`
  Tolerance: any
  Fallback: sets the trigger flag

- pkg/planner/core/stats/stats.go:105 reads `statsTbl.RealtimeCount` to trigger pseudo when zero
  Stage: same
  Tolerance: any
  Fallback: returns `PseudoTable(tblInfo, allowPseudoTblTriggerLoading, true)` and increments `PseudoEstimationNotAvailable`

- pkg/planner/core/stats/stats.go:111 reads `statsTbl.IsInitialized()` (stats initialized flag)
  Stage: same
  Tolerance: any
  Fallback: marks the cloned table `Pseudo=true`, bumps `PseudoEstimationNotAvailable`

- pkg/planner/core/stats/stats.go:112 reads `statsTbl.IsOutdated()`
  Stage: same
  Tolerance: any; gated on `EnablePseudoForOutdatedStats` session var
  Fallback: marks Pseudo=true, bumps `PseudoEstimationOutdate`

- pkg/planner/core/stats/stats.go:149-151 reads `HistColl.RealtimeCount`, `HistColl.ModifyCount`, `Version` and `tableStats.Pseudo` in `LoadTableStats` (point-get warm-up path)
  Stage: PointGetPlan.LoadTableStats / BatchPointGetPlan.LoadTableStats (executor-time prefetch into stmt UsedStatsInfo)
  Tolerance: any
  Fallback: records `PseudoVersion` in UsedStatsInfo when `tableStats.Pseudo`

### pkg/planner/core/stats.go (DataSource stats derivation)

- pkg/planner/core/stats.go:159 `deriveStats4DataSource` reads `ds.TblColHists.RealtimeCount` to compute AccessPathMinSelectivity
  Stage: deriveStats4DataSource
  Tolerance: tolerant of pseudo (only the resulting selectivity may be unreliable)
  Fallback: if RealtimeCount==0, getGeneralAttributesFromPaths leaves minSelectivity at 1.0

- pkg/planner/core/stats.go:169, :214, :272, :344, :358 read `ds.StatisticTable.RealtimeCount` to seed `CountAfterAccess`
  Stage: fillIndexPath / adjustCountAfterAccess / deriveTablePathStats / deriveCommonHandleTablePathStats
  Tolerance: tolerant of pseudo
  Fallback: pseudo table still has a synthetic RealtimeCount used here

- pkg/planner/core/stats.go:190-191 reads/writes `ds.TableStats.HistColl.Idx2ColUniqueIDs[path.Index.ID]` (an index metadata field, but on HistColl)
  Stage: fillIndexPath
  Tolerance: any
  Fallback: no-op when length already covers the index

- pkg/planner/core/stats.go:196 passes `ds.TableStats.HistColl` into `detachCondAndBuildRangeForPath` -> cardinality.GetRowCountByIndexRanges (line 443)
  Stage: fillIndexPath
  Tolerance: tolerant of pseudo (cardinality has its own pseudo path)
  Fallback: pseudo stats produce a hard-coded estimate via cardinality module

- pkg/planner/core/stats.go:227, :372 read `ds.StatisticTable.Pseudo`
  Stage: deriveIndexPathStats / deriveCommonHandleTablePathStats
  Tolerance: tolerant of pseudo
  Fallback: switches branch to `cardinality.PseudoAvgCountPerValue`

- pkg/planner/core/stats.go:228, :373 call `cardinality.PseudoAvgCountPerValue(ds.StatisticTable)` (reads `RealtimeCount` inside cardinality)
  Stage: same
  Tolerance: by design pseudo
  Fallback: hard-coded pseudo average

- pkg/planner/core/stats.go:230, :233, :375, :378 read `ds.StatisticTable.RealtimeCount` and call `cardinality.EstimateColumnNDV(ds.StatisticTable, col.ID)` (NDV from column stats)
  Stage: deriveIndexPathStats / deriveCommonHandleTablePathStats
  Tolerance: tolerant of pseudo (EstimateColumnNDV has pseudo formula)
  Fallback: pseudo NDV formula

- pkg/planner/core/stats.go:250, :347, :443, :574 call `cardinality.Selectivity` / `GetRowCountByColumnRanges` / `GetRowCountByIndexRanges` against `ds.TableStats.HistColl` or `ds.StatisticTable.HistColl`
  Stage: deriveIndexPathStats / deriveTablePathStats / detachCondAndBuildRangeForPath / deriveStatsByFilter
  Tolerance: tolerant of pseudo
  Fallback: on error, callers log and fall back to `cost.SelectionFactor` (0.8)

- pkg/planner/core/stats.go:495 `getGroupNDVs` reads `ds.TableStats.HistColl.Idx2ColUniqueIDs` and iterates indexes via `tbl.ForEachIndexImmutable(...)`
  Stage: getGroupNDVs (called from initStats)
  Tolerance: any (uses what's present)
  Fallback: skips this NDV group if not present

- pkg/planner/core/stats.go:521-525 reads `idx.IsEssentialStatsLoaded()` and `idx.NDV`
  Stage: getGroupNDVs
  Tolerance: must be loaded before use (only used when `IsEssentialStatsLoaded`)
  Fallback: skips emitting the group NDV

- pkg/planner/core/stats.go:537 calls `stats.GetStatsTable(...)` (see entry wrapper above)
  Stage: initStats
  Tolerance: see GetStatsTable
  Fallback: pseudo table

- pkg/planner/core/stats.go:540, :542, :543 read `ds.StatisticTable.RealtimeCount`, call `GenerateHistCollFromColumnInfo`, read `ds.StatisticTable.Version`
  Stage: initStats (builds the per-DataSource StatsInfo)
  Tolerance: any
  Fallback: pseudo path sets `tableStats.StatsVersion = statistics.PseudoVersion` at :545-:547

- pkg/planner/core/stats.go:555-557 reads `HistColl.RealtimeCount`, `HistColl.ModifyCount`, `ds.StatisticTable.ColAndIdxExistenceMap` for `stmtctx.UsedStatsInfoForTable`
  Stage: initStats (telemetry/used-stats record)
  Tolerance: any
  Fallback: stored as-is for slow-log and explain

- pkg/planner/core/stats.go:561 calls `cardinality.EstimateColumnNDV(ds.StatisticTable, col.ID)` for each schema column
  Stage: initStats (per-column ColNDVs)
  Tolerance: tolerant of pseudo
  Fallback: cardinality pseudo formula

- pkg/planner/core/stats.go:565 calls `ds.StatisticTable.ID2UniqueID(ds.TblCols)` into `ds.TblColHists`
  Stage: initStats (builds the planner-local TblColHists copy)
  Tolerance: any
  Fallback: structural rebuild; no values

### pkg/planner/core/rule/rule_collect_plan_stats.go (sync-load orchestration)

- pkg/planner/core/rule/rule_collect_plan_stats.go:115 reads `domain.GetDomain(sctx).StatsHandle()` (handle existence check) [API]
  Stage: CollectPredicateColumnsPoint.markAtLeastOneFullStatsLoadForEachTable
  Tolerance: any
  Fallback: returns early if handle is nil

- pkg/planner/core/rule/rule_collect_plan_stats.go:130 reads table via `statsHandle.GetPhysicalTableStats(tblInfo.ID, tblInfo)` [API]
  Stage: same
  Tolerance: any
  Fallback: skips iteration if nil or `tableStats.Pseudo`

- pkg/planner/core/rule/rule_collect_plan_stats.go:131 reads `tableStats.Pseudo`
  Stage: same
  Tolerance: tolerant of pseudo
  Fallback: skip table for "at least one full load"

- pkg/planner/core/rule/rule_collect_plan_stats.go:134 reads `tableStats.ColAndIdxExistenceMap.HasAnalyzed(...)` (ColAndIdxExistenceMap entry)
  Stage: same
  Tolerance: any
  Fallback: skips column if not analyzed

- pkg/planner/core/rule/rule_collect_plan_stats.go:152 reads `statsHandle.GetPhysicalTableStats(tbl.ID, tbl)` second time per table [API]
  Stage: same (the per-table "trigger at least one column" loop)
  Tolerance: any
  Fallback: bails if pseudo

- pkg/planner/core/rule/rule_collect_plan_stats.go:162 reads `tblStats.ColAndIdxExistenceMap.HasAnalyzed(col.ID, false)`
  Stage: same
  Tolerance: any
  Fallback: skips column

- pkg/planner/core/rule/rule_collect_plan_stats.go:165 reads `tblStats.GetCol(col.ID)`, then :167 `colStats.IsFullLoad()`
  Stage: same
  Tolerance: any; just inspects in-memory state
  Fallback: marks `colToTriggerLoad = nil` if any column is already full-loaded

- pkg/planner/core/rule/rule_collect_plan_stats.go:184 reads `tblStats.GetIdx(idx.ID)` and `idxStats.IsFullLoad()`
  Stage: same
  Tolerance: any
  Fallback: marks `colToTriggerLoad = nil`

- pkg/planner/core/rule/rule_collect_plan_stats.go:350 `domain.GetDomain(ctx).StatsHandle().SendLoadRequests(...)` [sync-load API]
  Stage: RequestLoadStats (called from CollectPredicateColumnsPoint.Optimize)
  Tolerance: must be loaded before use (blocking timeout from `StatsLoadSyncWait`, optionally capped by `max_execution_time`)
  Fallback: if `StatsLoadPseudoTimeout` then sets `IsSyncStatsFailed`, skips plan cache, returns nil (pseudo); else returns error

- pkg/planner/core/rule/rule_collect_plan_stats.go:375 `domain.GetDomain(plan.SCtx()).StatsHandle().SyncWaitStatsLoad(stmtCtx)` [sync-load API]
  Stage: SyncWaitStatsLoad (called by SyncWaitStatsLoadPoint, the second logical-rule pass)
  Tolerance: must be loaded before use
  Fallback: identical to RequestLoadStats fallback

- pkg/planner/core/rule/rule_collect_plan_stats.go:481 reads `domain.GetDomain(ctx).StatsHandle()` then :509 `stats.GetPhysicalTableStats(tbl.ID, tbl)` [API]
  Stage: collectSyncIndices (decides which index stats to load)
  Tolerance: any
  Fallback: skips index if pseudo

- pkg/planner/core/rule/rule_collect_plan_stats.go:513 `tblStats.IndexIsLoadNeeded(idxID)` (load-needed predicate)
  Stage: collectSyncIndices
  Tolerance: any
  Fallback: if not needed, skip

- pkg/planner/core/rule/rule_collect_plan_stats.go:553-560 reads `statsHandle.GetPhysicalTableStats(tableInfo.ID, tableInfo)` for runtime stats reporting [API]
  Stage: recordSingleTableRuntimeStats (post-execution telemetry)
  Tolerance: any
  Fallback: returns empty stats with `skip=true` for temp tables

### pkg/planner/core/find_best_task.go (physical planning + skyline)

- pkg/planner/core/find_best_task.go:865 reads `statsTbl.HistColl.Pseudo` (table pseudo flag)
  Stage: compareCandidates (skyline pruning per path-pair)
  Tolerance: tolerant of pseudo
  Fallback: drives `comparePseudo` branch / index-without-stats heuristics

- pkg/planner/core/find_best_task.go:960 reads `statsTbl.HistColl.Pseudo` (twice)
  Stage: isCandidatesPseudo
  Tolerance: tolerant of pseudo
  Fallback: leaves both lhs/rhs as pseudo

- pkg/planner/core/find_best_task.go:963, :970 `statsTbl.ColAndIdxExistenceMap.HasAnalyzed(idx.ID, true)`
  Stage: isCandidatesPseudo
  Tolerance: any
  Fallback: marks the side without stats as pseudo

- pkg/planner/core/find_best_task.go:1696 calls compareCandidates with `ds.StatisticTable` (which reads HistColl.Pseudo + ColAndIdxExistenceMap as above)
  Stage: skylinePruning loop
  Tolerance: tolerant of pseudo
  Fallback: idxMissingStats may flip `preferRange`

- pkg/planner/core/find_best_task.go:1719 reads `ds.TableStats.HistColl.Pseudo` and `ds.TableStats.RowCount`
  Stage: skylinePruning post-loop (set `preferRange`)
  Tolerance: tolerant of pseudo
  Fallback: enables prefer-range-scan when row count < 1 or pseudo

- pkg/planner/core/find_best_task.go:2214, :2448, :2467, :2786, :2760 propagate `ds.TblColHists` into CopTask / PhysicalIndexScan / MppTask
  Stage: convertToIndexMergeScan / convertToIndexScan / convertToTableScan / convertToPartialTableScan / mpp path
  Tolerance: any (snapshot copy of HistColl into physical plan)
  Fallback: pseudo HistColl flows through

- pkg/planner/core/find_best_task.go:2245-2246 reads `ds.StatsInfo().RowCount`
  Stage: convertToIndexMergeScan
  Tolerance: any
  Fallback: tolerance factor + scale

- pkg/planner/core/find_best_task.go:2356 `cardinality.Selectivity(..., ds.TableStats.HistColl, ...)`
  Stage: convertToPartialTableScan
  Tolerance: tolerant of pseudo
  Fallback: SelectionFactor

- pkg/planner/core/find_best_task.go:2474 reads `ds.TableStats.StatsVersion`
  Stage: convertToIndexScan
  Tolerance: any
  Fallback: PseudoVersion already baked into ds.TableStats

- pkg/planner/core/find_best_task.go:2588-2589 reads `is.StatsInfo().RowCount` and `p.TableStats.ScaleByExpectCnt(...)`
  Stage: addPushedDownSelection4PhysicalIndexScan
  Tolerance: any
  Fallback: indexSel built with the scaled stats

- pkg/planner/core/find_best_task.go:2598 `cardinality.Selectivity(..., copTask.TblColHists, tableConds, nil)`
  Stage: addPushedDownSelection4PhysicalIndexScan (root-task conds)
  Tolerance: tolerant of pseudo
  Fallback: SelectionFactor

- pkg/planner/core/find_best_task.go:2868, :2970, :2996, :3005 use `ds.TableStats.ScaleByExpectCnt(...)` for point-get / batch-point-get plans
  Stage: convertToPointGet / convertToBatchPointGet
  Tolerance: any
  Fallback: scales pseudo stats safely

- pkg/planner/core/find_best_task.go:3049 `cardinality.Selectivity(..., copTask.TblColHists, sel.Conditions, nil)`
  Stage: helper for cop-table selection
  Tolerance: tolerant of pseudo
  Fallback: SelectionFactor

### pkg/planner/core/exhaust_physical_plans.go (physical plan exhaustion + cost helpers)

- pkg/planner/core/exhaust_physical_plans.go:530-540 reads `stats.HistColl.RealtimeCount`, `stats.HistColl == nil`, `stats.HistColl.Pseudo`
  Stage: getProbeFullScanRowsForIndexJoinPrune / hasPseudoStatsForIndexJoinPrune (index-join scan-ratio prune)
  Tolerance: tolerant of pseudo
  Fallback: returns 0 (disables prune) when pseudo or unknown

- pkg/planner/core/exhaust_physical_plans.go:864 `cardinality.Selectivity(..., ds.TableStats.HistColl, ...)` for index-join inner table-scan
  Stage: constructDS2TableScanTask (constructInnerByZippedChildren)
  Tolerance: tolerant of pseudo
  Fallback: SelectionFactor

- pkg/planner/core/exhaust_physical_plans.go:882 reads `ds.StatsInfo().StatsVersion`
  Stage: same
  Tolerance: any
  Fallback: pseudo version already

- pkg/planner/core/exhaust_physical_plans.go:853, :892, :1053, :1061, :1075 propagate `ds.TblColHists` into physical plans
  Stage: constructDS2TableScanTask / constructDS2IndexScanTask
  Tolerance: any (snapshot copy)
  Fallback: pseudo copy still flows

- pkg/planner/core/exhaust_physical_plans.go:971-1016 `getColsNDVLowerBoundFromHistColl` reads `histColl.ColNum()`, `histColl.GetCol(uid)`, `colStats.IsStatsInitialized()`, `colStats.NDV`, `histColl.Idx2ColUniqueIDs`, `histColl.GetIdx(idxID)`, `idxStats.IsStatsInitialized()`, `idxStats.NDV`
  Stage: constructDS2IndexScanTask (Fix44855 index-join NDV bounding)
  Tolerance: must be loaded before use (skips column/index when not initialized)
  Fallback: returns -1 -> caller leaves rowCountUpperBound unchanged

- pkg/planner/core/exhaust_physical_plans.go:1092 reads `ds.TableStats.StatsVersion`
  Stage: constructDS2IndexScanTask
  Tolerance: any
  Fallback: pseudo version

- pkg/planner/core/exhaust_physical_plans.go:1126, :1138, :1140 reads `ds.TableStats != nil`, calls `getColsNDVLowerBoundFromHistColl(usedColIDs, ds.TableStats.HistColl)`, reads `ds.TableStats.RowCount`
  Stage: constructDS2IndexScanTask (NDV upper-bound)
  Tolerance: must be loaded before use (otherwise returns -1)
  Fallback: skips the rowCountUpperBound clamp

- pkg/planner/core/exhaust_physical_plans.go:1163, :1181, :1195, :1200 `cardinality.Selectivity(..., ds.TableStats.HistColl, ...)` and `ds.TableStats.ScaleByExpectCnt(...)`
  Stage: constructDS2IndexScanTask (table and index residual filters)
  Tolerance: tolerant of pseudo
  Fallback: SelectionFactor

- pkg/planner/core/exhaust_physical_plans.go:1720-1726 reads `stats.HistColl == nil`, then `stats.Count()` and `cardinality.GetAvgRowSize(..., stats.HistColl, ...)`
  Stage: checkChildFitBC (MPP broadcast-join decision)
  Tolerance: tolerant of pseudo (falls back to row-count threshold when HistColl is nil)
  Fallback: uses session var thresholds

- pkg/planner/core/exhaust_physical_plans.go:1729-1738 / :1756-1764 same pattern in calcBroadcastExchangeSize / calcHashExchangeSize
  Stage: MPP exchange-size estimate
  Tolerance: tolerant of pseudo
  Fallback: hasSize=false -> row-count comparison only

- pkg/planner/core/exhaust_physical_plans.go:2280 `cardinality.EstimateColsNDVWithMatchedLen(la.SCtx(), columns, la.Schema(), la.StatsInfo())`
  Stage: physical aggregate construction
  Tolerance: tolerant of pseudo (cardinality has fallback)
  Fallback: pseudo NDV

### pkg/planner/core/index_join_path.go

- pkg/planner/core/index_join_path.go:107 (struct field) `innerTableStats *property.StatsInfo`
  Stage: indexJoinPathInfo carries the snapshot of `innerDS.TableStats` into index-join path building

- pkg/planner/core/index_join_path.go:307 calls `compareCandidates(..., ds.StatisticTable, ...)` (which reads HistColl.Pseudo + ColAndIdxExistenceMap, see find_best_task.go entries)
  Stage: indexJoinPathCompare (reuse of skyline rules)
  Tolerance: tolerant of pseudo
  Fallback: falls through to indexJoinPathCmp4UnComparableOnes

- pkg/planner/core/index_join_path.go:411 reads `stats.StatsVersion != statistics.PseudoVersion`
  Stage: indexJoinPathConstructResult
  Tolerance: tolerant of pseudo (skips NDV use when pseudo, see #63869)
  Fallback: leaves `innerNDV` at 0 -> join estimator falls back

- pkg/planner/core/index_join_path.go:420 calls `cardinality.EstimateColsNDVWithMatchedLen(..., stats)`
  Stage: same
  Tolerance: tolerant of pseudo (gated by previous version check)
  Fallback: cardinality module returns conservative NDV

- pkg/planner/core/index_join_path.go:805 sets `innerTableStats: innerDS.TableStats`
  Stage: buildIndexJoinPathInfo (snapshot capture)

### pkg/planner/core/indexmerge_path.go and indexmerge_unfinished_path.go

- pkg/planner/core/indexmerge_path.go:103 reads `ds.StatsInfo().RowCount` and `ds.TableStats.ScaleByExpectCnt(...)`
  Stage: generateIndexMergeOrPaths (re-adjusting stats after index-merge path build)
  Tolerance: any
  Fallback: scales pseudo safely

- pkg/planner/core/indexmerge_path.go:393, :403 `cardinality.Selectivity(..., ds.TableStats.HistColl, partialFilters, nil)` and `sel * ds.TableStats.RowCount`
  Stage: generateIndexMergeIntersectionPath
  Tolerance: tolerant of pseudo
  Fallback: SelectionFactor

- pkg/planner/core/indexmerge_path.go:466, :490, :713, :759, :789, :806-807 propagate `ds.TableStats.HistColl` and read `histColl.RealtimeCount` through `buildPartialPathUp4MVIndex` / `buildPartialPaths4MVIndexWithPath` / `cardinality.CalcTotalSelectivityForMVIdxPath`
  Stage: MV-index merge planning
  Tolerance: tolerant of pseudo
  Fallback: pseudo NDV / selectivity from cardinality module

- pkg/planner/core/indexmerge_unfinished_path.go:384 `buildPartialPaths4MVIndexWithPath(..., ds.TableStats.HistColl)`
  Stage: buildIntoAccessPathForOrList (unfinished MV path build)
  Tolerance: tolerant of pseudo
  Fallback: cardinality fallback

- pkg/planner/core/indexmerge_unfinished_path.go:549, :555-557 `cardinality.CalcTotalSelectivityForMVIdxPath(ds.TableStats.HistColl, ...)` and `cardinality.Selectivity(..., ds.TableStats.HistColl, ...)`
  Stage: estimateCountAfterAccessForIndexMergeOR
  Tolerance: tolerant of pseudo
  Fallback: SelectionFactor

- pkg/planner/core/indexmerge_unfinished_path.go:566 reads `ds.TableStats.RowCount`
  Stage: same
  Tolerance: any
  Fallback: multiplies whatever value is present

### pkg/planner/core/optimizer.go (post-plan checks)

- pkg/planner/core/optimizer.go:1305-1306, :1310, :1315 reads `statsInfo.HistColl.Pseudo`, `HistColl.RealtimeCount`, `HistColl.ColNum()`, `HistColl.GetCol(column.UniqueID)`, `colStats.IsHandle`, `colStats.TotColSize`, `colStats.NullCount`
  Stage: hasUsableOverlongTypeSizeStats (reusable-chunk row-size estimation, called during optimizer.go physical optimization post-check)
  Tolerance: tolerant of pseudo (false means schema-worst-case fallback)
  Fallback: returns false -> schema-based EstimateTypeWidth path

- pkg/planner/core/optimizer.go:1336-1337, :1343-1344 reads `statsInfo.HistColl == nil || HistColl.Pseudo || HistColl.RealtimeCount == 0 || HistColl.ColNum() == 0`
  Stage: estimateReusableChunkRowsForOverlongType
  Tolerance: tolerant of pseudo
  Fallback: returns (0, false) for non-point readers

### pkg/planner/core/plan_cost_ver2.go (cost ver2)

- pkg/planner/core/plan_cost_ver2.go:1202-1212, :1237 reads `p.TblColHists.GetAnalyzeRowCount()`, `tblColHists.Pseudo`, `tblColHists.ModifyCount`
  Stage: getTableScanPenalty (table-scan cost penalty for high-risk plans)
  Tolerance: tolerant of pseudo; explicitly handles pseudo + high-ModifyCount
  Fallback: penalty is 0 when no risk indicator triggers

- pkg/planner/core/plan_cost_ver2.go:373-374 `cardinality.GetAvgRowSize(..., physicalop.GetTblStats(p.IndexPlan|p.TablePlan), ...)`
  Stage: IndexLookUpReader cost ver2
  Tolerance: tolerant of pseudo
  Fallback: cardinality has a pseudo fallback

### pkg/planner/core/plan_cost_ver1.go (cost ver1)

- pkg/planner/core/plan_cost_ver1.go:188, :195, :229, :265, :289, :332, :351, :1072, :1075, :1109, :1111 `cardinality.GetAvgRowSize` / `GetTableAvgRowSize` / `GetIndexAvgRowSize` against `GetTblStats(...)` or `p.StatsInfo().HistColl`
  Stage: physical-plan cost computation (TableReader, IndexReader, IndexLookUp, IndexMerge, PointGet, BatchPointGet)
  Tolerance: tolerant of pseudo (cardinality handles nil/pseudo HistColl)
  Fallback: cardinality returns a default per-column width

- pkg/planner/core/plan_cost_ver1.go:761, :788, :838 `cardinality.EstimateFullJoinRowCount(...)`, `cardinality.EstimateColsNDVWithMatchedLen(p.SCtx(), innerKeys, innerSchema, innerStats)`
  Stage: hash/index/merge-join cost computation
  Tolerance: tolerant of pseudo
  Fallback: cardinality returns conservative bound

### pkg/planner/core/planbuilder.go

- pkg/planner/core/planbuilder.go:1803, :1816, :1819, :1834, :1875 builds and propagates `pseudoHistColl := statistics.PseudoHistColl(physicalID, false)` into `PhysicalIndexScan`/`PhysicalTableScan`/`CopTask`
  Stage: PlanBuilder.buildPhysicalIndexLookUpReader (no DataSource exists -> synthesize pseudo)
  Tolerance: by-design pseudo
  Fallback: this is the fallback itself

- pkg/planner/core/planbuilder.go:3005 `statsHandle.GetPhysicalTableStats(physicalID, tblInfo)` then `statistics.AnalyzeVersionMatchesForTableStats(...)` [API]
  Stage: PlanBuilder.analyzeVersionMatchesForPhysicalIDs (build ANALYZE plan; checks if existing stats use the requested version)
  Tolerance: version-checked
  Fallback: appends warning that existing stats version will be overwritten

### pkg/planner/core/logical_plan_builder.go

- pkg/planner/core/logical_plan_builder.go:4696 reads `domain.GetDomain(ctx).StatsHandle()` [API]
  Stage: getLatestVersionFromStatsTable (used by plan-cache version matching)
  Tolerance: any
  Fallback: returns 0 if handle is nil

- pkg/planner/core/logical_plan_builder.go:4704, :4706 `statsHandle.GetPhysicalTableStats(...)` [API]
  Stage: same
  Tolerance: any
  Fallback: GetPhysicalTableStats always returns at minimum a pseudo Table

- pkg/planner/core/logical_plan_builder.go:4710 reads `statsTbl.RealtimeCount`
  Stage: same
  Tolerance: any
  Fallback: returns 0 (treats as pseudo, plan-cache mismatch)

- pkg/planner/core/logical_plan_builder.go:4712 reads `statsTbl.GetAnalyzeRowCount()` (OptObjectiveDeterminate)
  Stage: same
  Tolerance: tolerant of pseudo
  Fallback: max(0, count)

- pkg/planner/core/logical_plan_builder.go:4720-:4727 iterates `statsTbl.ForEachColumnImmutable(...)` and `statsTbl.ForEachIndexImmutable(...)` reading `col.LastUpdateVersion`, `idx.LastUpdateVersion`
  Stage: same (compute max LastUpdateVersion for plan-cache validation)
  Tolerance: monotonic (relies on LastUpdateVersion not going backwards)
  Fallback: comment notes that `statsTbl.LastAnalyzeVersion` would replace this; for now manual scan

- pkg/planner/core/logical_plan_builder.go:4991 `h.GetPhysicalTableStats(tableInfo.ID, tableInfo)` [API]
  Stage: PlanBuilder.buildDataSource (decides if dynamic partition prune mode can be used)
  Tolerance: any
  Fallback: see :4993

- pkg/planner/core/logical_plan_builder.go:4993 reads `tblStats.IsAnalyzed()` (analyzed flag)
  Stage: same
  Tolerance: any
  Fallback: when not analyzed and `tidb_skip_missing_partition_stats=off`, force static partition processor on; otherwise proceeds with dynamic prune

### pkg/planner/core/operator/logicalop/logical_mem_table.go

- pkg/planner/core/operator/logicalop/logical_mem_table.go:189-197 `statistics.PseudoTable(p.TableInfo, false, false)` then reads `statsTable.RealtimeCount` and calls `statsTable.GenerateHistCollFromColumnInfo(...)`
  Stage: LogicalMemTable.DeriveStats (memtables never have real stats)
  Tolerance: by-design pseudo
  Fallback: this is the fallback itself (pseudo with `StatsVersion = PseudoVersion`)

### pkg/planner/core/operator/physicalop/physical_table_scan.go

- pkg/planner/core/operator/physicalop/physical_table_scan.go:131 (struct field) `TblColHists *statistics.HistColl`
- pkg/planner/core/operator/physicalop/physical_table_scan.go:171, :192, :784 propagates `ds.TblColHists` into PhysicalTableScan
- pkg/planner/core/operator/physicalop/physical_table_scan.go:203-205 calls `cardinality.AdjustRowCountForTableScanByLimit(..., ds.StatsInfo(), ds.TableStats, ds.StatisticTable, ...)`
  Stage: GetOriginalPhysicalTableScan (heuristic when limit pushes through table scan)
  Tolerance: tolerant of pseudo
  Fallback: cardinality has pseudo branch

- pkg/planner/core/operator/physicalop/physical_table_scan.go:212, :792 `ds.TableStats.ScaleByExpectCnt(...)`
  Stage: GetOriginalPhysicalTableScan / BuildIndexMergeTableScan
  Tolerance: any
  Fallback: pseudo stats scale safely

- pkg/planner/core/operator/physicalop/physical_table_scan.go:494 reads `p.StatsInfo().StatsVersion == statistics.PseudoVersion`
  Stage: PhysicalTableScan.ExplainInfo (explain output)
  Tolerance: tolerant of pseudo
  Fallback: appends "stats:pseudo" string

- pkg/planner/core/operator/physicalop/physical_table_scan.go:648, :652 `cardinality.GetTableAvgRowSize(..., p.TblColHists, ...)`
  Stage: PhysicalTableScan.GetScanRowSize / TableRowSize
  Tolerance: tolerant of pseudo
  Fallback: cardinality default

- pkg/planner/core/operator/physicalop/physical_table_scan.go:797 reads `ds.StatisticTable.Pseudo`
  Stage: BuildIndexMergeTableScan (sets StatsVersion=PseudoVersion on the scan)
  Tolerance: tolerant of pseudo
  Fallback: explicit assignment

- pkg/planner/core/operator/physicalop/physical_table_scan.go:807 `cardinality.Selectivity(..., ds.TableStats.HistColl, pushedFilters, nil)`
  Stage: BuildIndexMergeTableScan
  Tolerance: tolerant of pseudo
  Fallback: SelectionFactor

### pkg/planner/core/operator/physicalop/physical_index_scan.go

- pkg/planner/core/operator/physicalop/physical_index_scan.go:94 (struct field) `TblColHists *statistics.HistColl`
- pkg/planner/core/operator/physicalop/physical_index_scan.go:308 reads `p.StatsInfo().StatsVersion == statistics.PseudoVersion`
  Stage: PhysicalIndexScan.ExplainInfo
  Tolerance: tolerant of pseudo
  Fallback: emits "stats:pseudo"

- pkg/planner/core/operator/physicalop/physical_index_scan.go:352 `cardinality.GetIndexAvgRowSize(..., p.TblColHists, ...)`
  Stage: GetScanRowSize
  Tolerance: tolerant of pseudo
  Fallback: cardinality default

- pkg/planner/core/operator/physicalop/physical_index_scan.go:636, :660 propagates `ds.TblColHists` into PhysicalIndexScan
- pkg/planner/core/operator/physicalop/physical_index_scan.go:680-682 `cardinality.AdjustRowCountForIndexScanByLimit(..., ds.StatsInfo(), ds.TableStats, ds.StatisticTable, ...)`
  Stage: GetOriginalPhysicalIndexScan
  Tolerance: tolerant of pseudo
  Fallback: cardinality fallback

- pkg/planner/core/operator/physicalop/physical_index_scan.go:688-691 `ds.TableStats.RowCount` and `ds.TableStats.Scale/ScaleByExpectCnt(...)`
  Stage: GetOriginalPhysicalIndexScan (special-case MV index overflow)
  Tolerance: any
  Fallback: scales pseudo safely

- pkg/planner/core/operator/physicalop/physical_index_scan.go:740-742 reads `ds.StatisticTable.Version` and `ds.StatisticTable.Pseudo`
  Stage: ConvertToPartialIndexScan (sets `stats.StatsVersion` to PseudoVersion when pseudo)
  Tolerance: tolerant of pseudo
  Fallback: explicit assignment

### pkg/planner/core/operator/physicalop/physical_batch_point_get.go

- pkg/planner/core/operator/physicalop/physical_batch_point_get.go:335-346, :758-762 `stats.LoadTableStats(ctx, p.TblInfo, ...)` (delegates to pkg/planner/core/stats/stats.go LoadTableStats; warms up UsedStatsInfo for executor at exec start)
  Stage: PointGetPlan.LoadTableStats / BatchPointGetPlan.LoadTableStats (called from executor)
  Tolerance: any
  Fallback: pseudo path inside LoadTableStats

- pkg/planner/core/operator/physicalop/physical_batch_point_get.go:449, :451, :1000, :1002 `cardinality.GetTableAvgRowSize|GetIndexAvgRowSize(..., p.StatsInfo().HistColl, ...)`
  Stage: GetCost methods
  Tolerance: tolerant of pseudo
  Fallback: cardinality default

### pkg/planner/core/operator/physicalop/physical_cte.go

- pkg/planner/core/operator/physicalop/physical_cte.go:275 propagates `p.StatsInfo().HistColl` into MppTask
  Stage: PhysicalCTE.attach2Task
  Tolerance: any
  Fallback: HistColl may be nil for derived plans; consumers gate on `s.HistColl == nil`

### pkg/planner/core/operator/physicalop/physical_utils.go

- pkg/planner/core/operator/physicalop/physical_utils.go:124-132 `GetTblStats(copTaskPlan)` returns `x.TblColHists` for PhysicalTableScan / PhysicalIndexScan
  Stage: helper used by cost ver1 / cost ver2 / readers
  Tolerance: any
  Fallback: returns whatever was set (pseudo or real)

### pkg/planner/core/operator/physicalop/task.go and task_base.go

- pkg/planner/core/operator/physicalop/task.go:49 `cardinality.Selectivity(ctx, t.TblColHists, t.RootTaskConds, nil)`
  Stage: CopTask.handleRootTaskConds (conversion to root task with residual filters)
  Tolerance: tolerant of pseudo
  Fallback: SelectionFactor

- pkg/planner/core/operator/physicalop/task_base.go:211, :245, :385-387 (struct fields) `tblColHists`/`TblColHists *statistics.HistColl` and getter `GetTblColHists()`
  Stage: MppTask / CopTask carry the snapshot reference

- pkg/planner/core/operator/physicalop/task_base.go:344 `cardinality.Selectivity(ctx, t.tblColHists, t.RootTaskConds, nil)` (sibling for the MppTask form)
  Stage: MppTask helper
  Tolerance: tolerant of pseudo
  Fallback: SelectionFactor

### pkg/planner/core/operator/physicalop/tiflash_predicate_push_down.go

- :120 `cardinality.Selectivity(sctx, ts.TblColHists, group, nil)` -- TiFlash group selectivity sort
- :216 `cardinality.Selectivity(sctx, ts.TblColHists, mergedConds, nil)` -- predicatePushDownToTableScan greedy
- :266 reads `ts.TblColHists.RealtimeCount` -- gate to skip TiFlash late-materialization for small tables
- :348 `cardinality.Selectivity(pctx, ts.TblColHists, []expression.Expression{cond}, nil)` -- inverted-index selectivity gate
- :391 `cardinality.Selectivity(ts.SCtx(), ts.TblColHists, selectedConditions, nil)` -- final row-count update after pushdown
  Stage: handleTiFlashPredicatePushDown / predicatePushDownToTableScan
  Tolerance: tolerant of pseudo (Selectivity has pseudo branch; the `RealtimeCount` gate explicitly bypasses late-materialization for very small tables)
  Fallback: bypass push-down / use selectivity threshold defaults

### pkg/planner/core/operator/physicalop/physical_table_reader.go, physical_index_reader.go, physical_indexlookup_reader.go, physical_indexmerge_reader.go

- physical_table_reader.go:141, :163 `cardinality.GetAvgRowSize(..., GetTblStats(p.TablePlan), ...)`
- physical_index_reader.go:169 `cardinality.GetAvgRowSize(..., tblStats, ...)`
- physical_indexlookup_reader.go:124, :129 `cardinality.GetAvgRowSize(..., GetTblStats(p.IndexPlan|TablePlan), ...)`
- physical_indexmerge_reader.go:115, :121 `cardinality.GetAvgRowSize(..., GetTblStats(...), ...)`
  Stage: reader-side row-size estimation for shuffle/exchange & cost ver2
  Tolerance: tolerant of pseudo
  Fallback: cardinality default

### pkg/planner/core/operator/logicalop/{logical_aggregation,logical_apply,logical_join,logical_projection,logical_cte}.go (derived NDV reads)

- logical_aggregation.go:235 `cardinality.EstimateColsNDVWithMatchedLen(la.SCtx(), gbyCols, childSchema[0], childProfile)`
- logical_apply.go:183 / :207 `cardinality.EstimateFullJoinRowCount(...)` / `EstimateColsNDVWithMatchedLen(...)`
- logical_join.go:572 `cardinality.EstimateFullJoinRowCount(p.SCtx(), ...)`
- logical_projection.go:296 `cardinality.EstimateColsNDVWithMatchedLen(...)` for each projection col
- logical_cte.go:235 `cardinality.EstimateColsNDVWithMatchedLen(...)` for CTE row count
  Stage: per-operator DeriveStats (logical-plan stats derivation)
  Tolerance: tolerant of pseudo (these consume `property.StatsInfo` which already carries the pseudo flag)
  Fallback: cardinality returns conservative bounds

### pkg/planner/core/task.go (root)

- pkg/planner/core/task.go:198-199 reads `stats.HistColl != nil` then `cardinality.GetAvgRowSizeDataInDiskByRows(stats.HistColl, cols)`
  Stage: cop-task to root-task conversion (size estimation for disk spill)
  Tolerance: tolerant of pseudo
  Fallback: size=0 (no spill estimate)

- pkg/planner/core/task.go:778, :815 reads `originStats.StatsVersion` and writes to `ts.StatsInfo().StatsVersion`
  Stage: cop-task `finishIndexPlan` / similar (propagates StatsVersion from existing stats)
  Tolerance: any
  Fallback: pseudo carried through

- pkg/planner/core/task.go:1555-1567 `stats.Scale(...)` -- internal StatsInfo scaling
  Stage: misc cop/mpp conversion
  Tolerance: any
  Fallback: scales pseudo safely

- pkg/planner/core/task.go:1706, :1734 `cardinality.EstimateColsNDVWithMatchedLen(...)` / `EstimateColsDNVWithMatchedLenFromUniqueIDs(...)`
  Stage: PartialAgg / FinalAgg derivation
  Tolerance: tolerant of pseudo
  Fallback: cardinality fallback

- pkg/planner/core/task.go:2180, :2211 reads `mpp.GetTblColHists()` and `lastTask.GetTblColHists()` propagation
  Stage: MppTask plumbing
  Tolerance: any
  Fallback: pseudo HistColl propagates

- pkg/planner/core/task.go:2231-2239 reads `mppPlan.StatsInfo().HistColl != nil` then `cardinality.GetAvgRowSize(..., mppPlan.StatsInfo().HistColl, schemaCols, ...)`
  Stage: MPP plan size estimate
  Tolerance: tolerant of pseudo
  Fallback: returns 0 when HistColl nil

### pkg/planner/core/plan.go

- pkg/planner/core/plan.go:81, :122 `cardinality.EstimateColsNDVWithMatchedLen(ctx, partitionBy, dataSource.Schema(), dataSource.StatsInfo())`
  Stage: optimizeByShuffle4Window / optimizeByShuffle4StreamAgg (concurrency capping)
  Tolerance: tolerant of pseudo
  Fallback: caps `concurrency` at 1 if ndv <= 1 -> no shuffle

## Out of pattern

- pkg/planner/core/planbuilder.go:1803-:1875 synthesizes a `statistics.PseudoHistColl` directly (no cache lookup) because `buildPhysicalIndexLookUpReader` builds a physical plan for `ADMIN CHECK INDEX` / `INDEX LOOKUP` outside the normal DataSource path. This is a deliberate bypass that hands the executor pseudo HistColl regardless of what's in the cache.

- pkg/planner/core/operator/logicalop/logical_mem_table.go:189 synthesizes `statistics.PseudoTable(...)` for memtables. This is a definitional bypass since system memtables never have real stats.

- pkg/planner/core/logical_plan_builder.go:4690-4729 `getLatestVersionFromStatsTable` performs a parallel implementation of `stats.GetStatsTable` for plan-cache version matching. It deliberately does NOT increment the pseudo metrics or copy the table; it relies on `ForEachColumnImmutable` / `ForEachIndexImmutable` over `LastUpdateVersion` instead of using `statsTbl.LastAnalyzeVersion` (commented-out alternative on :4719). This is duplicated logic and a candidate for unification.

- pkg/planner/core/rule/rule_collect_plan_stats.go:130 and :152 call `statsHandle.GetPhysicalTableStats(tbl.ID, tbl)` twice per visited table (once for the "any column triggers loading" gate, once for the iteration). These are unguarded against cache miss / pseudo flips between calls. The two snapshots could in principle differ.

- pkg/planner/core/find_best_task.go:865 reads `statsTbl.HistColl.Pseudo` but `compareCandidates`'s `lhsPseudo/rhsPseudo` reasoning (lines 854-906) further refines this using `ColAndIdxExistenceMap.HasAnalyzed(idxID, true)` -- so the same field is consulted twice through different lenses (table-level pseudo flag vs. per-index analyzed flag). This is a deliberate fall-through in the skyline-prune heuristic for plans with partial stats.

- pkg/planner/core/index_join_path.go:411 checks `stats.StatsVersion != statistics.PseudoVersion` rather than the table's `Pseudo` flag, because the planner sets `PseudoVersion` on `property.StatsInfo` at init time (stats.go:546) -- this duplicates the pseudo signal in two fields and code in different stages picks different ones.

- pkg/planner/core/stats.go:190-191 mutates `ds.TableStats.HistColl.Idx2ColUniqueIDs[path.Index.ID]` in place. Although it's a write to the `*statistics.HistColl` shape, the HistColl was constructed locally by `GenerateHistCollFromColumnInfo`, so this is read-side enrichment, not a write to the shared cache.

- pkg/planner/core/planbuilder.go:3005 uses `statistics.AnalyzeVersionMatchesForTableStats(statsHandle.GetPhysicalTableStats(...))` -- a version-check that ignores whether the stats are loaded; it only consults the table's persisted version metadata. This is the only "version-checked" caller in core; everywhere else, version is treated opaquely.

## Open questions

- pkg/planner/core/logical_plan_builder.go:4720-4727 iterates `ForEachColumnImmutable` / `ForEachIndexImmutable` reading `LastUpdateVersion` rather than `statsTbl.LastAnalyzeVersion` (referenced in the comment at :4719). Is the manual scan a deliberate fallback for unloaded stats, or should this be replaced once `LastAnalyzeVersion` is reliable?

- pkg/planner/core/rule/rule_collect_plan_stats.go calls `GetPhysicalTableStats` twice per table (lines 130 and 152). Is the second snapshot intended to observe state changes after sync-load completion, or is this redundant?

- pkg/planner/core/exhaust_physical_plans.go:1126 conditions on `ds.TableStats != nil` -- under what circumstances would `ds.TableStats` be nil at this stage? The `initStats` path always assigns it. Is this defensive code or a real branch?

- pkg/planner/core/index_join_path.go:411 explicitly uses `StatsVersion != PseudoVersion` for the pseudo check (with comment referencing #63869), while `find_best_task.go:865` uses `statsTbl.HistColl.Pseudo`. Are these intended to behave identically, or is the divergence load-bearing for some edge case?

- `pkg/planner/core/stats.go:565` constructs `ds.TblColHists = ds.StatisticTable.ID2UniqueID(ds.TblCols)` from the same source as `ds.TableStats.HistColl` (built at :542). After all downstream code paths, do these two HistColls ever diverge, or is `TblColHists` just a UniqueID-keyed view? (Relevant for understanding when reads through `TblColHists` may see stale data versus `TableStats.HistColl`.)
