package recommendations

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSimilarItemsCacheMissThenHit(t *testing.T) {
	var calls atomic.Int64
	c := newSimilarItemsCache(func(_ context.Context, itemID string, _ int) ([]ScoredItem, error) {
		calls.Add(1)
		return []ScoredItem{{MediaItemID: "other-" + itemID, Score: 0.9}}, nil
	})
	defer c.Close()

	ctx := context.Background()
	first, err := c.Get(ctx, "movie-1", 20)
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	second, err := c.Get(ctx, "movie-1", 20)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("compute calls = %d, want 1", got)
	}
	if len(first) != 1 || len(second) != 1 || second[0].MediaItemID != "other-movie-1" {
		t.Fatalf("unexpected results: first=%v second=%v", first, second)
	}
}

func TestSimilarItemsCacheDistinctLimitsAreDistinctEntries(t *testing.T) {
	var calls atomic.Int64
	c := newSimilarItemsCache(func(_ context.Context, _ string, limit int) ([]ScoredItem, error) {
		calls.Add(1)
		items := make([]ScoredItem, limit)
		return items, nil
	})
	defer c.Close()

	ctx := context.Background()
	a, _ := c.Get(ctx, "movie-1", 10)
	b, _ := c.Get(ctx, "movie-1", 20)
	if got := calls.Load(); got != 2 {
		t.Fatalf("compute calls = %d, want 2", got)
	}
	if len(a) != 10 || len(b) != 20 {
		t.Fatalf("lengths = %d, %d; want 10, 20", len(a), len(b))
	}
}

func TestSimilarItemsCacheCollapsesConcurrentGets(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	c := newSimilarItemsCache(func(_ context.Context, _ string, _ int) ([]ScoredItem, error) {
		calls.Add(1)
		<-release
		return []ScoredItem{{MediaItemID: "x"}}, nil
	})
	defer c.Close()

	const workers = 8
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.Get(context.Background(), "movie-1", 20)
		}(i)
	}
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("compute calls = %d, want 1", got)
	}
}

func TestSimilarItemsCacheDoesNotCacheErrors(t *testing.T) {
	var calls atomic.Int64
	c := newSimilarItemsCache(func(_ context.Context, _ string, _ int) ([]ScoredItem, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("transient")
		}
		return []ScoredItem{{MediaItemID: "x"}}, nil
	})
	defer c.Close()

	ctx := context.Background()
	if _, err := c.Get(ctx, "movie-1", 20); err == nil {
		t.Fatal("first Get: expected error")
	}
	items, err := c.Get(ctx, "movie-1", 20)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %v, want 1 entry", items)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("compute calls = %d, want 2", got)
	}
}

func TestSimilarItemsCacheCachesEmptyResults(t *testing.T) {
	var calls atomic.Int64
	c := newSimilarItemsCache(func(_ context.Context, _ string, _ int) ([]ScoredItem, error) {
		calls.Add(1)
		return nil, nil // item without an embedding
	})
	defer c.Close()

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		items, err := c.Get(ctx, "movie-1", 20)
		if err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
		if items != nil {
			t.Fatalf("Get %d: items = %v, want nil", i, items)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("compute calls = %d, want 1", got)
	}
}

func TestSimilarItemsCacheReturnsDefensiveCopies(t *testing.T) {
	c := newSimilarItemsCache(func(_ context.Context, _ string, _ int) ([]ScoredItem, error) {
		return []ScoredItem{{MediaItemID: "original", Score: 1}}, nil
	})
	defer c.Close()

	ctx := context.Background()
	first, err := c.Get(ctx, "movie-1", 20)
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	first[0].MediaItemID = "mutated"

	second, err := c.Get(ctx, "movie-1", 20)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if second[0].MediaItemID != "original" {
		t.Fatalf("cached entry was mutated: %v", second)
	}
}

func TestSimilarItemsCacheInvalidatePrefixDropsAllLimits(t *testing.T) {
	var calls atomic.Int64
	c := newSimilarItemsCache(func(_ context.Context, _ string, _ int) ([]ScoredItem, error) {
		calls.Add(1)
		return []ScoredItem{{MediaItemID: "x"}}, nil
	})
	defer c.Close()

	ctx := context.Background()
	_, _ = c.Get(ctx, "movie-1", 10)
	_, _ = c.Get(ctx, "movie-1", 20)
	c.cache.InvalidatePrefix("movie-1:")
	_, _ = c.Get(ctx, "movie-1", 10)
	if got := calls.Load(); got != 3 {
		t.Fatalf("compute calls = %d, want 3", got)
	}
}
