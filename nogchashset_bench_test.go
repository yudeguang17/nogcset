package nogchashmap

import (
	"strconv"
	"testing"
)

const benchKeyCount = 4096

// 预先构造好键，避免基准自身产生额外分配。
var (
	benchKB [][]byte
	benchKS []string
)

func init() {
	benchKB = make([][]byte, benchKeyCount)
	benchKS = make([]string, benchKeyCount)
	for i := 0; i < benchKeyCount; i++ {
		benchKS[i] = "bench-key-" + strconv.Itoa(i)
		benchKB[i] = []byte(benchKS[i])
	}
}

func BenchmarkSet(b *testing.B) {
	h := New()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Set(benchKB[i&(benchKeyCount-1)])
	}
}

func BenchmarkSetString(b *testing.B) {
	h := New()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.SetString(benchKS[i&(benchKeyCount-1)])
	}
}

func BenchmarkHas(b *testing.B) {
	h := New()
	for _, k := range benchKB {
		h.Set(k)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !h.Contain(benchKB[i&(benchKeyCount-1)]) {
			b.Fatal("unexpected miss")
		}
	}
}

func BenchmarkHasString(b *testing.B) {
	h := New()
	for _, k := range benchKS {
		h.SetString(k)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !h.ContainString(benchKS[i&(benchKeyCount-1)]) {
			b.Fatal("unexpected miss")
		}
	}
}

func BenchmarkSetThenDelete(b *testing.B) {
	h := New()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		k := benchKB[i&(benchKeyCount-1)]
		h.Set(k)
		h.Delete(k)
	}
}

// 与标准库 map[string]struct{} 对照，说明基准口径。
func BenchmarkStdMapSet(b *testing.B) {
	m := make(map[string]struct{}, benchKeyCount)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m[benchKS[i&(benchKeyCount-1)]] = struct{}{}
	}
}

func BenchmarkStdMapHas(b *testing.B) {
	m := make(map[string]struct{}, benchKeyCount)
	for _, k := range benchKS {
		m[k] = struct{}{}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := m[benchKS[i&(benchKeyCount-1)]]; !ok {
			b.Fatal("unexpected miss")
		}
	}
}
