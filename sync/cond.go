// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
	"unsafe"
)

// Cond implements a condition variable, a rendezvous point
// for goroutines waiting for or announcing the occurrence
// of an event.
//
// Cond 实现了条件变量，一个 goroutine 等待或者宣布事件发生的集合点。
//
// Each Cond has an associated Locker L (often a *Mutex or *RWMutex),
// which must be held when changing the condition and
// when calling the Wait method.
//
// 每个 Cond 都有一个关联的 Locker L（通常是 *Mutex 或者 *RWMutex），
// 在改变条件和调用 Wait 方法时必须持有该锁。
//
// A Cond must not be copied after first use.
// 一个 Cond 在第一次使用后不能被复制。
//
// In the terminology of the Go memory model, Cond arranges that
// a call to Broadcast or Signal “synchronizes before” any Wait call
// that it unblocks.
//
// 在 Go 内存模型的术语中，Cond保证调用Broadcast或者Signal的synchronizes before任何它解除阻塞的 Wait 调用完成前。
//
// For many simple use cases, users will be better off using channels than a
// Cond (Broadcast corresponds to closing a channel, and Signal corresponds to
// sending on a channel).
// 对于许多简单的用例，用户最好使用通道而不是 Cond（Broadcast 对应于关闭通道，Signal 对应于在通道上发送）。
//
// For more on replacements for sync.Cond, see [Roberto Clapis's series on
// advanced concurrency patterns], as well as [Bryan Mills's talk on concurrency
// patterns].
// 有关替换 sync.Cond 的更多信息，请参见[Roberto Clapis 的高级并发模式系列]，以及[Bryan Mills 的并发模式演讲]。
//
// [Roberto Clapis's series on advanced concurrency patterns]: https://blogtitle.github.io/categories/concurrency/
// [Bryan Mills's talk on concurrency patterns]: https://drive.google.com/file/d/1nPdvhB0PutEJzdCq5ms6UI58dp50fcAN/view
type Cond struct {
	noCopy noCopy

	// L is held while observing or changing the condition
	// L 在观察或者改变条件时被持有
	L Locker

	notify  notifyList
	checker copyChecker
}

// NewCond returns a new Cond with Locker l.
func NewCond(l Locker) *Cond {
	return &Cond{L: l}
}

// Wait atomically unlocks c.L and suspends execution
// of the calling goroutine. After later resuming execution,
// Wait locks c.L before returning. Unlike in other systems,
// Wait cannot return unless awoken by Broadcast or Signal.
//
// Wait 原子性地解锁c.L并挂起调用的 goroutine 的执行。稍后恢复执行后，Wait 在返回之前锁定 c.L.
// 与其他系统不同，Wait不能返回，除非被Broadcast或Signal唤醒。
//
// Because c.L is not locked while Wait is waiting, the caller
// typically cannot assume that the condition is true when
// Wait returns. Instead, the caller should Wait in a loop:
// 因为 Wait 在等待时 c.L 没有被锁定，所以调用者通常不能假设当 Wait 返回时条件为真。
// 相反，调用者应该在循环中等待：唤醒只是多了一次检查条件的机会而已
//
//	c.L.Lock()
//	for !condition() {
//	    c.Wait()
//	}
//	... make use of condition ...
//	c.L.Unlock()
func (c *Cond) Wait() {
	c.checker.check()
	// 将当前 goroutine 添加到 notifyList 中的等待队列中
	t := runtime_notifyListAdd(&c.notify)
	// 释放锁
	c.L.Unlock()
	// wait等待被唤醒
	runtime_notifyListWait(&c.notify, t)
	// 重新获取锁
	c.L.Lock()
}

// Signal wakes one goroutine waiting on c, if there is any.
// Signal 唤醒一个正在等待 c 的 goroutine，如果有的话。
//
// It is allowed but not required for the caller to hold c.L
// during the call.
// 允许但不要求调用者在调用期间持有 c.L。
//
// Signal() does not affect goroutine scheduling priority; if other goroutines
// are attempting to lock c.L, they may be awoken before a "waiting" goroutine.
// Signal() 不会影响 goroutine 的调度优先级；
// 如果其他 goroutine 正在尝试锁定 c.L，他们在等待goroutine之前可能会被唤醒。
func (c *Cond) Signal() {
	c.checker.check()
	runtime_notifyListNotifyOne(&c.notify)
}

// Broadcast wakes all goroutines waiting on c.
// Broadcast 唤醒所有正在等待 c 的 goroutine。
//
// It is allowed but not required for the caller to hold c.L
// during the call.
// 允许但不要求调用者在调用期间持有 c.L。
func (c *Cond) Broadcast() {
	c.checker.check()
	runtime_notifyListNotifyAll(&c.notify)
}

// copyChecker holds back pointer to itself to detect object copying.
// copyChecker 持有指向自身的后指针，以检测对象复制。
type copyChecker uintptr

func (c *copyChecker) check() {
	// 检查是否被复制
	// 通过原子操作将 c 转换为 uintptr，然后与 unsafe.Pointer(c) 比较
	// 如果不相等，说明被复制了
	// CAS 操作失败，说明被复制了
	// 再次比较c的地址，如果不相等，说明被复制了
	if uintptr(*c) != uintptr(unsafe.Pointer(c)) &&
		!atomic.CompareAndSwapUintptr((*uintptr)(c), 0, uintptr(unsafe.Pointer(c))) &&
		uintptr(*c) != uintptr(unsafe.Pointer(c)) {
		panic("sync.Cond is copied")
	}
}

// noCopy may be added to structs which must not be copied
// after the first use.
//
// See https://golang.org/issues/8005#issuecomment-190753527
// for details.
//
// Note that it must not be embedded, due to the Lock and Unlock methods.
// 如果想要在go vet中检查是否变量发生过拷贝，那么嵌入这个noCopy类型即可
type noCopy struct{}

// Lock is a no-op used by -copylocks checker from `go vet`.
func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}
