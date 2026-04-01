// Package cache provides a trigram-based similarity LRU cache for deduplicating tool results.
package cache

import (
	"container/list"
	"sort"
	"strings"
	"sync"
	"time"
)

const similarityThreshold = 0.7

type cacheEntry struct {
	key       string
	query     string
	trigrams  []uint32
	results   string
	createdAt time.Time
}

// SearchCache provides lightweight caching with trigram-based semantic similarity.
// LRU eviction is O(1) via container/list. Similarity scan uses RLock to allow
// concurrent reads; write lock is only acquired for LRU promotion and eviction.
type SearchCache struct {
	mu         sync.RWMutex
	items      map[string]*list.Element // key → list element
	lru        *list.List               // front = most recent
	maxEntries int
	ttl        time.Duration
}

// NewSearchCache creates a new cache with the given capacity and TTL per entry.
func NewSearchCache(maxEntries int, ttl time.Duration) *SearchCache {
	return &SearchCache{
		items:      make(map[string]*list.Element),
		lru:        list.New(),
		maxEntries: maxEntries,
		ttl:        ttl,
	}
}

// Get checks if a similar query exists in cache.
// Phase 1: read-only scan under RLock.
// Phase 2: LRU promotion under WLock (re-validates key is still present).
func (sc *SearchCache) Get(query string) (string, bool) {
	normalized := normalizeQuery(query)
	if normalized == "" {
		return "", false
	}

	sc.mu.RLock()

	if elem, ok := sc.items[normalized]; ok {
		entry := elem.Value.(*cacheEntry)
		if time.Since(entry.createdAt) < sc.ttl {
			results := entry.results
			sc.mu.RUnlock()
			sc.mu.Lock()
			if _, still := sc.items[normalized]; still {
				sc.lru.MoveToFront(sc.items[normalized])
			}
			sc.mu.Unlock()
			return results, true
		}
	}

	queryTrigrams := buildTrigrams(normalized)
	var bestKey string
	var bestResults string
	var bestSim float64

	for key, elem := range sc.items {
		entry := elem.Value.(*cacheEntry)
		if time.Since(entry.createdAt) >= sc.ttl {
			continue
		}
		sim := jaccardSimilarity(queryTrigrams, entry.trigrams)
		if sim > bestSim {
			bestSim = sim
			bestKey = key
			bestResults = entry.results
		}
	}

	sc.mu.RUnlock()

	if bestSim >= similarityThreshold && bestKey != "" {
		sc.mu.Lock()
		if _, still := sc.items[bestKey]; still {
			sc.lru.MoveToFront(sc.items[bestKey])
		}
		sc.mu.Unlock()
		return bestResults, true
	}

	return "", false
}

// Put adds or updates a query result in the cache.
func (sc *SearchCache) Put(query string, results string) {
	normalized := normalizeQuery(query)
	if normalized == "" {
		return
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()

	sc.evictExpiredLocked()

	if elem, ok := sc.items[normalized]; ok {
		entry := elem.Value.(*cacheEntry)
		entry.results = results
		entry.createdAt = time.Now()
		sc.lru.MoveToFront(elem)
		return
	}

	for sc.lru.Len() >= sc.maxEntries {
		oldest := sc.lru.Back()
		if oldest == nil {
			break
		}
		sc.lru.Remove(oldest)
		delete(sc.items, oldest.Value.(*cacheEntry).key)
	}

	entry := &cacheEntry{
		key:       normalized,
		query:     normalized,
		trigrams:  buildTrigrams(normalized),
		results:   results,
		createdAt: time.Now(),
	}
	elem := sc.lru.PushFront(entry)
	sc.items[normalized] = elem
}

func (sc *SearchCache) evictExpiredLocked() {
	now := time.Now()
	for key, elem := range sc.items {
		if now.Sub(elem.Value.(*cacheEntry).createdAt) >= sc.ttl {
			sc.lru.Remove(elem)
			delete(sc.items, key)
		}
	}
}

func normalizeQuery(q string) string {
	return strings.ToLower(strings.TrimSpace(q))
}

func buildTrigrams(s string) []uint32 {
	runes := []rune(s)
	if len(runes) < 3 {
		return nil
	}
	trigrams := make([]uint32, 0, len(runes)-2)
	for i := 0; i <= len(runes)-3; i++ {
		// Use lower 21 bits of each rune (covers full Unicode BMP+supplementary)
		val := (uint32(runes[i]&0x1FFFFF) << 10) ^ (uint32(runes[i+1]&0x1FFFFF) << 5) ^ uint32(runes[i+2]&0x1FFFFF)
		trigrams = append(trigrams, val)
	}
	sort.Slice(trigrams, func(i, j int) bool { return trigrams[i] < trigrams[j] })
	n := 1
	for i := 1; i < len(trigrams); i++ {
		if trigrams[i] != trigrams[i-1] {
			trigrams[n] = trigrams[i]
			n++
		}
	}
	return trigrams[:n]
}

func jaccardSimilarity(a, b []uint32) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0 // two sub-trigram queries are not meaningfully similar
	}
	i, j, intersection := 0, 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			intersection++
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	union := len(a) + len(b) - intersection
	return float64(intersection) / float64(union)
}
