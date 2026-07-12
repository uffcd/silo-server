package recommendations

import (
	"context"
	"strconv"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Silo-Server/silo-server/internal/cache"
)

const (
	// similarItemsTTL bounds how stale a Similar rail can get after embedding
	// or co-watch updates; those land via background jobs, so TTL expiry is the
	// only invalidation this cache needs.
	similarItemsTTL = 15 * time.Minute

	// similarItemsBuildTimeout re-bounds a cache rebuild once it is detached
	// from the leader request's cancellation.
	similarItemsBuildTimeout = 30 * time.Second
)

// similarCacheEntry wraps the result slice so the TTLCache value stays
// comparable, and so empty results are distinguishable from cache misses.
type similarCacheEntry struct {
	items []ScoredItem
}

// similarItemsCache memoizes SimilarItems responses. The computation is
// user-independent (keyed only by item and limit, with no access filter), so a
// single process-local entry can serve every caller — including the Jellyfin
// compat surface, which routes through the same Engine method. Residency is
// bounded by distinct items actually requested per TTL window (each entry is
// at most ~50 small structs), not by catalog size; the TTLCache sweeper evicts
// expired entries, so no size cap is needed.
type similarItemsCache struct {
	cache   *cache.TTLCache[*similarCacheEntry]
	group   singleflight.Group
	ttl     time.Duration
	compute func(ctx context.Context, itemID string, limit int) ([]ScoredItem, error)
}

func newSimilarItemsCache(compute func(ctx context.Context, itemID string, limit int) ([]ScoredItem, error)) *similarItemsCache {
	return &similarItemsCache{
		cache:   cache.NewTTLCache[*similarCacheEntry](),
		ttl:     similarItemsTTL,
		compute: compute,
	}
}

// Close stops the cache's background expiry sweeper (tests only; the engine's
// cache lives for the whole process).
func (c *similarItemsCache) Close() {
	if c != nil && c.cache != nil {
		c.cache.Close()
	}
}

func similarItemsCacheKey(itemID string, limit int) string {
	// The limit is part of the result shape (MMR re-ranks per limit), so it
	// belongs in the key. Handlers clamp it to a small range, which keeps key
	// cardinality bounded.
	return itemID + ":" + strconv.Itoa(limit)
}

// Get returns the cached result for (itemID, limit), computing it at most once
// per TTL window across concurrent callers. Empty results are cached too — an
// item without an embedding would otherwise recompute on every request. Errors
// are never cached.
func (c *similarItemsCache) Get(ctx context.Context, itemID string, limit int) ([]ScoredItem, error) {
	key := similarItemsCacheKey(itemID, limit)
	if entry, ok := c.cache.Get(key); ok {
		return copyScoredItems(entry.items), nil
	}

	value, err, _ := c.group.Do(key, func() (any, error) {
		// A concurrent flight may have installed the entry between the outer
		// read and acquiring this flight; reuse it rather than recomputing.
		if entry, ok := c.cache.Get(key); ok {
			return entry, nil
		}
		// Run the computation detached from the leader's request cancellation:
		// singleflight shares this one build across every collapsed waiter, so
		// the leader's client disconnecting must not fail all the other
		// requests riding on the flight. WithoutCancel keeps the leader's
		// context values (tracing, logging) while dropping its cancellation;
		// the timeout re-bounds the detached work.
		computeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), similarItemsBuildTimeout)
		defer cancel()
		items, err := c.compute(computeCtx, itemID, limit)
		if err != nil {
			return nil, err
		}
		entry := &similarCacheEntry{items: items}
		c.cache.Set(key, entry, c.ttl)
		return entry, nil
	})
	if err != nil {
		return nil, err
	}
	return copyScoredItems(value.(*similarCacheEntry).items), nil
}

// copyScoredItems returns a shallow copy so callers cannot mutate the cached
// slice. ScoredItem is a plain value struct, so element sharing is safe.
func copyScoredItems(items []ScoredItem) []ScoredItem {
	if items == nil {
		return nil
	}
	return append([]ScoredItem(nil), items...)
}
