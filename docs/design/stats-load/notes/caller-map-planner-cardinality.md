# pkg/planner/{cardinality, property, implementation} stats call sites

## Summary

The `cardinality` package is the principal consumer of `*statistics.HistColl` / `*statistics.Table` / `*statistics.Column` / `*statistics.Index` / `*statistics.Histogram` / `*statistics.TopN` / `*statistics.CMSketch` in the planner: row-count and selectivity estimation almost universally take a `*statistics.HistColl` parameter, call `coll.GetCol(...)` / `coll.GetIdx(...)`, validate the returned per-item stats with the `statistics.ColumnStatsIsInvalid` / `IndexStatsIsInvalid` predicates (which also trigger lazy stats load), and then read histogram, TopN, CMSketch, NDV, NullCount, RealtimeCount, ModifyCount, StatsVer, and load-status flags. `property/stats_info.go` doesn't read raw stats objects; it stores a pre-derived `*statistics.HistColl` plus `RowCount`/`ColNDVs`/`StatsVersion` snapshot. `implementation/` is almost entirely cost-only (it reads `*statistics.HistColl` only for row-size lookups in scan/reader implementations).

## Read sites by file

### pkg/planner/cardinality/cross_estimation.go

- 83 `crossEstimateTableRowCount` reads `*statistics.Table.Pseudo`
  Stage: AdjustRowCountForTableScanByLimit / table-scan limit correlation adjustment
  Tolerance: tolerant of pseudo (returns false / falls back to uniform)
  Fallback: caller (`AdjustRowCountForTableScanByLimit`) uses uniform-distribution estimate scaled by correlation factor
- 86 `crossEstimateTableRowCount` calls `getMostCorrCol4Handle(path.TableFilters, dsStatisticTable, ...)` which iterates `histColl.GetCol(col.ID)` then reads `hist.Correlation` (column-handle order correlation)
  Stage: same as above
  Tolerance: monotonic / read whatever is present, skip missing columns
  Fallback: returns nil column, ancestor disables correlation adjustment
- 135 `crossEstimateIndexRowCount` reads `*statistics.Table.Pseudo`
  Stage: AdjustRowCountForIndexScanByLimit
  Tolerance/fallback: same as table case
- 167 `crossEstimateRowCount` reads `dsStatsInfo.HistColl.ColUniqueID2IdxIDs` (index<->column mapping snapshot held inside the StatsInfo HistColl)
  Stage: cross-estimate range/limit adjustment
  Tolerance: any (it is the in-memory derived index map)
  Fallback: uses column histogram path
- 182, 186 `crossEstimateRowCount` recursively calls `GetRowCountByIndexRanges` / `GetRowCountByColumnRanges` on `dsTableStats.HistColl` -- reads the dsTableStats variant of the HistColl (the unfiltered table stats) to estimate the cross-column count.
  Stage: cross-estimate row count for table/index scan
  Tolerance: tolerant of pseudo (callers above already screen out pseudo tables)
  Fallback: returns ok=false, falls back to uniform * correlation
- 207-208 `getColumnRangeCounts` reads `histColl.GetIdx(idxID)` then probes `statistics.IndexStatsIsInvalid` (validates idx, may trigger sync load)
  Stage: per-range count for cross-estimation
  Tolerance: must be loaded before use (returns ok=false otherwise)
  Fallback: returns ok=false to crossEstimateRowCount
- 215-216 `getColumnRangeCounts` reads `histColl.GetCol(colID)` then `statistics.ColumnStatsIsInvalid` (same role for column fallback)
  Stage: same
  Tolerance/Fallback: same
- 273-277 `getMostCorrCol4Handle` reads `histColl.GetCol(col.ID)` and `hist.Correlation`
  Stage: pick most-correlated column to handle
  Tolerance: any (only reads .Correlation field)
  Fallback: skip column when nil

### pkg/planner/cardinality/ndv.go

- 40-50 `EstimateColumnNDV` reads `tbl.GetCol(colID)`, `hist.IsStatsInitialized()`, `hist.Histogram.NDV`, `tbl.RealtimeCount`; falls through to `tbl.RealtimeCount * distinctFactor`
  Stage: column NDV estimation for DataSource origin (used by DataSource.DeriveStats)
  Tolerance: tolerant of pseudo / missing (explicit fallback)
  Fallback: `RealtimeCount * distinctFactor` (0.8)
  Notes: scales NDV by `RealtimeCount / analyzeCount` to reflect growth
- 56-79 `getTotalRowCount` reads `colHist.IsFullLoad`, `colHist.TotalRowCount`, then `statsTbl.ForEachIndexImmutable` / `ForEachColumnImmutable` reading per-item `IsFullLoad`, `LastUpdateVersion`, `TotalRowCount`
  Stage: derive analyze-time row count for NDV scaling
  Tolerance: version-checked (matches `LastUpdateVersion`)
  Fallback: returns 0 if no matching loaded sibling
- 95 `EstimateColsNDVWithMatchedLen` reads `profile.GetGroupNDV4Cols` (group NDV from StatsInfo)
  Stage: derive multi-column NDV for join estimation
  Tolerance: any
  Fallback: falls through to single-col combination
- 139, 165 `estimateNaiveNDV` / `estimateNDVWithExponentialBackoff` read `profile.ColNDVs[col.UniqueID]` and `profile.RowCount`
  Stage: same as above
  Tolerance: any (StatsInfo is the derived snapshot)
  Fallback: defaults to 1.0

### pkg/planner/cardinality/pseudo.go

- 37 `PseudoAvgCountPerValue` reads `t.RealtimeCount`
  Stage: Selectivity fallback / index helpers
  Tolerance: any (lives behind pseudo paths)
  Fallback: caller computes count/pseudoEqualRate
- 53 `pseudoSelectivity` calls `statistics.ColumnStatsIsInvalid((*statistics.Column)(nil), sctx, coll, colID)` purely for its side-effect (request sync-load of the column)
  Stage: Selectivity (pseudo branch)
  Tolerance: any
  Fallback: continues with pseudoEqualRate
  Notes: relies on `ColumnStatsIsInvalid` triggering load-request side-effect
- 57-63 `pseudoSelectivity` reads `coll.GetCol(colID)`, `col.Info.Name`, `col.Info.GetFlag()`, `coll.RealtimeCount`
  Stage: same
  Tolerance: tolerant of pseudo
  Fallback: returns minFactor (typically SelectivityFactor)
- 75 `pseudoSelectivity` calls `coll.ForEachIndexImmutable`, reads `idx.Info.Columns`, `idx.Info.Unique`, `idx.ID`; also re-invokes `statistics.IndexStatsIsInvalid(... nil ...)` purely for sync-load side effect
  Stage: same
  Tolerance: tolerant of pseudo
  Fallback: returns 1/RealtimeCount only if a unique-key match is found
- 96 reads `coll.RealtimeCount` to produce `1.0 / RealtimeCount`
  Stage: same
  Tolerance: any
  Fallback: minFactor

### pkg/planner/cardinality/row_count_column.go

- 39 `GetRowCountByColumnRanges` reads `coll.GetCol(colUniqueID)`
- 41-42 reads `coll.UniqueID2colInfoID` (UID -> info ID map)
- 44 invokes `recordUsedItemStatsStatus` (records load status on the StatementContext, see trace.go)
- 45 `statistics.ColumnStatsIsInvalid(c, sctx, coll, colUniqueID)` (also triggers sync load)
- 52-57 reads `coll.RealtimeCount` for pseudo fallback
- 64 reads `coll.RealtimeCount`, `coll.ModifyCount` for the real path
  Stage: Selectivity / range estimation entry
  Tolerance: must be loaded before use (else pseudo branch)
  Fallback: pseudo signed/unsigned int range / pseudo column range
- 74-89 `equalRowCountOnColumn` reads `c.NullCount`, `c.StatsVer`, `c.Histogram.Bounds.NumRows()`, `c.Histogram.NDV`, `c.OutOfRange(val)`, `c.TotalRowCount()`, `c.CMSketch`, `c.TopN`, `c.Histogram.EqualRowCount`
  Stage: point-equal row count for column
  Tolerance: must be loaded before use
  Fallback: stats-v1 uses CMSketch / histogram bucket repeat; stats-v2 uses TopN, histogram, then uniform
- 95-117 `equalRowCountOnColumn` (V2) reads `c.TopN.Num()`, `c.TopN.QueryTopN`, `c.Histogram.EqualRowCount`, `IsLastBucketEndValueUnderrepresented` (which itself reads histogram fields), then falls into `estimateRowCountWithUniformDistribution`
  Stage: same
  Tolerance: must be loaded before use
  Fallback: uniform-distribution (see row_count_index.go)
- 161-162, 179-180, 217 `getColumnRowCount` reads `c.GetIncreaseFactor(realtimeRowCount)` (modify-aware scaling) and `c.NotNullCount`
  Stage: range row count
  Tolerance: monotonic
  Fallback: none -- internal use of already-validated column
- 224-230 reads `c.NDV`, `c.StatsVer`, `c.TopN.Num`, `c.Histogram.OutOfRangeRowCount` for out-of-range handling
  Stage: same
  Tolerance: must be loaded before use
  Fallback: histogram OutOfRange path
- 243 `betweenRowCountOnColumn` reads `c.Histogram.BetweenRowCount`, `c.StatsVer`, `c.TopN.BetweenCount`
  Stage: between-range estimation
  Tolerance: must be loaded before use
  Fallback: histogram-only when V1
- 295 `getPseudoRowCountWithPartialStats` recurses into `GetRowCountByColumnRanges` per column for an index lacking its own stats
  Stage: pseudo-with-partial-stats fallback for index ranges
  Tolerance: tolerant of pseudo (returns count derived from column stats or pseudo per column)
  Fallback: per-column pseudo

### pkg/planner/cardinality/row_count_index.go

- 44 `GetRowCountByIndexRanges` reads `coll.GetIdx(idxID)`
- 45 records load status
- 46 `statistics.IndexStatsIsInvalid(sctx, idx, coll, idxID)` (also triggers sync load)
- 47-58 reads `coll.RealtimeCount`, `idx.Info.Unique`, `idx.Info.Columns` for pseudo branch
  Stage: index range estimation entry
  Tolerance: must be loaded before use (else pseudo branch)
  Fallback: `getPseudoRowCountWithPartialStats` if usable column stats exist, otherwise `getPseudoRowCountByIndexRanges` (column count / pseudo rate)
- 60, 91 `coll.GetScaledRealtimeAndModifyCnt(idx)` -- scaled real-time + modify count for the index (compensates for index lag behind table updates)
  Stage: same
  Tolerance: monotonic / version-checked internally
  Fallback: returns table-wide counts when no scaling possible
- 61 `canSkipIndexEstimation(idx, indexRanges)` reads `idx.Info.ConditionExprString`, `idx.Info.MVIndex`
  Stage: same
  Tolerance: any (schema info, not stats)
  Fallback: returns false, proceed with regular estimation
- 64-69 reads `idx.CMSketch`, `idx.StatsVer`, then calls V1 / V2 estimators
  Stage: same
  Tolerance: must be loaded before use
- 73-174 `getIndexRowCountForStatsV1` reads `idx.Info.Columns` via `isSingleColIdxNullRange`, calls `coll.GetScaledRealtimeAndModifyCnt(idx)`, uses `getEqualCondSelectivity` (CMSketch via `idx.QueryBytes` / fallback) and `idx.TotalRowCount`; reads `coll.Idx2ColUniqueIDs` and `coll.ColUniqueID2IdxIDs` for column/index mapping; recurses into `GetRowCountByIndexRanges` / `GetRowCountByColumnRanges`.
  Stage: index range estimation for stats v1 (CMSketch)
  Tolerance: must be loaded before use
  Fallback: histogram-based estimate inside recursion
- 191, 205, 213-218, 221-223 `getIndexRowCountForStatsV2` reads `idx.Info.Columns`, `idx.Info.Unique`, `idx.NullCount`, `equalRowCountOnIndex` (which reads `idx.Histogram.NullCount`, `idx.StatsVer`, `idx.CMSketch`, `idx.QueryBytes`, `idx.TopN`, `idx.Histogram.EqualRowCount`, `idx.Histogram.NDV`, `idx.TopN.Num`, `idx.TotalRowCount`, `idx.GetIncreaseFactor`)
  Stage: index range estimation for stats v2 (Histogram+TopN)
  Tolerance: must be loaded before use
  Fallback: uniform-distribution estimate
- 241-296 reads `idx.Histogram.NullCount`, `idx.Histogram.Len`, `idx.Histogram.LocateBucket`, `idx.Histogram.Buckets[i].Count`, `idx.TopN.BetweenCount`, `idx.GetIncreaseFactor`, `idx.NDV`, `idx.StatsVer`, `idx.TopN.Num`, `idx.Histogram.OutOfRangeRowCount`
  Stage: same V2 path with exponential backoff + out-of-range
  Tolerance: must be loaded before use
  Fallback: histogram-only without TopN if V1
- 298, 303-308 reads `coll.GetCol(colIDs[0])`, `c.Histogram.NDV`, `c.Histogram.Len`, `c.TopN.Num`, `c.Histogram.OutOfRangeRowCount` (column stats used to refine out-of-range estimate for single-col index range)
  Stage: same
  Tolerance: must be loaded before use
  Fallback: index-only out-of-range estimate
- 339-389 `estimateRowCountWithUniformDistribution` reads `stats.GetHistogram().NDV`, `stats.GetTopN().Num()`, `stats.TotalRowCount()`, `stats.GetIncreaseFactor()`, `histogram.NotNullCount()`, `histogram.NullCount`, `topN.MinCount`
  Stage: shared uniform-distribution fallback for column/index equal estimation
  Tolerance: must be loaded before use (caller ensures it)
  Fallback: uniform NDV-based + skew estimate
- 396 `equalRowCountOnIndex` reads `idx.Histogram.NullCount` (single-col null fast path)
  Stage: equal-on-index
  Tolerance/Fallback: same
- 445 `expBackoffEstimation` reads `coll.Idx2ColUniqueIDs`, then per-column `coll.GetCol(colID)` + `ColumnStatsIsInvalid`, recurses into `GetRowCountByColumnRanges`, reads `coll.RealtimeCount` for selectivity ratio; also recurses into other indexes via `coll.ColUniqueID2IdxIDs` and `coll.GetIdx`.
  Stage: multi-column index exponential backoff
  Tolerance: must be loaded before use (skips invalid columns)
  Fallback: skip column from backoff product
- 522-526 reads `coll.RealtimeCount`, `idx.NDV`, `idx.Info.Columns` to bound exponential backoff
  Stage: same
  Tolerance: monotonic
  Fallback: uses RealtimeCount as bound
- 551-557 `outOfRangeOnIndex` reads `idx.Histogram.OutOfRange`, `idx.Histogram.Len`, `idx.Histogram.Bounds.GetRow(0)`
  Stage: range bound check
  Tolerance: must be loaded before use
- 572-577 `betweenRowCountOnIndex` reads `idx.Histogram.BetweenRowCount`, `idx.StatsVer`, `idx.TopN.BetweenCount`
  Stage: between-range estimation on index
  Tolerance: must be loaded before use
- 603-607 `canSkipIndexEstimation` reads `idx.Info.ConditionExprString`, `idx.Info.MVIndex` (schema flags only)
  Stage: short-circuit when full-range
- 629-637 `hasColumnStats` reads `coll.GetCol(...)` and `ColumnStatsIsInvalid` for each idx column
  Stage: choose pseudo-with-column-stats branch in index estimation
  Tolerance: must be loaded before use
  Fallback: returns false, falls back to pure pseudo

### pkg/planner/cardinality/row_size.go

- 65 `GetAvgRowSize` reads `coll.Pseudo`, `coll.ColNum()`, `coll.RealtimeCount` (header gate)
- 69 `coll.GetCol(col.UniqueID)`
- 72 reads `colHist.IsHandle`, `colHist.TotColSize`, `colHist.NullCount`, `coll.RealtimeCount` (compatibility check)
- 79-82 calls `AvgColSizeChunkFormat` / `AvgColSize`
  Stage: scan/reader cost calculation (TableScan/IndexScan/Reader)
  Tolerance: tolerant of pseudo
  Fallback: `pseudoColSize` (8) per column when missing/pseudo
- 97 `GetAvgRowSizeDataInDiskByRows` reads `coll.Pseudo`, `coll.ColNum`, `coll.RealtimeCount`
- 103-110 `coll.GetCol`, `colHist.IsHandle`, `colHist.TotColSize`, `colHist.NullCount`, `coll.RealtimeCount`
  Stage: spill-cost estimation
  Tolerance/Fallback: same as above
- 119-144 `AvgColSize` reads `c.IsHandle`, `c.TotalRowCount`, `c.NullCount`, `c.Histogram.Tp.GetType`, `c.TotColSize`
  Stage: per-column size
  Tolerance: must be loaded before use (caller ensures)
  Fallback: caller routes to `pseudoColSize` when histogram missing
- 149-164 `AvgColSizeChunkFormat` reads `c.Histogram.Tp`, `c.TotColSize`
- 169-187 `AvgColSizeDataInDiskByRows` reads `c.TotalRowCount`, `c.NullCount`, `c.Histogram.Tp`, `c.TotColSize`
  Stage/Tolerance/Fallback: same

### pkg/planner/cardinality/selectivity.go

- 61, 66 `Selectivity` reads `coll.RealtimeCount`, `coll.PhysicalID`
  Stage: Selectivity (top-level CNF selectivity)
  Tolerance: any
  Fallback: returns 1.0 if RealtimeCount == 0
- 69 `coll.ColNum()`, `coll.IdxNum()` -- entry into pseudoSelectivity when nothing usable
  Stage: same
  Tolerance: tolerant of pseudo
  Fallback: `pseudoSelectivity`
- 86-94 correlated-column handling reads `coll.GetCol(c.UniqueID)`, `colHist.Histogram.NDV`, gated by `ColumnStatsIsInvalid`
  Stage: same
  Tolerance: tolerant of pseudo
  Fallback: 1/pseudoEqualRate
- 112-137 per-column path reads `coll.GetCol(id)`, `colStats.IsHandle`, calls `GetRowCountByColumnRanges`, divides by `coll.RealtimeCount`
  Stage: per-column selectivity node
  Tolerance: must be loaded before use (else implicitly pseudo via GetRowCountByColumnRanges)
  Fallback: pseudo via inner call
- 140-141 explicit `ColumnStatsIsInvalid(nil, ...)` + `recordUsedItemStatsStatus(... nil ...)` to record missing-stats load request when col is absent
  Stage: missing-column trace path
  Tolerance: any (side-effecting load request)
  Fallback: column omitted from selectivity nodes
  Notes: comment "We are able to remove this path if we remove the async stats load."
- 153 `coll.ForEachIndexImmutable` enumerates available indexes; per-index `coll.GetIdx(id)`, `idxStats.Info.MVIndex`, `idxStats.Info.Columns`, `idxStats.ID`, `coll.Idx2ColUniqueIDs[id]`
  Stage: per-index selectivity nodes
  Tolerance: must be loaded before use
  Fallback: index skipped or pseudo via inner call
- 192-196 calls `GetRowCountByIndexRanges`, divides by `coll.RealtimeCount`
  Stage: same
  Tolerance: same
  Fallback: same
- 338 inside DNF coverage: `coll.GetCol(cols[i].UniqueID)` to gate DNF estimation (skip if any col has no stats)
  Stage: DNF independence-assumption estimation
  Tolerance: any (just nil-check)
  Fallback: skip DNF item
- 481-498 `CalcTotalSelectivityForMVIdxPath` reads `coll.RealtimeCount`, `coll.MVIdx2Columns`, `coll.GetIdx(path.Index.ID)`, `coll.GetScaledRealtimeAndModifyCnt`
  Stage: MV index path selectivity
  Tolerance: tolerant of pseudo
  Fallback: uses table RealtimeCount when MV path can't be specialised
- 583-610 `IsLastBucketEndValueUnderrepresented` reads `hg.Buckets`, `hg.AbsRowCountDifference`, `hg.NotNullCount`, `hg.LocateBucket`, `hg.Buckets[len-1]`
  Stage: V2 equal-row-count stale-bucket heuristic
  Tolerance: must be loaded before use
  Fallback: returns false, caller uses histogram count as-is
- 869, 876 `getMaskAndSelectivityForMVIndex` reads `coll.MVIdx2Columns[id]`, `coll.GetIdx(id).Info`, `coll` passed to `BuildPartialPaths4MVIndex`
  Stage: MV index selectivity within Selectivity
  Tolerance: must be loaded before use
  Fallback: returns ok=false, selectivity excluded
- 921-948 `GetSelectivityByFilter` (TopN-assisted estimation for LIKE / REGEX / NOT-LIKE / FTS fallback):
  - 922 `findAvailableStatsForCol` reads `coll.GetCol`, `ColumnStatsIsInvalid`, `colStats.IsFullLoad`, `coll.Idx2ColUniqueIDs`, `coll.GetIdx`, `IndexStatsIsInvalid`, `idxStats.Info.Columns[0].Length`, `idxStats.IsFullLoad`
  - 931-948 reads `stats.StatsVer`, `stats.Histogram`, `Histogram.NullCount`, `stats.TopN`, `topn.TotalCount`, `hist.NotNullCount`
  Stage: TopN-and-histogram-driven selectivity for non-range predicates
  Tolerance: must be loaded before use (`IsFullLoad` is the explicit gate)
  Fallback: returns ok=false, caller uses default str-match selectivity
- 964-1027 reads `hist.Len`, `topn.TopN[i].Encoded` / `.Count`, `hist.Bounds`, `hist.Buckets[i].Repeat` to feed expressions and sum selectivity components
  Stage: same
  Tolerance: must be loaded before use
- 1038 reads `nullCnt` for NULL part
  Stage: same
- 1048 `findAvailableStatsForCol` reads `colStats.IsFullLoad` (gate that requires full histogram + TopN)
  Stage: TopN filter eligibility
  Tolerance: must be loaded before use; requires full load
  Fallback: returns idx = -1 -> caller falls back
- 1054-1057 reads `idxStats.IsFullLoad`, `idxStats.Info.Columns[0].Length`
  Stage: same
- 1071 `getEqualCondSelectivity` reads `idx.TotalRowCount`, `idx.Info.Columns`, `idx.Info.Unique`, `coll.GetScaledRealtimeAndModifyCnt(idx)`, `idx.NDV`, `coll.Idx2ColUniqueIDs[idx.ID]`, `coll.GetCol(colID)`, `col.Histogram.NDV`, `idx.QueryBytes`
  Stage: equal-cond selectivity for stats v1 (CMSketch-based)
  Tolerance: must be loaded before use
  Fallback: out-of-range heuristic via `outOfRangeEQSelectivity`
- 1178-1199 `crossValidationSelectivity` reads `coll.Idx2ColUniqueIDs[idx.ID]`, per-column `coll.GetCol(colID)`, `ColumnStatsIsInvalid`, then `getColumnRowCount(... coll.RealtimeCount, coll.ModifyCount ...)`
  Stage: cross-validation for multi-column equal cond on index
  Tolerance: must be loaded before use (skips invalid columns)
  Fallback: returns minRowCount=MaxFloat64 / 1.0 sel

### pkg/planner/cardinality/trace.go

- 33-46 `recordUsedItemStatsStatus` reads `(*statistics.Column).StatsLoadedStatus` / `(*statistics.Index).StatsLoadedStatus`, calls `loadStatus.IsFullLoad`, `loadStatus.StatusToString`
  Stage: stats-load-status tracing (called from each row-count entry)
  Tolerance: any
  Fallback: marks "missing" / uninitialized depending on `ColAndIdxExistenceMap.HasAnalyzed`
- 80 reads `recordForTbl.ColAndIdxStatus.(*statistics.ColAndIdxExistenceMap).HasAnalyzed(id, isIndex)` to distinguish "missing" vs "uninitialized"
  Stage: same
  Tolerance: any
  Fallback: "missing" label

### pkg/planner/property/stats_info.go

- 49 `StatsInfo` struct field `HistColl *statistics.HistColl` -- every consumer that touches `profile.HistColl` is a downstream read (this is the derived HistColl snapshot threaded through plan trees)
  Stage: property-derivation (one per Logical/Physical operator)
  Tolerance: any (snapshot taken at logical plan time)
  Fallback: `StatsVersion == PseudoVersion` signals pseudo origin
- 64-99 `Count` / `Scale` / `ScaleByExpectCnt` -- read `s.RowCount`, `s.ColNDVs`, propagate `HistColl`/`StatsVersion`
  Stage: stats propagation through Scale operations
  Tolerance: any (uses derived NDVs only)
- 102-122 `GetGroupNDV4Cols` reads `s.GroupNDVs[i].Cols` / `.NDV` (multi-column NDV memo)
  Stage: join NDV / agg NDV path
  Tolerance: any
  Fallback: returns nil -- callers fall back to single-col NDV combination
- 126-137 `DeriveLimitStats` reads `childProfile.RowCount`, `childProfile.ColNDVs`, `childProfile.HistColl`
  Stage: Limit/TopN propagation
  Tolerance: any
  Fallback: none (works off derived snapshot)

### pkg/planner/property/logical_property.go

- 26 `LogicalProperty.Stats *StatsInfo` -- container only; not a direct stats read

### pkg/planner/property/physical_property.go and task_type.go

- No statistics reads (verified by grep for `statistics.` -- empty result).

### pkg/planner/implementation/datasource.go

- 64-74 `TableReaderImpl{tblColHists *statistics.HistColl}` captures `source.TblColHists` at construction time
- 81 `cardinality.GetAvgRowSize(... impl.tblColHists ...)` -- TableReader cost
  Stage: Implementation.CalcCost (TableReader)
  Tolerance: any (uses captured snapshot)
  Fallback: pseudoColSize via GetAvgRowSize
- 109-128 `TableScanImpl` captures `*statistics.HistColl`, calls `cardinality.GetTableAvgRowSize`
  Stage: Implementation.CalcCost (TableScan)
  Tolerance/Fallback: same
- 141, 160 `IndexReaderImpl` captures `*statistics.HistColl`, calls `cardinality.GetAvgRowSize`
  Stage: Implementation.CalcCost (IndexReader)
  Tolerance/Fallback: same
- 179, 186 `IndexScanImpl` captures `*statistics.HistColl`, calls `cardinality.GetIndexAvgRowSize`
  Stage: Implementation.CalcCost (IndexScan)
  Tolerance/Fallback: same

### pkg/planner/implementation/base.go, join.go, simple_plans.go, sort.go

- These call `child.GetPlan().StatsInfo().RowCount` / `StatsCount()` only. They read the derived `RowCount` field of the upstream `*property.StatsInfo`, not the underlying statistics.* objects.
  Stage: per-operator cost (HashJoin, MergeJoin, Projection, TiDB/TiKV Selection, TiDB/TiKV HashAgg, TiDB/TiKV TopN, Sort, Apply, UnionAll, Window, etc.)
  Tolerance: any (derived snapshot)
  Fallback: none -- derived snapshot must exist on every operator

## Cardinality estimation cheat-sheet

- Column NDV alone (`hist.Histogram.NDV`) -> equality selectivity `1/NDV` (selectivity.go correlated-col branch; `EstimateColumnNDV`).
- Column histogram + TopN + nullCount (`*statistics.Column` V2) -> range estimation in `getColumnRowCount` / `betweenRowCountOnColumn`; equal-via-bucket-repeat in `equalRowCountOnColumn` V2.
- CMSketch (`c.CMSketch`, `idx.CMSketch`) -> stats V1 equality estimation via `QueryValue` / `idx.QueryBytes` (`equalRowCountOnColumn` V1, `equalRowCountOnIndex` V1, `getEqualCondSelectivity`).
- Index histogram + TopN -> V2 index range estimation in `getIndexRowCountForStatsV2`, `betweenRowCountOnIndex`, `equalRowCountOnIndex`.
- TopN alone (high-skew values, `topn.QueryTopN`) -> exact count for hot values inside V2 equality.
- Histogram OutOfRange handling -> uses `realtimeRowCount - origRowCount` (table growth) plus `NDV` to spread; if NDV<100 floors to `outOfRangeBetweenRate=100`.
- Multi-column index: exponential backoff (`expBackoffEstimation`) blends per-column histograms; cross-validation (`crossValidationSelectivity`) bounds via column equal counts.
- MV index: `coll.MVIdx2Columns` + `BuildPartialPaths4MVIndex` per partial path, combined via `CalcTotalSelectivityForMVIdxPath`.
- Group NDV (multi-column NDV memo on `*StatsInfo`) -> direct NDV for join `EstimateColsNDVWithMatchedLen`; falls through to `estimateNaiveNDV` (max single-col NDV) or `estimateNDVWithExponentialBackoff` blended by `RiskGroupNDVSkewRatio`.
- Hist correlation (`hist.Correlation`) -> only `getMostCorrCol4Handle` for cross-estimation by limit.
- Pseudo flag (`coll.Pseudo`, `table.Pseudo`) -> short-circuits everything to `pseudoColSize`, `pseudoEqualRate`, `pseudoLessRate`, `pseudoBetweenRate`, or `SelectivityFactor`.
- `IsStatsInitialized` -> guards `EstimateColumnNDV`; missing -> `RealtimeCount * 0.8` (distinct factor).
- `IsFullLoad` -> hard precondition for TopN-assisted filter evaluation (`GetSelectivityByFilter`/`findAvailableStatsForCol`) and for `getTotalRowCount` to use a sibling's `TotalRowCount`.
- Modify count / RealtimeCount (`coll.ModifyCount`, `coll.RealtimeCount`, plus per-item `GetIncreaseFactor`, `GetScaledRealtimeAndModifyCnt`) -> scaling factors for growth-aware estimation and out-of-range adjustment.
- `*property.StatsInfo.RowCount` / `ColNDVs` / `GroupNDVs` -> the derived-snapshot inputs read by `implementation/*` and `cardinality/ndv.go`; never bypassed once derived.

## Out of pattern

- `pseudo.go:53` and `pseudo.go:87` call `statistics.ColumnStatsIsInvalid(nil, ...)` / `IndexStatsIsInvalid(... nil ...)` deliberately with a nil stats arg purely to trigger the sync-load side effect; this is not a read of any stats field but is a load-request emission.
- `selectivity.go:140-141` performs the same side-effect call plus `recordUsedItemStatsStatus(... nil ...)` for columns absent from `coll`, with an in-line comment noting it could be removed if async load were removed.
- `cross_estimation.go:171,182,186` works on two HistColls simultaneously: `dsStatsInfo.HistColl` (the filtered/derived StatsInfo HistColl, used for the index-to-column map) and `dsTableStats.HistColl` (the unfiltered table HistColl, used to compute counts). This dual-HistColl access is unique within these three packages.
- `ndv.go:62-79` walks all siblings on the table (`ForEachIndexImmutable`, `ForEachColumnImmutable`) looking for one matching `LastUpdateVersion` and `IsFullLoad` -- uses `LastUpdateVersion` as a join key across stats items.
- `trace.go` reads `recordForTbl.ColAndIdxStatus.(*statistics.ColAndIdxExistenceMap).HasAnalyzed` (the existence map, not the stats payload) to distinguish "missing" vs "uninitialized" in load tracing.
- `selectivity.go:921-967` is the only place that requires `IsFullLoad` explicitly (it walks `hist.Buckets`, `hist.Bounds.GetRow`, `topn.TopN[i].Encoded`, which need the full payload, not the lite/load-status surrogates).
- `implementation/datasource.go` captures `*statistics.HistColl` at Implementation construction (from `source.TblColHists` / scan ctor), so the stats snapshot is frozen at the moment the implementation is built and re-used in every subsequent `CalcCost`. Other implementation files (`base.go`, `join.go`, `simple_plans.go`, `sort.go`) never touch the HistColl directly; they only read `StatsInfo.RowCount` / `StatsCount`.

## Open questions

- Tolerance classification for the `ColumnStatsIsInvalid` / `IndexStatsIsInvalid` calls is recorded as "must be loaded before use" because they trigger sync-load via the planner ctx, but the actual load semantics (timeout, lazy fallback, retry) live in `pkg/statistics/handle/syncload` and are not directly observable here; if those calls return without loading (timeout / disabled), the calling code transparently falls into the pseudo branch -- this is documented as "tolerant of pseudo" at the higher level but the underlying call still claims invalidity, so the dual classification depends on the contract being defined in the broader Phase 0a work.
- `coll.GetScaledRealtimeAndModifyCnt(idx)` is treated as monotonic / version-checked; its precise semantics (and whether the scaled values can disagree across siblings within one `HistColl`) require verifying the implementation under `pkg/statistics`.
- `recordUsedItemStatsStatus(ctx, (*statistics.Column)(nil), tableID, col.ID)` at `selectivity.go:141` is followed by another `ColumnStatsIsInvalid(nil, ctx, coll, col.ID)` at line 140 (same logical block); whether both are needed or one is redundant is unclear from this directory alone.
- `cardinality/cross_estimation.go:171-182` uses `dsStatsInfo.HistColl.ColUniqueID2IdxIDs` but then estimates against `dsTableStats.HistColl`. Whether these two HistColls are guaranteed to share the same index ID space across all call paths (DataSource pre/post-prune cases) is not derivable from this directory.
- The `BuildPartialPaths4MVIndex` / `CollectFilters4MVIndex` indirection (`selectivity.go:1216-1238`) is implemented in `planner/core` and the precise stats reads it does (beyond `coll`) are out of scope for this slice; full mapping needs `planner/core` to be inventoried.
