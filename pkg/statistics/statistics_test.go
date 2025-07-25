// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package statistics

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/planner/core/resolve"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/sessionctx/stmtctx"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/codec"
	"github.com/pingcap/tidb/pkg/util/collate"
	"github.com/pingcap/tidb/pkg/util/mock"
	"github.com/pingcap/tidb/pkg/util/ranger"
	"github.com/pingcap/tidb/pkg/util/sqlexec"
	"github.com/stretchr/testify/require"
)

type testStatisticsSamples struct {
	count   int
	samples []*SampleItem
	rc      sqlexec.RecordSet
	pk      sqlexec.RecordSet
}

type recordSet struct {
	firstIsID bool
	data      []types.Datum
	count     int
	cursor    int
	fields    []*resolve.ResultField
}

func (r *recordSet) Fields() []*resolve.ResultField {
	return r.fields
}

func (r *recordSet) setFields(tps ...uint8) {
	r.fields = make([]*resolve.ResultField, len(tps))
	for i := range tps {
		rf := new(resolve.ResultField)
		rf.Column = new(model.ColumnInfo)
		rf.Column.FieldType = *types.NewFieldType(tps[i])
		r.fields[i] = rf
	}
}

func (r *recordSet) getNext() []types.Datum {
	if r.cursor == r.count {
		return nil
	}
	r.cursor++
	row := make([]types.Datum, 0, len(r.fields))
	if r.firstIsID {
		row = append(row, types.NewIntDatum(int64(r.cursor)))
	}
	row = append(row, r.data[r.cursor-1])
	return row
}

func (r *recordSet) Next(_ context.Context, req *chunk.Chunk) error {
	req.Reset()
	row := r.getNext()
	for i := range row {
		req.AppendDatum(i, &row[i])
	}
	return nil
}

func (r *recordSet) NewChunk(chunk.Allocator) *chunk.Chunk {
	fields := make([]*types.FieldType, 0, len(r.fields))
	for _, field := range r.fields {
		fields = append(fields, &field.Column.FieldType)
	}
	return chunk.NewChunkWithCapacity(fields, 32)
}

func (r *recordSet) Close() error {
	r.cursor = 0
	return nil
}

func buildPK(sctx sessionctx.Context, numBuckets, id int64, records sqlexec.RecordSet) (int64, *Histogram, error) {
	b := NewSortedBuilder(sctx.GetSessionVars().StmtCtx, numBuckets, id, types.NewFieldType(mysql.TypeLonglong), Version1)
	ctx := context.Background()
	for {
		req := records.NewChunk(nil)
		err := records.Next(ctx, req)
		if err != nil {
			return 0, nil, errors.Trace(err)
		}
		if req.NumRows() == 0 {
			break
		}
		it := chunk.NewIterator4Chunk(req)
		for row := it.Begin(); row != it.End(); row = it.Next() {
			datums := RowToDatums(row, records.Fields())
			err = b.Iterate(datums[0])
			if err != nil {
				return 0, nil, errors.Trace(err)
			}
		}
	}
	return b.Count, b.hist, nil
}

func mockHistogram(lower, num int64) *Histogram {
	h := NewHistogram(0, num, 0, 0, types.NewFieldType(mysql.TypeLonglong), int(num), 0)
	for i := int64(0); i < num; i++ {
		lower, upper := types.NewIntDatum(lower+i), types.NewIntDatum(lower+i)
		h.AppendBucket(&lower, &upper, i+1, 1)
	}
	return h
}

func TestMergeHistogram(t *testing.T) {
	tests := []struct {
		leftLower  int64
		leftNum    int64
		rightLower int64
		rightNum   int64
		bucketNum  int
		ndv        int64
	}{
		{
			leftLower:  0,
			leftNum:    0,
			rightLower: 0,
			rightNum:   1,
			bucketNum:  1,
			ndv:        1,
		},
		{
			leftLower:  0,
			leftNum:    200,
			rightLower: 200,
			rightNum:   200,
			bucketNum:  200,
			ndv:        400,
		},
		{
			leftLower:  0,
			leftNum:    200,
			rightLower: 199,
			rightNum:   200,
			bucketNum:  200,
			ndv:        399,
		},
	}
	sc := mock.NewContext().GetSessionVars().StmtCtx
	bucketCount := 256
	for _, tt := range tests {
		lh := mockHistogram(tt.leftLower, tt.leftNum)
		rh := mockHistogram(tt.rightLower, tt.rightNum)
		h, err := MergeHistograms(sc, lh, rh, bucketCount, Version1)
		require.NoError(t, err)
		require.Equal(t, tt.ndv, h.NDV)
		require.Equal(t, tt.bucketNum, h.Len())
		require.Equal(t, tt.leftNum+tt.rightNum, int64(h.TotalRowCount()))
		expectLower := types.NewIntDatum(tt.leftLower)
		cmp, err := h.GetLower(0).Compare(sc.TypeCtx(), &expectLower, collate.GetBinaryCollator())
		require.NoError(t, err)
		require.Equal(t, 0, cmp)
		expectUpper := types.NewIntDatum(tt.rightLower + tt.rightNum - 1)
		cmp, err = h.GetUpper(h.Len()-1).Compare(sc.TypeCtx(), &expectUpper, collate.GetBinaryCollator())
		require.NoError(t, err)
		require.Equal(t, 0, cmp)
	}
}

func buildCMSketch(values []types.Datum) *CMSketch {
	cms := NewCMSketch(8, 2048)
	for _, val := range values {
		err := cms.insert(&val)
		if err != nil {
			panic(err)
		}
	}
	return cms
}

func SubTestColumnRange() func(*testing.T) {
	return func(t *testing.T) {
		s := createTestStatisticsSamples(t)
		bucketCount := int64(256)
		ctx := mock.NewContext()
		sc := ctx.GetSessionVars().StmtCtx
		sketch, _, err := buildFMSketch(sc, s.rc.(*recordSet).data, 1000)
		require.NoError(t, err)

		collector := &SampleCollector{
			Count:     int64(s.count),
			NullCount: 0,
			Samples:   s.samples,
			FMSketch:  sketch,
		}
		hg, err := BuildColumn(ctx, bucketCount, 2, collector, types.NewFieldType(mysql.TypeLonglong))
		hg.PreCalculateScalar()
		require.NoError(t, err)
		col := &Column{
			Histogram:         *hg,
			CMSketch:          buildCMSketch(s.rc.(*recordSet).data),
			Info:              &model.ColumnInfo{},
			StatsLoadedStatus: NewStatsFullLoadStatus(),
		}
		tbl := &Table{
			HistColl: *NewHistCollWithColsAndIdxs(0, int64(col.TotalRowCount()), 0, make(map[int64]*Column), make(map[int64]*Index)),
		}
		ran := []*ranger.Range{{
			LowVal:    []types.Datum{{}},
			HighVal:   []types.Datum{types.MaxValueDatum()},
			Collators: collate.GetBinaryCollatorSlice(1),
		}}
		count, err := GetRowCountByColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 100000, int(count))
		ran[0].LowVal[0] = types.MinNotNullDatum()
		count, err = GetRowCountByColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 99900, int(count))
		ran[0].LowVal[0] = types.NewIntDatum(1000)
		ran[0].LowExclude = true
		ran[0].HighVal[0] = types.NewIntDatum(2000)
		ran[0].HighExclude = true
		count, err = GetRowCountByColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 2500, int(count))
		ran[0].LowExclude = false
		ran[0].HighExclude = false
		count, err = GetRowCountByColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 2500, int(count))
		ran[0].LowVal[0] = ran[0].HighVal[0]
		count, err = GetRowCountByColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 100, int(count))

		tbl.SetCol(0, col)
		ran[0].LowVal[0] = types.Datum{}
		ran[0].HighVal[0] = types.MaxValueDatum()
		count, err = GetRowCountByColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 100000, int(count))
		ran[0].LowVal[0] = types.NewIntDatum(1000)
		ran[0].LowExclude = true
		ran[0].HighVal[0] = types.NewIntDatum(2000)
		ran[0].HighExclude = true
		count, err = GetRowCountByColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 9998, int(count))
		ran[0].LowExclude = false
		ran[0].HighExclude = false
		count, err = GetRowCountByColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 10000, int(count))
		ran[0].LowVal[0] = ran[0].HighVal[0]
		count, err = GetRowCountByColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 1, int(count))
	}
}

func SubTestIntColumnRanges() func(*testing.T) {
	return func(t *testing.T) {
		s := createTestStatisticsSamples(t)
		bucketCount := int64(256)
		ctx := mock.NewContext()

		s.pk.(*recordSet).cursor = 0
		rowCount, hg, err := buildPK(ctx, bucketCount, 0, s.pk)
		hg.PreCalculateScalar()
		require.NoError(t, err)
		require.Equal(t, int64(100000), rowCount)
		col := &Column{Histogram: *hg, Info: &model.ColumnInfo{}, StatsLoadedStatus: NewStatsFullLoadStatus()}
		tbl := &Table{
			HistColl: *NewHistCollWithColsAndIdxs(0, int64(col.TotalRowCount()), 0, make(map[int64]*Column), make(map[int64]*Index)),
		}
		ran := []*ranger.Range{{
			LowVal:    []types.Datum{types.NewIntDatum(math.MinInt64)},
			HighVal:   []types.Datum{types.NewIntDatum(math.MaxInt64)},
			Collators: collate.GetBinaryCollatorSlice(1),
		}}
		count, err := GetRowCountByIntColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 100000, int(count))
		ran[0].LowVal[0].SetInt64(1000)
		ran[0].HighVal[0].SetInt64(2000)
		count, err = GetRowCountByIntColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 1000, int(count))
		ran[0].LowVal[0].SetInt64(1001)
		ran[0].HighVal[0].SetInt64(1999)
		count, err = GetRowCountByIntColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 998, int(count))
		ran[0].LowVal[0].SetInt64(1000)
		ran[0].HighVal[0].SetInt64(1000)
		count, err = GetRowCountByIntColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 1, int(count))

		ran = []*ranger.Range{{
			LowVal:    []types.Datum{types.NewUintDatum(0)},
			HighVal:   []types.Datum{types.NewUintDatum(math.MaxUint64)},
			Collators: collate.GetBinaryCollatorSlice(1),
		}}
		count, err = GetRowCountByIntColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 100000, int(count))
		ran[0].LowVal[0].SetUint64(1000)
		ran[0].HighVal[0].SetUint64(2000)
		count, err = GetRowCountByIntColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 1000, int(count))
		ran[0].LowVal[0].SetUint64(1001)
		ran[0].HighVal[0].SetUint64(1999)
		count, err = GetRowCountByIntColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 998, int(count))
		ran[0].LowVal[0].SetUint64(1000)
		ran[0].HighVal[0].SetUint64(1000)
		count, err = GetRowCountByIntColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 1, int(count))

		tbl.SetCol(0, col)
		ran[0].LowVal[0].SetInt64(math.MinInt64)
		ran[0].HighVal[0].SetInt64(math.MaxInt64)
		count, err = GetRowCountByIntColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 100000, int(count))
		ran[0].LowVal[0].SetInt64(1000)
		ran[0].HighVal[0].SetInt64(2000)
		count, err = GetRowCountByIntColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 1001, int(count))
		ran[0].LowVal[0].SetInt64(1001)
		ran[0].HighVal[0].SetInt64(1999)
		count, err = GetRowCountByIntColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 999, int(count))
		ran[0].LowVal[0].SetInt64(1000)
		ran[0].HighVal[0].SetInt64(1000)
		count, err = GetRowCountByIntColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 1, int(count))

		tbl.RealtimeCount *= 10
		count, err = GetRowCountByIntColumnRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 1, int(count))
	}
}

func SubTestIndexRanges() func(*testing.T) {
	return func(t *testing.T) {
		s := createTestStatisticsSamples(t)
		bucketCount := int64(256)
		ctx := mock.NewContext()

		s.rc.(*recordSet).cursor = 0
		rowCount, hg, cms, err := buildIndex(ctx, bucketCount, 0, s.rc)
		hg.PreCalculateScalar()
		require.NoError(t, err)
		require.Equal(t, int64(100000), rowCount)
		idxInfo := &model.IndexInfo{Columns: []*model.IndexColumn{{Offset: 0}}}
		idx := &Index{Histogram: *hg, CMSketch: cms, Info: idxInfo}
		tbl := &Table{
			HistColl: *NewHistCollWithColsAndIdxs(0, int64(idx.TotalRowCount()), 0, nil, make(map[int64]*Index)),
		}
		ran := []*ranger.Range{{
			LowVal:    []types.Datum{types.MinNotNullDatum()},
			HighVal:   []types.Datum{types.MaxValueDatum()},
			Collators: collate.GetBinaryCollatorSlice(1),
		}}
		count, _, err := GetRowCountByIndexRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 99900, int(count))
		ran[0].LowVal[0] = types.NewIntDatum(1000)
		ran[0].HighVal[0] = types.NewIntDatum(2000)
		count, _, err = GetRowCountByIndexRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 2500, int(count))
		ran[0].LowVal[0] = types.NewIntDatum(1001)
		ran[0].HighVal[0] = types.NewIntDatum(1999)
		count, _, err = GetRowCountByIndexRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 2500, int(count))
		ran[0].LowVal[0] = types.NewIntDatum(1000)
		ran[0].HighVal[0] = types.NewIntDatum(1000)
		count, _, err = GetRowCountByIndexRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 100, int(count))

		tbl.SetIdx(0, &Index{Info: &model.IndexInfo{Columns: []*model.IndexColumn{{Offset: 0}}, Unique: true}})
		ran[0].LowVal[0] = types.NewIntDatum(1000)
		ran[0].HighVal[0] = types.NewIntDatum(1000)
		count, _, err = GetRowCountByIndexRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 1, int(count))

		tbl.SetIdx(0, idx)
		ran[0].LowVal[0] = types.MinNotNullDatum()
		ran[0].HighVal[0] = types.MaxValueDatum()
		count, _, err = GetRowCountByIndexRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 100000, int(count))
		ran[0].LowVal[0] = types.NewIntDatum(1000)
		ran[0].HighVal[0] = types.NewIntDatum(2000)
		count, _, err = GetRowCountByIndexRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 1000, int(count))
		ran[0].LowVal[0] = types.NewIntDatum(1001)
		ran[0].HighVal[0] = types.NewIntDatum(1990)
		count, _, err = GetRowCountByIndexRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 989, int(count))
		ran[0].LowVal[0] = types.NewIntDatum(1000)
		ran[0].HighVal[0] = types.NewIntDatum(1000)
		count, _, err = GetRowCountByIndexRanges(ctx, &tbl.HistColl, 0, ran)
		require.NoError(t, err)
		require.Equal(t, 1, int(count))
	}
}

func encodeKey(key types.Datum) types.Datum {
	sc := stmtctx.NewStmtCtxWithTimeZone(time.Local)
	buf, _ := codec.EncodeKey(sc.TimeZone(), nil, key)
	return types.NewBytesDatum(buf)
}

func checkRepeats(t *testing.T, hg *Histogram) {
	for _, bkt := range hg.Buckets {
		require.Greater(t, bkt.Repeat, int64(0))
	}
}

func buildIndex(sctx sessionctx.Context, numBuckets, id int64, records sqlexec.RecordSet) (int64, *Histogram, *CMSketch, error) {
	b := NewSortedBuilder(sctx.GetSessionVars().StmtCtx, numBuckets, id, types.NewFieldType(mysql.TypeBlob), Version1)
	cms := NewCMSketch(8, 2048)
	ctx := context.Background()
	req := records.NewChunk(nil)
	it := chunk.NewIterator4Chunk(req)
	for {
		err := records.Next(ctx, req)
		if err != nil {
			return 0, nil, nil, errors.Trace(err)
		}
		if req.NumRows() == 0 {
			break
		}
		for row := it.Begin(); row != it.End(); row = it.Next() {
			datums := RowToDatums(row, records.Fields())
			buf, err := codec.EncodeKey(sctx.GetSessionVars().StmtCtx.TimeZone(), nil, datums...)
			if err != nil {
				return 0, nil, nil, errors.Trace(err)
			}
			data := types.NewBytesDatum(buf)
			err = b.Iterate(data)
			if err != nil {
				return 0, nil, nil, errors.Trace(err)
			}
			cms.InsertBytes(buf)
		}
	}
	return b.Count, b.Hist(), cms, nil
}

func SubTestBuild() func(*testing.T) {
	return func(t *testing.T) {
		s := createTestStatisticsSamples(t)
		bucketCount := int64(256)
		topNCount := 100
		ctx := mock.NewContext()
		sc := ctx.GetSessionVars().StmtCtx
		sketch, _, err := buildFMSketch(sc, s.rc.(*recordSet).data, 1000)
		require.NoError(t, err)

		collector := &SampleCollector{
			Count:     int64(s.count),
			NullCount: 0,
			Samples:   s.samples,
			FMSketch:  sketch,
		}
		col, err := BuildColumn(ctx, bucketCount, 2, collector, types.NewFieldType(mysql.TypeLonglong))
		require.NoError(t, err)
		checkRepeats(t, col)
		col.PreCalculateScalar()
		require.Equal(t, 226, col.Len())
		count, _ := col.EqualRowCount(nil, types.NewIntDatum(1000), false)
		require.Equal(t, 0, int(count))
		count = col.LessRowCount(nil, types.NewIntDatum(1000))
		require.Equal(t, 10000, int(count))
		count = col.LessRowCount(nil, types.NewIntDatum(2000))
		require.Equal(t, 19999, int(count))
		count = col.GreaterRowCount(types.NewIntDatum(2000))
		require.Equal(t, 80000, int(count))
		count = col.LessRowCount(nil, types.NewIntDatum(200000000))
		require.Equal(t, 100000, int(count))
		count = col.GreaterRowCount(types.NewIntDatum(200000000))
		require.Equal(t, 0.0, count)
		count, _ = col.EqualRowCount(nil, types.NewIntDatum(200000000), false)
		require.Equal(t, 0.0, count)
		count = col.BetweenRowCount(ctx, types.NewIntDatum(3000), types.NewIntDatum(3500))
		require.Equal(t, 4994, int(count))
		count = col.LessRowCount(nil, types.NewIntDatum(1))
		require.Equal(t, 5, int(count))

		colv2, topnv2, err := BuildHistAndTopN(ctx, int(bucketCount), topNCount, 2, collector, types.NewFieldType(mysql.TypeLonglong), true, nil, false)
		require.NoError(t, err)
		require.NotNil(t, topnv2.TopN)
		// The most common one's occurrence is 9990, the second most common one's occurrence is 30.
		// The ndv of the histogram is 73344, the total count of it is 90010. 90010/73344 vs 30, it's not a bad estimate.
		expectedTopNCount := []uint64{9990}
		require.Equal(t, len(expectedTopNCount), len(topnv2.TopN))
		for i, meta := range topnv2.TopN {
			require.Equal(t, expectedTopNCount[i], meta.Count)
		}
		require.Equal(t, 251, colv2.Len())
		count = colv2.LessRowCount(nil, types.NewIntDatum(1000))
		require.Equal(t, 328, int(count))
		count = colv2.LessRowCount(nil, types.NewIntDatum(2000))
		require.Equal(t, 10007, int(count))
		count = colv2.GreaterRowCount(types.NewIntDatum(2000))
		require.Equal(t, 80001, int(count))
		count = colv2.LessRowCount(nil, types.NewIntDatum(200000000))
		require.Equal(t, 90010, int(count))
		count = colv2.GreaterRowCount(types.NewIntDatum(200000000))
		require.Equal(t, 0.0, count)
		count = colv2.BetweenRowCount(ctx, types.NewIntDatum(3000), types.NewIntDatum(3500))
		require.Equal(t, 5001, int(count))
		count = colv2.LessRowCount(nil, types.NewIntDatum(1))
		require.Equal(t, 0, int(count))

		builder := SampleBuilder{
			Sc:              mock.NewContext().GetSessionVars().StmtCtx,
			RecordSet:       s.pk,
			ColLen:          1,
			MaxSampleSize:   1000,
			MaxFMSketchSize: 1000,
			Collators:       make([]collate.Collator, 1),
			ColsFieldType:   []*types.FieldType{types.NewFieldType(mysql.TypeLonglong)},
		}
		require.NoError(t, s.pk.Close())
		collectors, _, err := builder.CollectColumnStats()
		require.NoError(t, err)
		require.Equal(t, 1, len(collectors))
		col, err = BuildColumn(mock.NewContext(), 256, 2, collectors[0], types.NewFieldType(mysql.TypeLonglong))
		require.NoError(t, err)
		checkRepeats(t, col)
		require.Equal(t, 250, col.Len())

		tblCount, col, _, err := buildIndex(ctx, bucketCount, 1, s.rc)
		require.NoError(t, err)
		checkRepeats(t, col)
		col.PreCalculateScalar()
		require.Equal(t, 100000, int(tblCount))
		count, _ = col.EqualRowCount(nil, encodeKey(types.NewIntDatum(10000)), false)
		require.Equal(t, 1, int(count))
		count = col.LessRowCount(nil, encodeKey(types.NewIntDatum(20000)))
		require.Equal(t, 19999, int(count))
		count = col.BetweenRowCount(ctx, encodeKey(types.NewIntDatum(30000)), encodeKey(types.NewIntDatum(35000)))
		require.Equal(t, 4999, int(count))
		count = col.BetweenRowCount(ctx, encodeKey(types.MinNotNullDatum()), encodeKey(types.NewIntDatum(0)))
		require.Equal(t, 0, int(count))
		count = col.LessRowCount(nil, encodeKey(types.NewIntDatum(0)))
		require.Equal(t, 0, int(count))

		s.pk.(*recordSet).cursor = 0
		tblCount, col, err = buildPK(ctx, bucketCount, 4, s.pk)
		require.NoError(t, err)
		checkRepeats(t, col)
		col.PreCalculateScalar()
		require.Equal(t, 100000, int(tblCount))
		count, _ = col.EqualRowCount(nil, types.NewIntDatum(10000), false)
		require.Equal(t, 1, int(count))
		count = col.LessRowCount(nil, types.NewIntDatum(20000))
		require.Equal(t, 20000, int(count))
		count = col.BetweenRowCount(ctx, types.NewIntDatum(30000), types.NewIntDatum(35000))
		require.Equal(t, 5000, int(count))
		count = col.GreaterRowCount(types.NewIntDatum(1001))
		require.Equal(t, 98998, int(count))
		count = col.LessRowCount(nil, types.NewIntDatum(99999))
		require.Equal(t, 99999, int(count))

		datum := types.Datum{}
		datum.SetMysqlJSON(types.BinaryJSON{TypeCode: types.JSONTypeCodeLiteral})
		item := &SampleItem{Value: datum}
		collector = &SampleCollector{
			Count:     1,
			NullCount: 0,
			Samples:   []*SampleItem{item},
			FMSketch:  sketch,
		}
		col, err = BuildColumn(ctx, bucketCount, 2, collector, types.NewFieldType(mysql.TypeJSON))
		require.NoError(t, err)
		require.Equal(t, 1, col.Len())
		require.Equal(t, col.GetUpper(0), col.GetLower(0))
	}
}

func SubTestHistogramProtoConversion() func(*testing.T) {
	return func(t *testing.T) {
		s := createTestStatisticsSamples(t)
		ctx := mock.NewContext()
		require.NoError(t, s.rc.Close())
		tblCount, col, _, err := buildIndex(ctx, 256, 1, s.rc)
		require.NoError(t, err)
		require.Equal(t, 100000, int(tblCount))

		p := HistogramToProto(col)
		h := HistogramFromProto(p)
		require.True(t, HistogramEqual(col, h, true))
	}
}

func TestPruneTopN(t *testing.T) {
	var totalNDV, nullCnt, sampleRows, totalRows int64

	// case 1
	topnIn := []TopNMeta{{[]byte{1}, 100_000}}
	totalNDV = 2
	nullCnt = 0
	sampleRows = 100_010
	totalRows = 500_050
	topnOut := pruneTopNItem(topnIn, totalNDV, nullCnt, sampleRows, totalRows)
	require.Equal(t, topnIn, topnOut)

	// case 2
	topnIn = []TopNMeta{
		{[]byte{1}, 30_000},
		{[]byte{2}, 30_000},
		{[]byte{3}, 20_000},
		{[]byte{4}, 20_000},
	}
	totalNDV = 5
	nullCnt = 0
	sampleRows = 100_000
	totalRows = 10_000_000
	topnOut = pruneTopNItem(topnIn, totalNDV, nullCnt, sampleRows, totalRows)
	require.Equal(t, topnIn, topnOut)

	// case 3
	topnIn = nil
	for i := range 10 {
		topnIn = append(topnIn, TopNMeta{[]byte{byte(i)}, 10_000})
	}
	totalNDV = 100
	nullCnt = 0
	sampleRows = 100_000
	totalRows = 10_000_000
	topnOut = pruneTopNItem(topnIn, totalNDV, nullCnt, sampleRows, totalRows)
	require.Equal(t, topnIn, topnOut)

	// case 4 - test TopN pruning for small table
	topnIn = []TopNMeta{
		{[]byte{1}, 3_000},
		{[]byte{2}, 3_000},
	}
	totalNDV = 4002
	nullCnt = 0
	sampleRows = 10_000
	totalRows = 10_000
	topnOut = pruneTopNItem(topnIn, totalNDV, nullCnt, sampleRows, totalRows)
	require.Equal(t, topnIn, topnOut)

	// case 5 - test pruning of value=1
	topnIn = nil
	for i := range 10 {
		topnIn = append(topnIn, TopNMeta{[]byte{byte(i)}, 90})
	}
	topnPruned := topnIn
	for i := 90; i < 150; i++ {
		topnIn = append(topnIn, TopNMeta{[]byte{byte(i)}, 1})
	}
	totalNDV = 150
	nullCnt = 0
	sampleRows = 1500
	totalRows = 1500
	topnOut = pruneTopNItem(topnIn, totalNDV, nullCnt, sampleRows, totalRows)
	require.Equal(t, topnPruned, topnOut)
}
