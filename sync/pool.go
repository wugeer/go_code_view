// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"runtime"
	"sync/atomic"
	"unsafe"

	"internal/race"
)

// A Pool is a set of temporary objects that may be individually saved and
// retrieved.
//
// Pool是一个临时对象的集合，可以单独保存和检索。
//
// Any item stored in the Pool may be removed automatically at any time without
// notification. If the Pool holds the only reference when this happens, the
// item might be deallocated.
//
// 在任何时候(STW)，Pool中存储的任何项都可能被自动删除，而不会通知。
// 如果Pool在此时保持唯一的引用，则可能会取消分配该项。
//
// A Pool is safe for use by multiple goroutines simultaneously.
//
// Pool可同时由多个goroutine使用。
//
// Pool's purpose is to cache allocated but unused items for later reuse,
// relieving pressure on the garbage collector. That is, it makes it easy to
// build efficient, thread-safe free lists. However, it is not suitable for all
// free lists.
//
// Pool的目的是缓存已分配但未使用的项以供以后重用，从而减轻垃圾收集器的压力。
// 也就是说，它可以轻松地构建高效的线程安全的空闲列表。但是，它不适用于所有空闲列表。
//
// An appropriate use of a Pool is to manage a group of temporary items
// silently shared among and potentially reused by concurrent independent
// clients of a package. Pool provides a way to amortize allocation overhead
// across many clients.
//
// Pool的适当用法是管理一组临时项，这些临时项在包的并发独立客户端之间静默共享.
// Pool 提供了一种方法，可以在许多客户端之间分摊分配开销。
//
// An example of good use of a Pool is in the fmt package, which maintains a
// dynamically-sized store of temporary output buffers. The store scales under
// load (when many goroutines are actively printing) and shrinks when
// quiescent.
//
// Pool的一个很好的用例是fmt包，它维护一个动态大小的临时输出缓冲区存储。
// 负载下，存储器会扩展(当许多goroutine正在主动打印时)，
//
// On the other hand, a free list maintained as part of a short-lived object is
// not a suitable use for a Pool, since the overhead does not amortize well in
// that scenario. It is more efficient to have such objects implement their own
// free list.
//
// 另一方面，作为短期对象的一部分维护的空闲列表不适合用于Pool，因为在这种情况下，开销不会很好地分摊。
// 对于这样的对象，实现自己的空闲列表更有效。
//
// A Pool must not be copied after first use.
// Pool 在第一次使用后不能被复制。
//
// In the terminology of the Go memory model, a call to Put(x) “synchronizes before”
// a call to Get returning that same value x.
// Similarly, a call to New returning x “synchronizes before”
// a call to Get returning that same value x.
// 在go内存模型的术语中，调用Put(x)在调用Get返回相同值x同步之前
type Pool struct {
	noCopy noCopy

	local     unsafe.Pointer // local fixed-size per-P pool, actual type is [P]poolLocal
	localSize uintptr        // size of the local array

	victim     unsafe.Pointer // local from previous cycle
	victimSize uintptr        // size of victims array

	// New optionally specifies a function to generate
	// a value when Get would otherwise return nil.
	// It may not be changed concurrently with calls to Get.
	// New 可选地指定一个函数来在Get时生成一个值，否则Get会返回nil。
	// 它不能与对Get的调用并发更改。
	New func() any
}

// Local per-P Pool appendix.
// Local per-P Pool 附录。
type poolLocalInternal struct {
	private any // Can be used only by the respective P.  只能由相应的P使用。
	// 本地P可以pushHead/popHead; 任何P都可以popTail
	shared poolChain // Local P can pushHead/popHead; any P can popTail.
}

type poolLocal struct {
	poolLocalInternal

	// Prevents false sharing on widespread platforms with
	// 128 mod (cache line size) = 0 .
	// 防止在广泛使用的平台上出现128 mod (cache line size) = 0的错误共享。
	pad [128 - unsafe.Sizeof(poolLocalInternal{})%128]byte
}

// from runtime
func fastrandn(n uint32) uint32

var poolRaceHash [128]uint64

// poolRaceAddr returns an address to use as the synchronization point
// for race detector logic. We don't use the actual pointer stored in x
// directly, for fear of conflicting with other synchronization on that address.
// Instead, we hash the pointer to get an index into poolRaceHash.
// See discussion on golang.org/cl/31589.
// poolRaceAddr 返回一个地址，用作race detector逻辑的同步点。
// 不直接使用x中存储的实际指针，出于担心在那个地址上有别的同步操作带来的冲突
// 相反，我们对指针进行哈希，以获得poolRaceHash中的索引。
// 请参见golang.org/cl/31589上的讨论。
func poolRaceAddr(x any) unsafe.Pointer {
	// 获取x的第二个指针，因为第一个指针是poolLocal.private; 第二个指针是poolLocal.shared
	ptr := uintptr((*[2]unsafe.Pointer)(unsafe.Pointer(&x))[1])
	// 解释下下面这行代码
	// 1. ptr是一个指针，指向poolLocal.shared
	// 2. uint32(ptr) 将ptr转换为uint32类型
	// 3. uint64(uint32(ptr)) 将uint32(ptr)转换为uint64类型
	// 4. uint64(uint32(ptr)) * 0x85ebca6b 将uint64(uint32(ptr))乘以0x85ebca6b; 0x85ebca6b代表一个随机数
	// 5. 将第4步的结果右移16位，再转为uint32类型
	h := uint32((uint64(uint32(ptr)) * 0x85ebca6b) >> 16)
	return unsafe.Pointer(&poolRaceHash[h%uint32(len(poolRaceHash))])
}

// Put adds x to the pool.
// Put 将x添加到pool中
func (p *Pool) Put(x any) {
	// 如果x为nil，忽略
	if x == nil {
		return
	}
	// 如果开启竞态
	if race.Enabled {
		// 解释下下面的这行代码
		// 1. fastrandn(4) 生成一个0-3的随机数
		// 2. 如果随机数为0，忽略
		if fastrandn(4) == 0 {
			// 向下取整时随机丢弃x
			// Randomly drop x on floor.
			return
		}
		// 否则，获取x的地址，作为race detector逻辑的同步点
		race.ReleaseMerge(poolRaceAddr(x))
		// 关闭race
		race.Disable()
	}
	// 获取当前P的poolLocal
	l, _ := p.pin()
	// 如果当前P的poolLocal.private为nil，将x赋值给poolLocal.private
	// 否则，将x插入到poolLocal.shared的头部
	if l.private == nil {
		l.private = x
	} else {
		l.shared.pushHead(x)
	}
	// 解除pin
	runtime_procUnpin()
	// 如果enable了race，重新开启race
	if race.Enabled {
		race.Enable()
	}
}

// Get selects an arbitrary item from the Pool, removes it from the
// Pool, and returns it to the caller.
// Get may choose to ignore the pool and treat it as empty.
// Callers should not assume any relation between values passed to Put and
// the values returned by Get.
//
// Get 从Pool中选择任意项，将其从Pool中删除，并将其返回给调用者。
// Get 可能选择忽略pool并将其视为空。调用者不应该假设Put传递的值与Get返回的值之间存在任何关系。
//
// If Get would otherwise return nil and p.New is non-nil, Get returns
// the result of calling p.New.
// Get如果在前面的逻辑中取不到非nil的值，如果p.New 是nil, 则返回nil，p.New非nil则Get返回调用p.New的结果。
func (p *Pool) Get() any {
	if race.Enabled {
		// 如果开启了竞态，先临时关闭race
		race.Disable()
	}
	// 获取当前P的poolLocal和pid
	l, pid := p.pin()
	// 取出当前P的poolLocal.private赋值给x，并置为nil
	x := l.private
	l.private = nil
	// 如果x为nil，尝试从local.shared中取出一个值
	if x == nil {
		// Try to pop the head of the local shard. We prefer
		// the head over the tail for temporal locality of
		// reuse.
		// 尝试弹出本地共享的头部。我们更喜欢头部而不是尾部，出于可以重用的时间局部性考虑。
		x, _ = l.shared.popHead()
		if x == nil {
			// 如果local.shared为空，尝试从其他P的local.shared中偷一个值
			x = p.getSlow(pid)
		}
	}
	// 解除pin
	runtime_procUnpin()
	if race.Enabled {
		// 如果开启了race，重新开启race
		race.Enable()
		if x != nil {
			// 如果x不为nil，向race detector逻辑的同步点发送x的地址
			race.Acquire(poolRaceAddr(x))
		}
	}
	if x == nil && p.New != nil {
		// 上面的逻辑获取的x为nil，且p.New非nil，则调用p.New获取一个值
		x = p.New()
	}
	return x
}

func (p *Pool) getSlow(pid int) any {
	// See the comment in pin regarding ordering of the loads.
	// 请参阅pin中有关加载顺序的注释。
	// 获取localSize和local
	size := runtime_LoadAcquintptr(&p.localSize) // load-acquire
	locals := p.local                            // load-consume
	// Try to steal one element from other procs.
	// 尝试从其他P的shared.local元素中偷取第一个不为nil的元素
	for i := 0; i < int(size); i++ {
		l := indexLocal(locals, (pid+i+1)%int(size))
		if x, _ := l.shared.popTail(); x != nil {
			return x
		}
	}

	// Try the victim cache. We do this after attempting to steal
	// from all primary caches because we want objects in the
	// victim cache to age out if at all possible.
	// 尝试从victim cache中获取一个值，这里之所以放在尝试从所有primary caches中偷取值之后，
	// 是因为我们希望victim cache中的对象尽可能地过期。
	size = atomic.LoadUintptr(&p.victimSize)
	if uintptr(pid) >= size {
		// 如果pid大于等于victimSize，直接返回nil；
		// 因为victimSize是一个递增的值，所以如果pid大于等于victimSize，说明victim cache为空
		return nil
	}
	locals = p.victim
	// 从victim cache中获取pid对应的P的poolLocal.private
	l := indexLocal(locals, pid)
	if x := l.private; x != nil {
		l.private = nil
		return x
	}
	// 从victim cache中获取pid对应的P的poolLocal.shared
	for i := 0; i < int(size); i++ {
		l := indexLocal(locals, (pid+i)%int(size))
		if x, _ := l.shared.popTail(); x != nil {
			return x
		}
	}

	// Mark the victim cache as empty for future gets don't bother
	// with it.
	// 将victim cache标记为空，以便将来的获取不会再次使用它。
	atomic.StoreUintptr(&p.victimSize, 0)

	return nil
}

// pin pins the current goroutine to P, disables preemption and
// returns poolLocal pool for the P and the P's id.
// Caller must call runtime_procUnpin() when done with the pool.
// pin 将当前goroutine固定到P，禁用抢占并返回P的poolLocal池和P的id。
// 调用者在完成pool操作后必须调用runtime_procUnpin()
func (p *Pool) pin() (*poolLocal, int) {
	// 调用runtime_procPin() 将当前goroutine固定到P
	pid := runtime_procPin()
	// In pinSlow we store to local and then to localSize, here we load in opposite order.
	// Since we've disabled preemption, GC cannot happen in between.
	// Thus here we must observe local at least as large localSize.
	// We can observe a newer/larger local, it is fine (we must observe its zero-initialized-ness).
	// 在pinSlow中，我们先存储local数据，然后再存储数据到localSize，这里我们以相反的顺序加载。
	// 由于我们已经禁用了抢占，因此GC不能在中间发生。
	// 因此，我们必须至少观察到local大于等于localSize。
	// 我们可以观察到一个更新/更大的local，这是可以的(我们必须观察到它的零初始化)。
	s := runtime_LoadAcquintptr(&p.localSize) // load-acquire
	l := p.local                              // load-consume
	if uintptr(pid) < s {
		return indexLocal(l, pid), pid
	}
	return p.pinSlow()
}

func (p *Pool) pinSlow() (*poolLocal, int) {
	// Retry under the mutex.
	// Can not lock the mutex while pinned.
	// 在互斥锁下重试。在pinned状态下不能锁定互斥锁。
	// 所以要先接触pinned状态，再加锁
	runtime_procUnpin()
	allPoolsMu.Lock()
	defer allPoolsMu.Unlock()
	// 重新获取pid
	pid := runtime_procPin()
	// poolCleanup won't be called while we are pinned.
	s := p.localSize
	l := p.local
	// 只要pid小于s，就说明p.localSize和p.local是有效的，可以直接返回
	// 否则，需要重新初始化p.local
	if uintptr(pid) < s {
		return indexLocal(l, pid), pid
	}
	if p.local == nil {
		// Initialize the pool.
		// 这里视为p是新建的，需要加到allPools中
		allPools = append(allPools, p)
	}
	// If GOMAXPROCS changes between GCs, we re-allocate the array and lose the old one.
	// 如果GOMAXPROCS在GC之间发生变化，我们将重新分配数组并丢弃旧数组。
	size := runtime.GOMAXPROCS(0)
	// 有多少个P，就有多少个poolLocal
	local := make([]poolLocal, size)
	// p.local重新初始化
	atomic.StorePointer(&p.local, unsafe.Pointer(&local[0])) // store-release
	runtime_StoreReluintptr(&p.localSize, uintptr(size))     // store-release
	return &local[pid], pid
}

func poolCleanup() {
	// This function is called with the world stopped, at the beginning of a garbage collection.
	// It must not allocate and probably should not call any runtime functions.
	// 这个函数在STW时被调用，在垃圾回收的开始。它不能分配内存，也可能不应该调用任何runtime函数。

	// Because the world is stopped, no pool user can be in a
	// pinned section (in effect, this has all Ps pinned).
	// 因为STW了，所以没有pool的使用者可以在pinned状态下, 也就是说，所有的P都是pinned状态。

	// Drop victim caches from all pools.
	// 从所有的pool中丢弃victim cache
	for _, p := range oldPools {
		p.victim = nil
		p.victimSize = 0
	}

	// Move primary cache to victim cache.
	// 将primary cache移动到victim cache
	for _, p := range allPools {
		p.victim = p.local
		p.victimSize = p.localSize
		p.local = nil
		p.localSize = 0
	}

	// The pools with non-empty primary caches now have non-empty
	// victim caches and no pools have primary caches.
	// 现在，具有非空primary cache的pool具有非空victim cache，没有pool具有primary cache。
	// 池子降级
	oldPools, allPools = allPools, nil
}

var (
	allPoolsMu Mutex

	// allPools is the set of pools that have non-empty primary
	// caches. Protected by either 1) allPoolsMu and pinning or 2)
	// STW.
	// allPools是具有非空primary cache的pool集合。由
	// 1) allPoolsMu和pinning
	// 2) STW保护。
	allPools []*Pool

	// oldPools is the set of pools that may have non-empty victim
	// caches. Protected by STW.
	// oldPools是可能具有非空victim cache的pool集合。由STW保护。
	oldPools []*Pool
)

func init() {
	runtime_registerPoolCleanup(poolCleanup)
}

func indexLocal(l unsafe.Pointer, i int) *poolLocal {
	// 根据i获取l的第i个poolLocal地址
	lp := unsafe.Pointer(uintptr(l) + uintptr(i)*unsafe.Sizeof(poolLocal{}))
	return (*poolLocal)(lp)
}

// Implemented in runtime.
func runtime_registerPoolCleanup(cleanup func())
func runtime_procPin() int
func runtime_procUnpin()

// The below are implemented in runtime/internal/atomic and the
// compiler also knows to intrinsify the symbol we linkname into this
// package.

//go:linkname runtime_LoadAcquintptr runtime/internal/atomic.LoadAcquintptr
func runtime_LoadAcquintptr(ptr *uintptr) uintptr

//go:linkname runtime_StoreReluintptr runtime/internal/atomic.StoreReluintptr
func runtime_StoreReluintptr(ptr *uintptr, val uintptr) uintptr
