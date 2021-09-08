package chunk

import (
	"bytes"
	"context"

	"github.com/opentracing/opentracing-go"

	"github.com/grafana/dskit/math"
)

// QueryFilter wraps a callback to ensure the results are filtered correctly;
// useful for the cache and Bigtable backend, which only ever fetches the whole
// row.
func QueryFilter(callback QueryCallback) QueryCallback {
	return func(query IndexQuery, batch ReadBatch) bool {
		return callback(query, &filteringBatch{query, batch})
	}
}

type filteringBatch struct {
	query IndexQuery
	ReadBatch
}

func (f filteringBatch) Iterator() ReadBatchIterator {
	return &filteringBatchIter{
		query:             f.query,
		ReadBatchIterator: f.ReadBatch.Iterator(),
	}
}

type filteringBatchIter struct {
	query IndexQuery
	ReadBatchIterator
}

func (f *filteringBatchIter) Next() bool {
	for f.ReadBatchIterator.Next() {
		rangeValue, value := f.ReadBatchIterator.RangeValue(), f.ReadBatchIterator.Value()

		if len(f.query.RangeValuePrefix) != 0 && !bytes.HasPrefix(rangeValue, f.query.RangeValuePrefix) {
			continue
		}
		if len(f.query.RangeValueStart) != 0 && bytes.Compare(f.query.RangeValueStart, rangeValue) > 0 {
			continue
		}
		if len(f.query.ValueEqual) != 0 && !bytes.Equal(value, f.query.ValueEqual) {
			continue
		}

		return true
	}

	return false
}

// QueryCallback from an IndexQuery.
type QueryCallback func(IndexQuery, ReadBatch) bool

// DoSingleQuery is the interface for indexes that don't support batching yet.
type DoSingleQuery func(context.Context, IndexQuery, QueryCallback) error

// QueryParallelism is the maximum number of subqueries run in
// parallel per higher-level query.
var QueryParallelism = 100

// DoParallelQueries translates between our interface for query batching,
// and indexes that don't yet support batching.
func DoParallelQueries(
	ctx context.Context, doSingleQuery DoSingleQuery, queries []IndexQuery,
	callback QueryCallback,
) error {
	if len(queries) == 1 {
		return doSingleQuery(ctx, queries[0], callback)
	}

	queue := make(chan IndexQuery)
	incomingErrors := make(chan error)
	n := math.Min(len(queries), QueryParallelism)
	// Run n parallel goroutines fetching queries from the queue
	for i := 0; i < n; i++ {
		go func() {
			sp, ctx := opentracing.StartSpanFromContext(ctx, "DoParallelQueries-worker")
			defer sp.Finish()
			for {
				query, ok := <-queue
				if !ok {
					return
				}
				incomingErrors <- doSingleQuery(ctx, query, callback)
			}
		}()
	}
	// Send all the queries into the queue
	go func() {
		for _, query := range queries {
			queue <- query
		}
		close(queue)
	}()

	// Now receive all the results.
	var lastErr error
	for i := 0; i < len(queries); i++ {
		err := <-incomingErrors
		if err != nil {

			lastErr = err
		}
	}
	return lastErr
}
