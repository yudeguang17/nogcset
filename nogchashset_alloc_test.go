package nogchashmap

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// allocSink 用于防止编译器把显式分配优化掉（仅测试使用）。
var allocSink string

// TestNoAllocOnHotPaths 精确测量各热路径的堆分配次数。
// 本包的定位是「低 GC 压力」，因此所有查询、命中、未命中路径都必须是零堆分配。
func TestNoAllocOnHotPaths(t *testing.T) {
	h := New()
	k := []byte("some-key-for-alloc-test")
	ks := "some-key-for-alloc-test"
	missing := []byte("no-such-key-at-all")
	missingStr := "no-such-key-at-all"
	h.Add(k)
	h.AddString("another-key-that-exists")

	// 先验证测量方法本身有效：显式做一次 string([]byte) 转换必须被测出分配
	if got := testing.AllocsPerRun(100, func() { allocSink = string(k) }); got == 0 {
		t.Fatal("测量方法失效：显式分配没有被测出来")
	}

	cases := []struct {
		name string
		fn   func()
	}{
		{"Contain 命中([]byte)", func() { h.Contain(k) }},
		{"Contain 未命中([]byte)", func() { h.Contain(missing) }},
		{"ContainString 命中", func() { h.ContainString(ks) }},
		{"ContainString 未命中", func() { h.ContainString(missingStr) }},
		{"Add 已有键([]byte)", func() { h.Add(k) }},
		{"Add 已有键(string)", func() { h.AddString("another-key-that-exists") }},
		{"Remove 未命中([]byte)", func() { h.Remove(missing) }},
		{"Remove 未命中(string)", func() { h.RemoveString(missingStr) }},
		{"Len", func() { _ = h.Len() }},
	}
	for _, c := range cases {
		if got := testing.AllocsPerRun(200, c.fn); got != 0 {
			t.Errorf("%s：期望 0 次堆分配，实际 %v 次", c.name, got)
		}
	}

	// Remove 命中是「一次性」操作，AllocsPerRun 的预热会先把键删掉，
	// 因此这里用 MemStats 直接测量一次真实删除的堆分配次数。
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	h.Remove(k)
	runtime.ReadMemStats(&after)
	if m := after.Mallocs - before.Mallocs; m != 0 {
		t.Errorf("Remove 命中([]byte)：期望 0 次堆分配，实际 %d 次", m)
	}
	h.AddString(ks)
	runtime.GC()
	runtime.ReadMemStats(&before)
	h.RemoveString(ks)
	runtime.ReadMemStats(&after)
	if m := after.Mallocs - before.Mallocs; m != 0 {
		t.Errorf("Remove 命中(string)：期望 0 次堆分配，实际 %d 次", m)
	}
	if h.Contain(k) || h.ContainString(ks) {
		t.Fatal("删除后仍存在")
	}
}

// TestAmortizedAllocOnInsert 说明插入全新键时的分配是摊销的：
// 每写满一个 chunk 才分配一次 chunk，索引 map 增长也是摊销的。
func TestAmortizedAllocOnInsert(t *testing.T) {
	const n = 100000
	keys := make([][]byte, n)
	for i := 0; i < n; i++ {
		keys[i] = []byte(fmt.Sprintf("insert-key-%d-%s", i, strings.Repeat("q", i%24)))
	}
	h := New()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for _, k := range keys {
		h.Add(k)
	}
	runtime.ReadMemStats(&after)
	if h.Len() != n {
		t.Fatalf("Len() = %d，期望 %d", h.Len(), n)
	}
	allocs := after.Mallocs - before.Mallocs
	t.Logf("插入 %d 个全新键共发生 %d 次堆分配（每个键约 %.5f 次）", n, allocs, float64(allocs)/float64(n))
	if allocs > n/10 {
		t.Errorf("插入新键的堆分配次数过多：%d 次 / %d 个键", allocs, n)
	}
}

// TestSmallFirstChunk 分片第一次写入只分配 4KB 的小 chunk，
// 避免「一个分片只放一两个小键也占满 64KB」；写满后新增的 chunk 才是完整大小。
func TestSmallFirstChunk(t *testing.T) {
	h := New()
	// 让每个键的 hash 都落在分片 1 上，但彼此不同（不冲突）
	forceHash(h,
		func(k []byte) uint64 {
			n, err := strconv.Atoi(string(k))
			if err != nil {
				n = 0
			}
			return uint64(n)*bucketsNum + 1
		},
		func(s string) uint64 {
			n, err := strconv.Atoi(s)
			if err != nil {
				n = 0
			}
			return uint64(n)*bucketsNum + 1
		},
	)
	b := &h.buckets[1]
	h.Add([]byte("0"))
	if len(b.chunks) != 1 {
		t.Fatalf("首次写入后 chunk 个数 = %d，期望 1", len(b.chunks))
	}
	if got := cap(b.chunks[0]); got != firstChunkSize {
		t.Fatalf("首个 chunk 容量 = %d，期望 %d", got, firstChunkSize)
	}
	// 继续写入，直到写满 4KB 并分配第二个 chunk
	last := 0
	for i := 1; i < 100000; i++ {
		h.Add([]byte(strconv.Itoa(i)))
		last = i
		if len(b.chunks) > 1 {
			break
		}
	}
	if len(b.chunks) < 2 {
		t.Fatalf("写入 %d 个键后仍未分配第二个 chunk", last)
	}
	if got := cap(b.chunks[1]); got != 64*1024 {
		t.Fatalf("第二个 chunk 容量 = %d，期望 %d", got, 64*1024)
	}
	if h.Len() != last+1 {
		t.Fatalf("Len() = %d，期望 %d", h.Len(), last+1)
	}
	for i := 0; i <= last; i++ {
		if !h.ContainString(strconv.Itoa(i)) {
			t.Fatalf("键 %d 读不回", i)
		}
	}
}
