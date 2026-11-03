# nogchashmap

一个**低 GC 压力**的哈希集合（hash set），只保存键、不保存值。

设计思路参考 [github.com/yudeguang17/noGcMap](https://github.com/yudeguang17/noGcMap)：把不利于 GC 的容器拆成
「基础类型索引 + `[]byte` 字节流 + 少量回退 map」，让大集合在 GC 时几乎不被扫描。

```go
package main

import (
	"fmt"

	"github.com/yudeguang17/nogchashmap"
)

func main() {
	s := nogchashmap.New()
	s.SetString("hello")
	s.Set([]byte("world"))

	fmt.Println(s.HasString("hello"), s.Has([]byte("world"))) // true true
	fmt.Println(s.Len())                                      // 2

	s.DeleteString("hello")
	fmt.Println(s.HasString("hello"), s.Len()) // false 1
}
```

## 特性

- **低 GC 压力**：`Has` / `HasString` / 重复 `Set` / `Delete` / `Len` 等热路径实测 **0 次堆分配**（见下节「实测数据」）。
- **无 hash 冲突问题**：索引用 64 位 hash 做键，冲突的键落到该分片的回退 map，查询会自动回落查找。
- **支持增删改**：`Set` 已存在的键是空操作（集合没有值，不存在"改值"），`Delete` 之后不会再读到旧数据。
- **分片并发**：512 个分片各持一把读写锁，读写冲突被分散；实例可被多个 goroutine 并发使用。
- **键长度受限且显式**：键长度必须**小于 65535**（最大 65534），超限**直接 panic**，绝不静默截断。
- **配置属于实例**：两个实例可以用不同配置，互不影响。
- **删除可回收**：删除比例超过阈值时自动把存活键前移、释放不再使用的 chunk。
- **零第三方依赖**：hash 使用标准库 `hash/maphash`，且每个实例一个随机种子。

## 安装

```bash
go get github.com/yudeguang17/nogchashmap
```

要求 **Go 1.24 及以上**（用到了 `hash/maphash` 的 `Bytes` / `String`）。若要兼容更早的 Go 版本，
只需把 `New` 里的 `h.hash` / `h.hashString` 换成其它哈希实现（例如 `github.com/cespare/xxhash` 的 `xxhash.Sum64`），
其余代码不需要改。

## API

| 方法 | 说明 |
| --- | --- |
| `New(config ...Config) *HashSet` | 创建集合；配置非法会 panic |
| `(*HashSet) Set(k []byte)` | 加入键；已存在则无操作 |
| `(*HashSet) SetString(k string)` | 同上，string 版本 |
| `(*HashSet) Has(k []byte) bool` | 判断键是否存在 |
| `(*HashSet) HasString(k string) bool` | 同上，string 版本 |
| `(*HashSet) Delete(k []byte)` | 删除键；不存在则无操作 |
| `(*HashSet) DeleteString(k string)` | 同上，string 版本 |
| `(*HashSet) Len() int` | 键的个数，**精确** |

`Set` 与 `SetString` 写入的键可以互相查到（同一段字节的 hash 相同），跨类型 `Has` / `Delete` 也能正常工作。

## 键长度约束

- 允许：`len(key) <= 65534`（`nogchashmap.MaxKeyLen`）。
- 不允许：`len(key) >= 65535` —— `Set` / `SetString` / `Has` / `HasString` / `Delete` / `DeleteString`
  收到这种键会**立即 panic**：

```text
panic: nogchashmap: 非法键长度 65535：键长度必须小于 65535
```

这样处理的原因：键长度在字节流里用 2 字节（大端 `uint16`）记录，长度达到 65535 以上就会被截断，
一旦截断就会出现"写进去读不回"甚至"读回被截断的错误内容"。与其静默出错，不如按契约直接 panic。

空键（`len(key) == 0`）是合法的。

## 配置

```go
s := nogchashmap.New(nogchashmap.Config{
	NeedDeletePercent:          50, // 1..100，默认 50
	MultiplesOf64KForChunkSize: 1,  // 1..1000（1=64KB，1000=64MB），默认 1
})
```

| 字段 | 取值范围 | 默认 | 说明 |
| --- | --- | --- | --- |
| `NeedDeletePercent` | 1..100 | 50 | 某分片「已删除键数 / 历史键总数」达到该百分比时触发空间回收 |
| `MultiplesOf64KForChunkSize` | 1..1000 | 1 | 单个 chunk 大小 = 该值 × 64KB |

取值非法会直接 panic（例如 `NeedDeletePercent: 101`、`MultiplesOf64KForChunkSize: 1001`），不会静默忽略。
两个字段留 0 表示使用默认值。

## 与 noGcMap 的差异

| 维度 | noGcMap | nogchashmap |
| --- | --- | --- |
| 数据模型 | `map[key]value`，键和值都存 | 只能存键（hash set），没有值 |
| 键长度 | 长度字段 2 字节，超长时会被静默截断（配置 chunkSize > 64KB 时尤其明显） | 键长度 ≥ 65535 直接 panic，不存在截断 |
| Config | 写在**包级全局变量**里，多个实例互相串扰，运行期还会被后来的 `New` 改掉 | 只属于当前实例，实例之间互不影响 |
| 删除语义 | 同一个键可能同时存在于字节流与回退 map，删除后可能仍读到旧值 | 一个键只会存在于一处，删除后不再存在 |
| 哈希 | `github.com/cespare/xxhash` | 标准库 `hash/maphash`（每实例随机 seed，无第三方依赖） |
| 首次内存占用 | 分片第一次写入即分配一个完整 chunk（默认 64KB，512 个分片最多约 32MB） | 首次只分配 4KB 的小 chunk，写满后再分配完整 chunk |
| 配置字段名 | `NeedDeketeKVPairPersent`、`MultiplesOf64KForchunkSize` | `NeedDeletePercent`、`MultiplesOf64KForChunkSize`（修正拼写与大小写） |
| Int 键 | 提供 `NoGcMapInt` | 未提供（见「已知限制」） |

## 实测数据

环境：Windows / go1.27.3 windows/amd64 / AMD Ryzen 7 PRO 7840U。

**热路径堆分配**（`TestNoAllocOnHotPaths`，`testing.AllocsPerRun` 200 次 + `runtime.MemStats` 单次复核）：

| 路径 | 堆分配次数 |
| --- | --- |
| `Has` 命中 / 未命中（`[]byte`） | 0 |
| `HasString` 命中 / 未命中 | 0 |
| `Set` 已有键（`[]byte` / `string`） | 0 |
| `Delete` 未命中 / 命中 | 0 |
| `Len` | 0 |

**插入全新键的分配是摊销的**（`TestAmortizedAllocOnInsert`）：
插入 100000 个全新键共发生 8214 次堆分配，即**每个键约 0.082 次**，主要来自每个分片索引 map 的增长与 chunk 分配。

**基准**（`go test -bench . -benchmem -benchtime 500000x`，键为 4096 个不重复的字符串）：

| 基准 | ns/op | allocs/op |
| --- | --- | --- |
| `BenchmarkSet`（重复加入已有键，即空操作快路径） | 42.2 | 0 |
| `BenchmarkSetString`（同上） | 51.7 | 0 |
| `BenchmarkHas` | 33.1 | 0 |
| `BenchmarkHasString` | 49.8 | 0 |
| `BenchmarkSetThenDelete`（真实插入 + 删除） | 100.7 | 0 |
| `BenchmarkStdMapSet`（对照：`map[string]struct{}`） | 14.4 | 0 |
| `BenchmarkStdMapHas`（对照） | 7.11 | 0 |

说明：`-benchmem` 的 `B/op` 列会把基准运行期间「插入时为分片一次性分配的 chunk」按 `b.N` 摊销，
所以 `B/op` 不为 0 而 `allocs/op` 为 0；判断某条路径是否真的零分配，以上面的 `AllocsPerRun` 结论为准。
与原始 `map[string]struct{}` 相比，本包的单次操作更慢（分片加锁 + 字节流 + 索引三层），
换取的是**GC 时不扫描大容器**。

## 设计与实现要点

1. **分片**：512 个 `bucket`，每个 bucket 有独立的读写锁、索引、chunk 列表和回退 map。
2. **索引**：`map[uint64]uint`，键是键的 64 位 hash，值是键在字节流中的**绝对偏移**；
   偏移解析方式固定为 `chunk 下标 = 偏移 / chunkSize`、`块内偏移 = 偏移 % chunkSize`。索引 map 的键值都是基础类型，GC 不扫描。
3. **字节流**：键按 `[2 字节 keyLen][key]` 顺序追加在 `[]byte` chunk 中；键长度 < 65535 保证一个键
   （最多 2 + 65534 = 65536 字节）一定能放进任何一个完整 chunk。
4. **回退 map**：只在 hash 冲突（64 位 hash 撞上、但键不同）时启用，懒创建，存放 `map[string]struct{}`。
5. **空间回收**：分片「已删除键数 / 历史键总数」超过 `NeedDeletePercent`（且历史键数 ≥ 256）时，
   把该分片存活的键按原顺序搬到 chunk 前面，并释放尾部不再使用的 chunk；判断存活的方式是
   「键的 hash 在索引中的偏移是否正好等于它在字节流中的当前位置」。
6. **首次小 chunk**：分片第一次写入只分配 4KB，避免小集合也占满 512 × 64KB。

## 并发安全

- `HashSet` 可被多个 goroutine 并发使用：`Has` 走分片读锁，`Set` / `Delete` 走分片写锁，`Len` 依次加各分片读锁。
- 后创建的实例不会影响已有实例的配置或数据。
- 注意：并发**调用构造函数**没有特殊保护，但构造函数只写自己实例的字段，不共享状态。

## 测试与基准

```bash
go test ./...                                  # 11 个测试
go test -run '^$' -bench . -benchmem ./...     # 基准（-run '^$' 表示不跑测试）
go test -run TestNoAllocOnHotPaths -v ./...    # 复核热路径零分配
```

测试覆盖：与标准库 `map[string]bool` 对账的增删查（含空键、含 `0x00` 的二进制键、最长键）、
键长边界与 panic、`[]byte`/`string` 互查、强制 hash 冲突下的回退 map 语义与
「删除后不再存在」回归、实例配置隔离与非法配置 panic、最长键正好填满 chunk、
空间回收后 `Len`/`Has` 仍正确、8 goroutine 并发混合读写删对账、热路径零分配、插入分配摊销、首次小 chunk。

## 已知限制

- 只支持字节序列键（`[]byte` / `string`），没有像 noGcMap 那样的 `int` 键版本。
- 不提供遍历 / 迭代器，也没有 `Clear`；如需这些能力可以在此实现上自行扩展。
- 空间回收是**在分片写锁内一次性完成**的：如果 `MultiplesOf64KForChunkSize` 配置得很大
  （例如 1000，即 64MB），一次回收会把该分片阻塞较长时间，建议按实际数据量选择合适的大小。
- 内存下限为「被触碰的分片数 × 4KB」，上限随数据量增长；这是「零拷贝字节流」方案的固有代价。
  另外，每个分片在首个小 chunk 之后新增的 chunk 一律是完整大小（`chunkSize`，默认 64KB），
  所以「数据量不大但键分散在全部 512 个分片」时，最多会有约 512 × 64KB 的额外占用；
  如果对这种场景敏感，可考虑调小分片数或按需裁剪 `bucketsNum`。
- 空间回收复用一份最多一个 chunk 大小的临时缓冲，不释放（换取后续回收不再分配）。
- 未提供持久化、过期淘汰、容量上限等能力。