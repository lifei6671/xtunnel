// Package safego 统一启动由项目组件拥有的 goroutine，并阻止未恢复的 panic
// 越过 goroutine 边界导致整个进程退出。
package safego

import (
	"errors"
	"runtime/debug"
)

// ErrPanic 表示 goroutine 因 panic 非正常退出。
var ErrPanic = errors.New("goroutine panicked")

// PanicError 保存 panic 发生时的调用栈，但不会暴露可能包含敏感信息的 panic 原值。
type PanicError struct {
	stack []byte
}

// Error 实现 error。
func (*PanicError) Error() string {
	return ErrPanic.Error()
}

// Unwrap 允许调用方使用 errors.Is 识别 goroutine panic。
func (*PanicError) Unwrap() error {
	return ErrPanic
}

// Stack 返回 panic 发生时调用栈的副本，避免调用方修改内部记录。
func (err *PanicError) Stack() []byte {
	if err == nil {
		return nil
	}
	return append([]byte(nil), err.stack...)
}

// Go 启动 function。function 发生 panic 时，Go 会恢复 panic 并调用 onPanic，
// 由 goroutine 的 owner 将错误送入既有失败路径并触发取消或关闭。onDone 无论
// function 正常返回还是 panic 都只执行一次；nil 表示无需额外完成通知。
//
// onPanic 自身的 panic 也会被恢复，确保错误处理代码不会再次击穿 goroutine
// 边界。function panic 时 onPanic 总是在 onDone 之前执行，避免 Wait 提前返回而
// 漏掉错误；onDone 自身的 panic 也会被恢复并交给 onPanic。调用方仍应让两个
// 回调保持简单、非阻塞，并完整执行 owner 的退出协议。
func Go(onPanic func(error), onDone func(), function func()) {
	if onPanic == nil || function == nil {
		panic("safego: nil callback")
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				report(onPanic, &PanicError{stack: debug.Stack()})
				_ = finish(onDone)
				return
			}
			if err := finish(onDone); err != nil {
				report(onPanic, err)
			}
		}()
		function()
	}()
}

func finish(onDone func()) (panicErr error) {
	if onDone == nil {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			panicErr = &PanicError{stack: debug.Stack()}
		}
	}()
	onDone()
	return nil
}

func report(onPanic func(error), err error) {
	defer func() {
		_ = recover()
	}()
	onPanic(err)
}
