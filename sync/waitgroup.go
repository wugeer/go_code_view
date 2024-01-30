// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
	"unsafe"

	"internal/race"
)

// A WaitGroup waits for a collection of goroutines to finish.
// The main goroutine calls Add to set the number of
// goroutines to wait for. Then each of the goroutines
// runs and calls Done when finished. At the same time,
// Wait can be used to block until all goroutines have finished.
//
// waitgroup用于等待一组goroutine完成。主goroutine调用Add来设置等待的goroutine数量。
//
// A WaitGroup must not be copied after first use.
// waitgroup在第一次使用后不能被复制。
//
// In the terminology of the Go memory model, a call to Done
// “synchronizes before” the return of any Wait call that it unblocks.
// 在Go内存模型的术语中，Done的调用发生在任何Wait调用的返回之前同步。
type WaitGroup struct {
	noCopy noCopy

	// 高32位是计数器即没有done的个数，低32位是等待计数即调用wait的个数。
	state atomic.Uint64 // high 32 bits are counter, low 32 bits are waiter count.
	sema  uint32
}

// Add adds delta, which may be negative, to the WaitGroup counter.
// If the counter becomes zero, all goroutines blocked on Wait are released.
// If the counter goes negative, Add panics.
//
// Add方法将delta(可以是负数)值加到WaitGroup的计数器上。如果计数器变为0，所有在Wait上等待的goroutine都会被释放。
// 如果计数器变为负数，Add会panic。
//
// Note that calls with a positive delta that occur when the counter is zero
// must happen before a Wait. Calls with a negative delta, or calls with a
// positive delta that start when the counter is greater than zero, may happen
// at any time.
// 值得注意的是，当计数器为0时，调用Add方法的正数delta值必须在Wait方法之前执行。
// 当计数器大于0时，无论调用负数delta值还是正数delta值，都可以在任何时候执行。
// Typically this means the calls to Add should execute before the statement
// creating the goroutine or other event to be waited for.
// 通常情况下，这意味着调用Add的方法应该在创建goroutine或者其他等待的事件之前执行。
// If a WaitGroup is reused to wait for several independent sets of events,
// new Add calls must happen after all previous Wait calls have returned.
// 如果WaitGroup被重用来等待几个独立的事件集，那么新的Add调用必须在所有之前的Wait调用返回之后执行。
// See the WaitGroup example.
func (wg *WaitGroup) Add(delta int) {
	if race.Enabled {
		if delta < 0 {
			// Synchronize decrements with Wait.
			// 与Wait方法同步减少。
			race.ReleaseMerge(unsafe.Pointer(wg))
		}
		race.Disable()
		defer race.Enable()
	}
	// delta 右移32位，即delta的高32位，即计数器的值。
	state := wg.state.Add(uint64(delta) << 32)
	// 新的计数器值v
	v := int32(state >> 32)
	// wait个数w
	w := uint32(state)
	if race.Enabled && delta > 0 && v == int32(delta) {
		// The first increment must be synchronized with Wait.
		// Need to model this as a read, because there can be
		// several concurrent wg.counter transitions from 0.
		// 第一次增量必须与Wait同步。
		// 需要将其建模为读取，因为可以从0开始有几个并发的wg.counter转换。
		// 第一次add调用
		race.Read(unsafe.Pointer(&wg.sema))
	}
	// 计数器不能为负
	if v < 0 {
		panic("sync: negative WaitGroup counter")
	}
	// 如果wait个数不为0且之前的计数器为0，相当于原先计数器为0的情况下，wait个数又增加了，视为异常，抛出panic。
	if w != 0 && delta > 0 && v == int32(delta) {
		panic("sync: WaitGroup misuse: Add called concurrently with Wait")
	}
	if v > 0 || w == 0 {
		return
	}
	// This goroutine has set counter to 0 when waiters > 0.
	// Now there can't be concurrent mutations of state:
	// - Adds must not happen concurrently with Wait,
	// - Wait does not increment waiters if it sees counter == 0.
	// Still do a cheap sanity check to detect WaitGroup misuse.
	// 当waiters > 0时，这个goroutine将计数器设置为0。
	// 现在不能有并发的状态变化：
	// - Add不能与Wait并发发生
	// - 如果Wait看到计数器为0，Wait不会增加waiters。
	// 仍然做一个廉价的健全性检查来检测WaitGroup的错误使用。
	if wg.state.Load() != state {
		panic("sync: WaitGroup misuse: Add called concurrently with Wait")
	}
	// Reset waiters count to 0.
	wg.state.Store(0)
	for ; w != 0; w-- {
		runtime_Semrelease(&wg.sema, false, 0)
	}
}

// Done decrements the WaitGroup counter by one.
func (wg *WaitGroup) Done() {
	wg.Add(-1)
}

// Wait blocks until the WaitGroup counter is zero.
func (wg *WaitGroup) Wait() {
	if race.Enabled {
		race.Disable()
	}
	for {
		state := wg.state.Load()
		// 取出计数器值和wait个数
		v := int32(state >> 32)
		w := uint32(state)
		if v == 0 {
			// Counter is 0, no need to wait.
			if race.Enabled {
				race.Enable()
				race.Acquire(unsafe.Pointer(wg))
			}
			return
		}
		// Increment waiters count.
		if wg.state.CompareAndSwap(state, state+1) {
			// 第一次wait调用
			if race.Enabled && w == 0 {
				// Wait must be synchronized with the first Add.
				// Need to model this is as a write to race with the read in Add.
				// As a consequence, can do the write only for the first waiter,
				// otherwise concurrent Waits will race with each other.
				// Wait必须与第一次Add同步。
				// 需要将其建模为与Add中的读取相冲突的写入。
				// 因此，只能为第一个waiter做写入，否则并发的wait会相互竞争。
				race.Write(unsafe.Pointer(&wg.sema))
			}
			// 等待被唤醒
			runtime_Semacquire(&wg.sema)
			// 被唤醒后，校验state是否为0, 如果不为0，说明是异常情况，抛出panic。
			if wg.state.Load() != 0 {
				panic("sync: WaitGroup is reused before previous Wait has returned")
			}
			if race.Enabled {
				race.Enable()
				race.Acquire(unsafe.Pointer(wg))
			}
			return
		}
	}
}
