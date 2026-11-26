package goja

import (
	"errors"
	"fmt"
	"testing"
)

func TestGeneratorReturnAfterNestedIteratorClose(t *testing.T) {
	rt := New()
	v, err := rt.RunString(`
		let closed = 0, ticks = 0;
		function nested(n) {
			try { if (n) nested(n - 1); }
			finally { ticks++; }
		}
		const iterable = {
			[Symbol.iterator]() { return this; },
			next() { return { value: 1, done: false }; },
			return() { closed++; nested(32); return { done: true }; }
		};
		function* gen() {
			try { for (const x of iterable) yield x; }
			finally { yield 2; }
		}
		const g = gen();
		JSON.stringify([g.next(), g.return(3), g.next(), closed, ticks]);
	`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String(), `[{"value":1,"done":false},{"value":2,"done":false},{"value":3,"done":true},1,33]`; got != want {
		t.Fatalf("generator return after nested IteratorClose = %s, want %s", got, want)
	}
}

func TestGeneratorAndAsyncUnwindRestoresRuntime(t *testing.T) {
	for name, script := range map[string]string{
		"generator_next":             `function* g(){ stop(); for(;;){} } g().next();`,
		"generator_default_argument": `function* g(x=stop()){ yield x; } g();`,
		"generator_return_finally":   `function* g(){ try { yield 1; } finally { stop(); for(;;){} } } const x=g(); x.next(); x.return();`,
		"generator_throw":            `function* g(){ try {yield 1;} catch(e){stop();for(;;){}} } const x=g();x.next();x.throw(1);`,
		"generator_delegate":         `function* g(){ stop(); for(;;){} } function* f(){ yield* g(); } f().next();`,
		"async_start":                `async function f(){ stop(); for(;;){} } f();`,
		"async_resume":               `async function f(){ await 1; stop(); for(;;){} } f();`,
		"async_reject":               `async function f(){ try {await Promise.reject(1);} catch(e){stop();for(;;){}} } f();`,
	} {
		for _, mode := range []string{"interrupt", "abort", "panic"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				rt := New()
				cause := errors.New("stop")
				_ = rt.Set("stop", func() {
					switch mode {
					case "interrupt":
						rt.Interrupt(cause)
					case "abort":
						rt.Abort(cause)
					case "panic":
						panic(cause)
					}
				})
				var err error
				var recovered interface{}
				func() {
					defer func() { recovered = recover() }()
					_, err = rt.RunString(script)
				}()
				if mode == "panic" {
					if recovered != cause {
						t.Fatalf("panic=%v, want %v", recovered, cause)
					}
				} else {
					if recovered != nil {
						t.Fatalf("unexpected panic: %v", recovered)
					}
					requireAbortCause(t, err, cause)
				}
				if len(rt.vm.tryStack) != 0 || len(rt.vm.callStack) != 0 || len(rt.vm.iterStack) != 0 || len(rt.vm.refStack) != 0 || len(rt.vm.stack) != 0 || rt.vm.prg != nil || rt.vm.sb != -1 {
					t.Fatalf("dirty VM: try=%d call=%d iter=%d ref=%d stack=%d prg=%v sb=%d", len(rt.vm.tryStack), len(rt.vm.callStack), len(rt.vm.iterStack), len(rt.vm.refStack), len(rt.vm.stack), rt.vm.prg, rt.vm.sb)
				}
				v, err := rt.RunString(`21 + 21`)
				if mode == "abort" {
					requireAbortCause(t, err, cause)
				} else if err != nil || v.ToInteger() != 42 {
					t.Fatalf("runtime reuse failed: value=%v err=%v", v, err)
				}
			})
		}
	}
}

func TestGeneratorAndAsyncUnwindPreservesOuterExecution(t *testing.T) {
	for name, source := range map[string]string{
		"generator_next":  `(function* () { stop(); for (;;) {} })()`,
		"generator_start": `(function* (x = stop()) { yield x; })`,
		"async_start":     `(async function () { stop(); for (;;) {} })`,
	} {
		for _, rawPanic := range []bool{false, true} {
			t.Run(name+"/panic="+fmt.Sprint(rawPanic), func(t *testing.T) {
				rt := New()
				cause := errors.New("inner stop")
				_ = rt.Set("stop", func() {
					if rawPanic {
						panic(cause)
					}
					rt.Interrupt(cause)
				})
				target, err := rt.RunString(source)
				if err != nil {
					t.Fatal(err)
				}
				this := Undefined()
				fnValue := target
				if name == "generator_next" {
					this = target
					fnValue = target.(*Object).Get("next")
				}
				fn, ok := AssertFunction(fnValue)
				if !ok {
					t.Fatal("not callable")
				}
				var innerErr error
				var innerPanic interface{}
				_ = rt.Set("invoke", func() int {
					func() {
						defer func() { innerPanic = recover() }()
						_, innerErr = fn(this)
					}()
					rt.ClearInterrupt()
					return 2
				})
				v, err := rt.RunString(`let obj={x:40}; for(const x of [1]) { obj.x += invoke(); } obj.x;`)
				if err != nil || v.ToInteger() != 42 {
					t.Fatalf("outer execution failed: value=%v err=%v", v, err)
				}
				if rawPanic {
					if innerPanic != cause {
						t.Fatalf("inner panic=%v, want %v", innerPanic, cause)
					}
				} else {
					if innerPanic != nil {
						t.Fatalf("unexpected inner panic: %v", innerPanic)
					}
					requireAbortCause(t, innerErr, cause)
				}
				if name == "generator_next" {
					v, err := fn(this)
					if err != nil || !v.(*Object).Get("done").ToBoolean() {
						t.Fatalf("interrupted generator not completed: %v %v", v, err)
					}
				}
			})
		}
	}
}

func TestGeneratorAndAsyncStackOverflowRestoresRuntime(t *testing.T) {
	for name, script := range map[string]string{
		"generator": `function* g(n){if(n>0){yield* g(n-1);}else{yield 1;}} g(64).next();`,
		"async":     `async function f(){return f();} f();`,
	} {
		for _, limit := range []int{0, 1, 2, 16} {
			t.Run(name+"/limit="+fmt.Sprint(limit), func(t *testing.T) {
				rt := New()
				rt.SetMaxCallStackSize(limit)
				_, err := rt.RunString(script)
				var overflow *StackOverflowError
				if !errors.As(err, &overflow) {
					t.Fatalf("err=%v, want StackOverflowError", err)
				}
				if len(rt.vm.callStack) != 0 || len(rt.vm.tryStack) != 0 || len(rt.vm.iterStack) != 0 || len(rt.vm.refStack) != 0 {
					t.Fatalf("dirty stacks after overflow: call=%d try=%d iter=%d ref=%d", len(rt.vm.callStack), len(rt.vm.tryStack), len(rt.vm.iterStack), len(rt.vm.refStack))
				}
				rt.SetMaxCallStackSize(1024)
				v, err := rt.RunString(`21+21`)
				if err != nil || v.ToInteger() != 42 {
					t.Fatalf("runtime reuse failed: value=%v err=%v", v, err)
				}
			})
		}
	}
}

func TestAbortAsyncUnwindForwardsBufferedJobs(t *testing.T) {
	rt := New()
	rt.SetAsyncContextTracker(noopAsyncContextTracker{})
	cause := errors.New("async abort")
	var jobs []func()
	runInline := true
	rt.SetPromiseJobEnqueuer(func(job func()) {
		if runInline {
			_ = rt.RunPromiseJob(job)
			return
		}
		jobs = append(jobs, job)
	})
	reached := 0
	_ = rt.Set("neverRuns", func() { reached++ })
	_ = rt.Set("triggerAbort", func() { runInline = false; rt.Abort(cause) })
	_, err := rt.RunString(`
		Promise.resolve().then(async function () {
			Promise.resolve().then(neverRuns);
			triggerAbort();
			for (;;) {}
		});
		for (;;) {}
	`)
	requireAbortCause(t, err, cause)
	if len(jobs) != 1 || len(rt.jobQueue) != 0 || len(rt.vm.callStack) != 0 || len(rt.vm.tryStack) != 0 || reached != 0 {
		t.Fatalf("forwarded=%d queued=%d call=%d try=%d reached=%d", len(jobs), len(rt.jobQueue), len(rt.vm.callStack), len(rt.vm.tryStack), reached)
	}
	requireAbortCause(t, rt.RunPromiseJob(jobs[0]), cause)
}

func TestAsyncSettlementPanicAfterReentryRestoresRuntime(t *testing.T) {
	rt := New()
	cause := errors.New("then getter panic")
	thenable := rt.NewObject()
	getter := rt.ToValue(func(FunctionCall) Value {
		if _, err := rt.RunString(`1 + 1`); err != nil {
			panic(err)
		}
		panic(cause)
	})
	if err := thenable.DefineAccessorProperty("then", getter, Undefined(), FLAG_TRUE, FLAG_TRUE); err != nil {
		t.Fatal(err)
	}
	if err := rt.Set("thenable", thenable); err != nil {
		t.Fatal(err)
	}
	var recovered interface{}
	func() {
		defer func() { recovered = recover() }()
		_, _ = rt.RunString(`(async function(a,b,c){return thenable})(1,2,3)`)
	}()
	if recovered != cause {
		t.Fatalf("panic=%v, want original %v", recovered, cause)
	}
	if len(rt.vm.callStack) != 0 || len(rt.vm.tryStack) != 0 || len(rt.vm.stack) != 0 {
		t.Fatalf("dirty VM: call=%d try=%d stack=%d", len(rt.vm.callStack), len(rt.vm.tryStack), len(rt.vm.stack))
	}
	v, err := rt.RunString(`21+21`)
	if err != nil || v.ToInteger() != 42 {
		t.Fatalf("runtime reuse failed: value=%v err=%v", v, err)
	}
}
