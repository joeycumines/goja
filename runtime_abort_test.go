package goja

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetFuncNameHandlesNilWrappedObject(t *testing.T) {
	rt := New()
	var nilObject *Object
	wrapper := rt.newDestructKeyedSource(nilObject)
	stack := []Value{wrapper, _undefined}
	if got := getFuncName(stack, 1); got != "" {
		t.Fatalf("name = %q, want empty", got)
	}
}

func TestCaptureStackDoesNotInvokeDestructuredProxyTrap(t *testing.T) {
	rt := New()
	var traps atomic.Int32
	// Install before destructuring so the proxy handler can call it.
	if err := rt.Set("descriptorTrap", func() { traps.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if err := rt.Set("wrappedTarget", func() {
		if frames := rt.CaptureCallStack(0, nil); len(frames) == 0 {
			t.Error("CaptureCallStack returned no frames")
		}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RunString(`
		let escaped;
		const handler = {
			get(target, key, receiver) {
				escaped = receiver;
				return Reflect.get(target, key, receiver);
			},
			getOwnPropertyDescriptor(target, key) {
				descriptorTrap();
				return Reflect.getOwnPropertyDescriptor(target, key);
			}
		};
		const { arbitrary, ...rest } = new Proxy(wrappedTarget, handler);
		delete handler.get;
	`); err != nil {
		t.Fatal(err)
	}
	if traps.Load() == 0 {
		t.Fatal("destructuring did not exercise the descriptor trap")
	}
	traps.Store(0)
	if _, err := rt.RunString(`escaped()`); err != nil {
		t.Fatal(err)
	}
	if calls := traps.Load(); calls != 0 {
		t.Fatalf("stack capture with a destructuring wrapper invoked %d proxy descriptor traps", calls)
	}
}

func TestCaptureStackKeepsDestructuredFunctionName(t *testing.T) {
	rt := New()
	fn, err := rt.RunString(`(function nativeNamed() {})`)
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := fn.(*Object)
	if !ok {
		t.Fatal("expected a function object")
	}
	wrapped := rt.newDestructKeyedSource(obj)
	stack := []Value{wrapped, _undefined}
	if got := getFuncName(stack, 1); got != "nativeNamed" {
		t.Fatalf("wrapped function name = %q, want nativeNamed", got)
	}
}

func TestAbortCurrentCallCanReturnNativeError(t *testing.T) {
	rt := New()
	cause := errors.New("terminal cause")
	failure := errors.New("native operation failure")
	if err := rt.Set("native", func() error {
		rt.Abort(cause)
		return failure
	}); err != nil {
		t.Fatal(err)
	}
	_, err := rt.RunString(`native()`)
	var exception *Exception
	if !errors.As(err, &exception) || !errors.Is(err, failure) || errors.Is(err, cause) {
		t.Fatalf("current call error = %v, want native error %v", err, failure)
	}
	_, err = rt.RunString(`1 + 1`)
	requireAbortCause(t, err, cause)
}

func TestAbortNativeCleanupThroughDestructuredProxy(t *testing.T) {
	rt := New()
	cause := errors.New("terminal cancellation")
	var descriptorTraps atomic.Int32
	if err := rt.Set("descriptorTrap", func() { descriptorTraps.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if err := rt.Set("native", func() {
		rt.Abort(cause)
		_ = rt.NewGoError(cause)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RunString(`
		let escaped;
		const handler = {
			get(target, key, receiver) {
				escaped = receiver;
				return Reflect.get(target, key, receiver);
			},
			getOwnPropertyDescriptor(target, key) {
				descriptorTrap();
				return Reflect.getOwnPropertyDescriptor(target, key);
			}
		};
		const { arbitrary, ...rest } = new Proxy(native, handler);
		delete handler.get;
	`); err != nil {
		t.Fatal(err)
	}
	trapsAfterDestructuring := descriptorTraps.Load()
	done := make(chan error, 1)
	go func() {
		_, err := rt.RunString(`escaped()`)
		done <- err
	}()
	select {
	case err := <-done:
		requireAbortCause(t, err, cause)
	case <-time.After(5 * time.Second):
		t.Fatal("native cleanup after abort hung in recursive stack capture")
	}
	if calls := descriptorTraps.Load(); calls != trapsAfterDestructuring {
		t.Fatalf("native cleanup after abort invoked %d additional proxy descriptor traps", calls-trapsAfterDestructuring)
	}
}

func requireAbortCause(t *testing.T, err, cause error) {
	t.Helper()
	var interrupted *InterruptedError
	if !errors.As(err, &interrupted) || interrupted.Value() != cause || !errors.Is(err, cause) {
		t.Fatalf("execution error = %v, want InterruptedError carrying %v", err, cause)
	}
}

func TestAbortPersistsAcrossExecutionAndClear(t *testing.T) {
	rt := New()
	value, err := rt.RunString(`(function () { throw new Error("must not run"); })`)
	if err != nil {
		t.Fatal(err)
	}
	call, ok := AssertFunction(value)
	if !ok {
		t.Fatal("expected a JavaScript function")
	}
	cause := errors.New("host stopped runtime")
	rt.Abort(cause)
	for range 3 {
		rt.ClearInterrupt()
		rt.Interrupt(errors.New("later ordinary interrupt"))
		rt.Abort(errors.New("later terminal abort"))
		_, err = rt.RunString(`throw new Error("must not run")`)
		requireAbortCause(t, err, cause)
		_, err = call(Undefined())
		requireAbortCause(t, err, cause)
	}
}

func TestAbortConcurrentInterruptAndClear(t *testing.T) {
	rt := New()
	cause := errors.New("first abort")
	rt.Abort(cause)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				rt.ClearInterrupt()
				rt.Interrupt(errors.New("ordinary interrupt"))
				rt.Abort(errors.New("later abort"))
			}
		})
	}
	for range 100 {
		_, err := rt.RunString(`1 + 1`)
		requireAbortCause(t, err, cause)
	}
	wg.Wait()
	_, err := rt.RunString(`1 + 1`)
	requireAbortCause(t, err, cause)
}

func TestAbortStopsActiveExecutionAndQueuedJobs(t *testing.T) {
	rt := New()
	ready := make(chan struct{})
	var queuedCalls atomic.Int32
	if err := rt.Set("ready", func() { close(ready) }); err != nil {
		t.Fatal(err)
	}
	if err := rt.Set("queued", func() { queuedCalls.Add(1) }); err != nil {
		t.Fatal(err)
	}
	var jobs []func()
	rt.SetPromiseJobEnqueuer(func(job func()) { jobs = append(jobs, job) })
	done := make(chan error, 1)
	go func() {
		_, err := rt.RunString(`
			Promise.resolve().then(function () { queued(); for (;;) {} });
			ready(); for (;;) {}
		`)
		done <- err
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		rt.Abort(errors.New("test startup timeout"))
		t.Fatal("script did not enter its active loop")
	}
	cause := errors.New("host abort")
	rt.Abort(cause)
	select {
	case err := <-done:
		requireAbortCause(t, err, cause)
	case <-time.After(5 * time.Second):
		t.Fatal("Abort did not interrupt active JavaScript")
	}
	if len(jobs) != 1 {
		t.Fatalf("captured jobs = %d, want one native Promise reaction", len(jobs))
	}
	rt.ClearInterrupt()
	for _, job := range jobs {
		requireAbortCause(t, rt.RunPromiseJob(job), cause)
	}
	if calls := queuedCalls.Load(); calls != 0 {
		t.Fatalf("queued JavaScript ran after abort: %d calls", calls)
	}
}

func TestAbortNestedExecutionDoesNotReadFunctionName(t *testing.T) {
	rt := New()
	ready := make(chan struct{})
	release := make(chan struct{})
	var nameReads atomic.Int32
	var innerErr error
	if err := rt.Set("native", func() {
		close(ready)
		<-release // Native Go work is cooperative, even after Abort.
		_, innerErr = rt.RunString(`for (;;) {}`)
	}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Set("nameRead", func() { nameReads.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RunString(`Object.defineProperty(native, "name", {
		get() { nameRead(); return "native"; }
	});`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := rt.RunString(`native()`)
		done <- err
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		close(release)
		rt.Abort(errors.New("test startup timeout"))
		t.Fatal("native callback did not begin")
	}
	cause := errors.New("host abort during native callback")
	rt.Abort(cause)
	close(release)
	select {
	case err := <-done:
		requireAbortCause(t, err, cause)
		requireAbortCause(t, innerErr, cause)
	case <-time.After(5 * time.Second):
		t.Fatal("interrupt stack capture reentered JavaScript or deadlocked")
	}
	if calls := nameReads.Load(); calls != 0 {
		t.Fatalf("interrupt stack capture read a user name getter %d times", calls)
	}
}

func TestAbortAllowsNativePromiseDisposal(t *testing.T) {
	rt := New()
	promise, _, reject := rt.NewPromise()
	cause := errors.New("host abort")
	reason := errors.New("loop terminated")
	rt.Abort(cause)
	if err := reject(rt.NewGoError(reason)); err != nil {
		t.Fatalf("native terminal rejection failed: %v", err)
	}
	if promise.State() != PromiseStateRejected {
		t.Fatalf("terminal promise state = %v, want rejected", promise.State())
	}
	value := promise.Result().(*Object).Get("value")
	if value.Export() != reason {
		t.Fatalf("terminal rejection reason = %v, want original Go error", value)
	}
	_, err := rt.RunString(`1 + 1`)
	requireAbortCause(t, err, cause)
}

func TestNewGoErrorDoesNotInvokeInheritedSetter(t *testing.T) {
	for _, abort := range []bool{false, true} {
		rt := New()
		var setters int
		if err := rt.Set("setter", func() { setters++ }); err != nil {
			t.Fatal(err)
		}
		if _, err := rt.RunString(`Object.defineProperty(Object.prototype, "value", {
			set(value) { setter(); throw new Error("must not run"); }, configurable: true
		});`); err != nil {
			t.Fatal(err)
		}
		if abort {
			rt.Abort(errors.New("host abort"))
		}
		cause := errors.New("native error")
		value := rt.NewGoError(cause)
		if setters != 0 {
			t.Fatalf("abort=%v: NewGoError invoked inherited setter %d times", abort, setters)
		}
		if got := value.Get("value").Export(); got != cause {
			t.Fatalf("abort=%v: own error value = %v, want original error", abort, got)
		}
	}
}

func TestInterruptRemainsReusableWithoutAbort(t *testing.T) {
	rt := New()
	cause := errors.New("ordinary interrupt")
	rt.Interrupt(cause)
	_, err := rt.RunString(`for (;;) {}`)
	requireAbortCause(t, err, cause)
	value, err := rt.RunString(`6 * 7`)
	if err != nil || value.ToInteger() != 42 {
		t.Fatalf("ordinary interrupted runtime is not reusable: value=%v error=%v", value, err)
	}
}

func TestUncatchableUnwindSkipsIteratorClose(t *testing.T) {
	for _, abort := range []bool{false, true} {
		name := "interrupt"
		if abort {
			name = "abort"
		}
		t.Run(name, func(t *testing.T) {
			rt := New()
			cause := errors.New("stop loop")
			returnCalls, returnTraps, finallyRuns := 0, 0, 0
			if err := rt.Set("onReturnCall", func() { returnCalls++ }); err != nil {
				t.Fatal(err)
			}
			if err := rt.Set("onReturnTrap", func() { returnTraps++ }); err != nil {
				t.Fatal(err)
			}
			if err := rt.Set("onFinally", func() { finallyRuns++ }); err != nil {
				t.Fatal(err)
			}
			if err := rt.Set("stopNow", func() {
				if abort {
					rt.Abort(cause)
				} else {
					rt.Interrupt(cause)
				}
			}); err != nil {
				t.Fatal(err)
			}
			_, err := rt.RunString(`
				const iterable = {
					[Symbol.iterator]() {
						const raw = {
							next() { return { value: 1, done: false }; },
							return() { onReturnCall(); return { done: true }; }
						};
						return new Proxy(raw, {
							get(target, key, receiver) {
								if (key === "return") { onReturnTrap(); }
								return Reflect.get(target, key, receiver);
							}
						});
					}
				};
				function* gen() {
					try { yield 1; } finally { onFinally(); }
				}
				function outerLoop() {
					for (const a of iterable) {
						for (const b of gen()) {
							stopNow();
							for (;;) {}
						}
					}
				}
				outerLoop();
			`)
			requireAbortCause(t, err, cause)
			var interrupted *InterruptedError
			if !errors.As(err, &interrupted) || !strings.Contains(interrupted.String(), "outerLoop") {
				t.Fatalf("interrupt stack lost outerLoop frame: %s", err)
			}
			if returnCalls != 0 || returnTraps != 0 || finallyRuns != 0 {
				t.Fatalf("IteratorClose ran on uncatchable unwind: returnCalls=%d returnTraps=%d finallyRuns=%d", returnCalls, returnTraps, finallyRuns)
			}
			if len(rt.vm.iterStack) != 0 || len(rt.vm.refStack) != 0 {
				t.Fatalf("VM stacks not cleared: iterStack=%d refStack=%d", len(rt.vm.iterStack), len(rt.vm.refStack))
			}
			if !abort {
				val, err := rt.RunString(`let s = 0; for (const x of [10, 20, 12]) { s += x; } s;`)
				if err != nil || val.ToInteger() != 42 {
					t.Fatalf("reused runtime failed after interrupt in for-of: val=%v err=%v", val, err)
				}
			}
		})
	}

	t.Run("promise_all_step_abort_skips_iterator_close", func(t *testing.T) {
		rt := New()
		cause := errors.New("abort inside promise.all step")
		returnCalls := 0
		if err := rt.Set("onReturn", func() { returnCalls++ }); err != nil {
			t.Fatal(err)
		}
		if err := rt.Set("abortStep", func() {
			rt.Abort(cause)
			if _, err := rt.RunString(`for (;;) {}`); err != nil {
				panic(err)
			}
		}); err != nil {
			t.Fatal(err)
		}
		_, err := rt.RunString(`
			const iterable = {
				[Symbol.iterator]() {
					return {
						next() { return { value: 1, done: false }; },
						return() { onReturn(); return { done: true }; }
					};
				}
			};
			function CustomPromise(executor) {
				executor(function () {}, function () {});
			}
			CustomPromise.resolve = function () {
				abortStep();
			};
			Promise.all.call(CustomPromise, iterable);
		`)
		requireAbortCause(t, err, cause)
		if returnCalls != 0 {
			t.Fatalf("IteratorClose ran on uncatchable Promise.all step: %d calls", returnCalls)
		}
	})

	t.Run("promise_all_iterator_close_propagates_abort", func(t *testing.T) {
		rt := New()
		cause := errors.New("abort inside iterator return")
		returnCalls := 0
		if err := rt.Set("onReturn", func() {
			returnCalls++
			rt.Abort(cause)
			_, err := rt.RunString(`for (;;) {}`)
			if err != nil {
				panic(err)
			}
		}); err != nil {
			t.Fatal(err)
		}
		_, err := rt.RunString(`
			const iterable = {
				[Symbol.iterator]() {
					return {
						next() { return { value: 1, done: false }; },
						return() { onReturn(); return { done: true }; }
					};
				}
			};
			function CustomPromise(executor) {
				executor(function () {}, function () {});
			}
			CustomPromise.resolve = function () {
				throw new Error("step failure");
			};
			Promise.all.call(CustomPromise, iterable);
		`)
		requireAbortCause(t, err, cause)
		if returnCalls != 1 {
			t.Fatalf("returnCalls = %d, want 1", returnCalls)
		}
	})
}
