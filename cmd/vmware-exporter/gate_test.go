package main

import (
	"sync"
	"testing"
)

func TestScrapeGate(t *testing.T) {
	t.Run("limit zero means unlimited", func(t *testing.T) {
		g := &scrapeGate{}
		// 0 表示不限制：连续占 100 个也应全部成功。
		for i := 0; i < 100; i++ {
			if !g.tryAcquire(0) {
				t.Fatalf("acquire %d rejected under an unlimited gate", i)
			}
		}
		if got := g.current(); got != 100 {
			t.Fatalf("current = %d, want 100", got)
		}
	})

	t.Run("rejects over the limit then admits after release", func(t *testing.T) {
		g := &scrapeGate{}
		const limit = 2

		if !g.tryAcquire(limit) {
			t.Fatal("first acquire must succeed")
		}
		if !g.tryAcquire(limit) {
			t.Fatal("second acquire must succeed")
		}
		// 满员：第三个必须被拒绝，这是 503 路径的依据。
		if g.tryAcquire(limit) {
			t.Fatal("third acquire must be rejected at the limit")
		}
		if got := g.current(); got != limit {
			t.Fatalf("current = %d, want %d (rejected acquire must not count)", got, limit)
		}

		g.release()
		// 反向护栏：只"拒绝"的实现（常量 false）会让这里也失败；真正释放后
		// 必须能再进。
		if !g.tryAcquire(limit) {
			t.Fatal("acquire after release must succeed")
		}
	})

	t.Run("negative limit is unlimited", func(t *testing.T) {
		g := &scrapeGate{}
		if !g.tryAcquire(-1) {
			t.Fatal("negative limit should be treated as unlimited")
		}
	})

	// 并发下 active 绝不能超过 limit。这是计数器正确性（mu 保护）的回归，
	// 与 503 行为分开断言。
	t.Run("never exceeds the limit under concurrency", func(t *testing.T) {
		g := &scrapeGate{}
		const limit = 4

		var wg sync.WaitGroup
		var mu sync.Mutex
		peak := 0
		granted := 0

		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if g.tryAcquire(limit) {
					mu.Lock()
					granted++
					if g.current() > peak {
						peak = g.current()
					}
					mu.Unlock()
					g.release()
				}
			}()
		}
		wg.Wait()

		if peak > limit {
			t.Fatalf("peak in-flight %d exceeded limit %d", peak, limit)
		}
		if granted == 0 {
			t.Fatal("no acquire succeeded; the gate is stuck closed")
		}
		if g.current() != 0 {
			t.Fatalf("current = %d after all releases, want 0 (unbalanced release)", g.current())
		}
	})
}
