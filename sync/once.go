// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
)

// Once is an object that will perform exactly one action.
// Once 是一个只执行一次动作的对象
//
// A Once must not be copied after first use.
// Once 在第一次使用后不能被复制
//
// In the terminology of the Go memory model,
// the return from f “synchronizes before”
// the return from any call of once.Do(f).
// 在 Go 内存模型的术语中，函数f 的返回在任何对 once.Do(f) 的调用返回之前维持同步顺序
type Once struct {
	// done indicates whether the action has been performed.
	// It is first in the struct because it is used in the hot path.
	// The hot path is inlined at every call site.
	// Placing done first allows more compact instructions on some architectures (amd64/386),
	// and fewer instructions (to calculate offset) on other architectures.
	// done 表示动作是否已经执行过
	// 它在结构体中是第一个，因为它在热路径中使用, 热路径在每个调用站点都是内联的
	// 将done放在第一位可以在某些架构（amd64/386）上使用更紧凑的指令，而在其他架构上可以使用更少的指令（计算偏移量）
	done uint32
	m    Mutex
}

// Do calls the function f if and only if Do is being called for the
// first time for this instance of Once. In other words, given
//
// Do 仅在Once实例第一次调用时调用函数f
//
//	var once Once
//
// if once.Do(f) is called multiple times, only the first call will invoke f,
// even if f has a different value in each invocation. A new instance of
// Once is required for each function to execute.
//
// 如果多次调用once.Do(f)，只有第一次调用会调用f，即使每次调用f的值都不同，也是如此
//
// Do is intended for initialization that must be run exactly once. Since f
// is niladic, it may be necessary to use a function literal to capture the
// arguments to a function to be invoked by Do:
//
// Do 用于必须仅运行一次的初始化。 由于f是无参数的，因此可能需要使用函数文字来捕获要由Do调用的函数的参数(更常见的 闭包)：
//
//	config.once.Do(func() { config.init(filename) })
//
// Because no call to Do returns until the one call to f returns, if f causes
// Do to be called, it will deadlock.
// 因为只有当f返回时，Do才会返回，所以如果f再次调用Do，它将导致死锁
//
// If f panics, Do considers it to have returned; future calls of Do return
// without calling f.
// 如果f发生恐慌，Do将其视为已返回; 未来的Do调用返回而不调用f
func (o *Once) Do(f func()) {
	// Note: Here is an incorrect implementation of Do:
	//
	//	if atomic.CompareAndSwapUint32(&o.done, 0, 1) {
	//		f()
	//	}
	//
	// Do guarantees that when it returns, f has finished.
	// This implementation would not implement that guarantee:
	// given two simultaneous calls, the winner of the cas would
	// call f, and the second would return immediately, without
	// waiting for the first's call to f to complete.
	// This is why the slow path falls back to a mutex, and why
	// the atomic.StoreUint32 must be delayed until after f returns.
	// 注意：这是一个不正确的Do实现：
	// 如果原子比较并交换uint32(&o.done, 0, 1) { 执行f() }
	// Do需要保证在返回时，f已经完成。但是这个实现不会实现刚才的保证：
	// 给定两个并发调用，cas的赢家将调用f，而第二个将立即返回，而不等待第一个对f的调用完成。
	// 这就是为什么慢路径回退到互斥锁的原因，以及为什么必须延迟atomic.StoreUint32直到f返回之后。

	// 在o.done没有执行完成之前，结果都是0，进入if执行f()
	// 在doSlow中，会加锁，然后再次判断o.done是否为0, 如果为0，则执行f
	if atomic.LoadUint32(&o.done) == 0 {
		// Outlined slow-path to allow inlining of the fast-path.
		o.doSlow(f)
	}
}

func (o *Once) doSlow(f func()) {
	o.m.Lock()
	defer o.m.Unlock()
	// 防止在等待第一个拿到锁的goroutine执行f过程中的子goroutine被唤醒执行再次执行f,做了二次校验
	if o.done == 0 {
		defer atomic.StoreUint32(&o.done, 1)
		f()
	}
}
