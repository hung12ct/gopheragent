package cache

import (
	"fmt"
	"testing"
	"time"
)

func TestSearchCache_ExactMatch(t *testing.T) {
	c := NewSearchCache(10, 5*time.Minute)
	c.Put("hello world", "result1")

	val, ok := c.Get("hello world")
	if !ok || val != "result1" {
		t.Fatalf("expected exact match, got ok=%v val=%q", ok, val)
	}
}

func TestSearchCache_SimilarityMatch(t *testing.T) {
	c := NewSearchCache(10, 5*time.Minute)
	c.Put("find top creatives on facebook", "facebook_results")

	val, ok := c.Get("find top creative on facebook")
	if !ok {
		t.Fatal("expected similarity match")
	}
	if val != "facebook_results" {
		t.Fatalf("expected 'facebook_results', got %q", val)
	}
}

func TestSearchCache_NoMatchForDissimilar(t *testing.T) {
	c := NewSearchCache(10, 5*time.Minute)
	c.Put("find top creatives on facebook", "facebook_results")

	_, ok := c.Get("what is the weather today")
	if ok {
		t.Fatal("expected no match for dissimilar query")
	}
}

func TestSearchCache_TTLExpiry(t *testing.T) {
	c := NewSearchCache(10, 50*time.Millisecond)
	c.Put("query1", "result1")

	time.Sleep(100 * time.Millisecond)

	_, ok := c.Get("query1")
	if ok {
		t.Fatal("expected cache miss after TTL expiry")
	}
}

func TestSearchCache_LRUEviction(t *testing.T) {
	c := NewSearchCache(2, 5*time.Minute)
	c.Put("aaa bbb ccc", "r1")
	c.Put("ddd eee fff", "r2")
	c.Put("ggg hhh iii", "r3") // should evict "aaa bbb ccc"

	_, ok := c.Get("aaa bbb ccc")
	if ok {
		t.Fatal("expected LRU eviction of oldest entry")
	}

	val, ok := c.Get("ggg hhh iii")
	if !ok || val != "r3" {
		t.Fatal("expected newest entry to exist")
	}
}

func TestSearchCache_UpdateExisting(t *testing.T) {
	c := NewSearchCache(10, 5*time.Minute)
	c.Put("hello world", "old")
	c.Put("hello world", "new")

	val, ok := c.Get("hello world")
	if !ok || val != "new" {
		t.Fatalf("expected updated value 'new', got %q", val)
	}
}

func TestSearchCache_EmptyQuery(t *testing.T) {
	c := NewSearchCache(10, 5*time.Minute)
	c.Put("", "should_not_store")
	_, ok := c.Get("")
	if ok {
		t.Fatal("empty query should not match")
	}
}

func TestSearchCache_ConcurrentAccess(t *testing.T) {
	c := NewSearchCache(100, 5*time.Minute)
	done := make(chan bool, 20)

	for i := 0; i < 10; i++ {
		go func(n int) {
			c.Put("query "+string(rune('a'+n)), "result")
			done <- true
		}(i)
	}
	for i := 0; i < 10; i++ {
		go func(n int) {
			c.Get("query " + string(rune('a'+n)))
			done <- true
		}(i)
	}
	for i := 0; i < 20; i++ {
		<-done
	}
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

func BenchmarkSearchCache_Put(b *testing.B) {
	c := NewSearchCache(1000, 5*time.Minute)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Put(fmt.Sprintf("find top creatives on platform %d", i), "result")
	}
}

func BenchmarkSearchCache_Get_Hit(b *testing.B) {
	c := NewSearchCache(1000, 5*time.Minute)
	c.Put("find top creatives on facebook", "result")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get("find top creative on facebook") // slight variation → similarity match
	}
}

func BenchmarkSearchCache_Get_Miss(b *testing.B) {
	c := NewSearchCache(1000, 5*time.Minute)
	c.Put("find top creatives on facebook", "result")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get("completely unrelated query about cooking recipes")
	}
}

func BenchmarkSearchCache_ConcurrentReadWrite(b *testing.B) {
	c := NewSearchCache(500, 5*time.Minute)
	for i := 0; i < 100; i++ {
		c.Put(fmt.Sprintf("seed query number %d abc def", i), "val")
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if i%2 == 0 {
				c.Put(fmt.Sprintf("parallel write query %d xyz", i), "result")
			} else {
				c.Get(fmt.Sprintf("seed query number %d abc def", i%100))
			}
			i++
		}
	})
}
