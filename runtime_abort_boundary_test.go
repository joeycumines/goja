package goja

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type abortBoundaryNativeError string

func (e *abortBoundaryNativeError) Error() string { return string(*e) }

func TestExceptionUnwrapDoesNotExecuteJavaScript(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		rt := New()
		failure := new(abortBoundaryNativeError)
		*failure = "original native failure"
		native := rt.NewGoError(failure)
		if err := rt.Set("nativeFailure", native); err != nil {
			t.Fatal(err)
		}
		reads := 0
		if err := rt.Set("read", func() { reads++ }); err != nil {
			t.Fatal(err)
		}
		if _, err := rt.RunString(`
			Object.defineProperty(nativeFailure, "value", {
				get() { read(); throw new Error("must not run"); }
			});
			Object.setPrototypeOf(nativeFailure, new Proxy({}, {
				getPrototypeOf() { read(); throw new Error("must not run"); }
			}));
		`); err != nil {
			t.Fatal(err)
		}
		_, err := rt.RunString(`throw nativeFailure`)
		if _, ok := err.(*Exception); !ok {
			t.Fatalf("exception type = %T", err)
		}
		if terminal {
			rt.Abort(errors.New("stop"))
		}
		func() {
			defer func() {
				if x := recover(); x != nil {
					t.Fatalf("terminal=%v: errors.Is panicked with %T", terminal, x)
				}
			}()
			if !errors.Is(err, failure) {
				t.Fatal("native error identity was lost")
			}
			var original *abortBoundaryNativeError
			if !errors.As(err, &original) || original != failure {
				t.Fatal("native error type was lost")
			}
		}()
		if reads != 0 || len(rt.vm.callStack) != 0 || len(rt.vm.tryStack) != 0 || rt.vm.prg != nil || rt.vm.sb != -1 || len(rt.vm.stack) != 0 {
			t.Fatalf("terminal=%v: unwrapping executed JS or changed VM state", terminal)
		}
	}
}

func TestIteratorClassificationDoesNotInspectThrownValues(t *testing.T) {
	for name, thrown := range map[string]string{
		"go_error": `Object.defineProperty(new GoError("original"), "value", {
			get() { reads++; throw replacement; }
		})`,
		"proxy": `new Proxy({}, {
			getPrototypeOf() { reads++; throw replacement; }
		})`,
	} {
		t.Run(name, func(t *testing.T) {
			rt := New()
			v, err := rt.RunString(`
				let reads = 0, closes = 0;
				const replacement = new Error("replacement");
				const original = ` + thrown + `;
				const iterable = {
					[Symbol.iterator]() { return {
						next() { return {value: 1, done: false}; },
						return() { closes++; return {done: true}; }
					}; }
				};
				let caught;
				try { Array.from(iterable, () => { throw original; }); }
				catch (e) { caught = e; }
				JSON.stringify([reads, closes, caught === original]);
			`)
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String(); got != `[0,1,true]` {
				t.Fatalf("iterator cleanup/exception changed: %s", got)
			}
		})
	}
}

func TestExceptionUnwrapIgnoresJavaScriptLookalikes(t *testing.T) {
	rt := New()
	reads := 0
	if err := rt.Set("read", func() { reads++ }); err != nil {
		t.Fatal(err)
	}
	_, err := rt.RunString(`throw Object.create(GoError.prototype, {
		value: {get() { read(); throw new Error("must not run"); }}
	})`)
	if _, ok := err.(*Exception); !ok {
		t.Fatalf("exception type = %T", err)
	}
	rt.Abort(errors.New("stop"))
	defer func() {
		if x := recover(); x != nil {
			t.Fatalf("errors.Is panicked with %T", x)
		}
	}()
	if errors.Is(err, errors.New("unrelated")) || errors.Unwrap(err) != nil || reads != 0 {
		t.Fatal("JavaScript object was treated as a native error wrapper")
	}
}

func TestExceptionUnwrapNilValues(t *testing.T) {
	var exception *Exception
	var object *Object
	for _, ex := range []*Exception{exception, {val: object}} {
		if ex.Unwrap() != nil || errors.Is(ex, errors.New("unrelated")) {
			t.Fatal("nil exception value unwrapped to an error")
		}
	}
}

func TestIteratorClosePreservesCatchFrameAfterStackGrowth(t *testing.T) {
	rt := New()
	v, err := rt.RunString(`
		function nested(n) {
			if (n) { try { nested(n - 1); } finally {} }
		}
		const iterable = {
			[Symbol.iterator]() { return {
				next() { return {value: 1, done: false}; },
				return() { nested(64); return {done: true}; }
			}; }
		};
		let catches = 0, outer;
		try {
			try { for (const x of iterable) { throw "first"; } }
			catch (e) { if (++catches === 1) { throw "second"; } }
		} catch (e) { outer = e; }
		JSON.stringify([catches, outer]);
	`)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String(); got != `[1,"second"]` {
		t.Fatalf("catch state after IteratorClose = %s", got)
	}
}

func TestAbortInitialTransitionRacesInterruptAndClear(t *testing.T) {
	for iteration := 0; iteration < 50; iteration++ {
		rt := New()
		ready := make(chan struct{})
		if err := rt.Set("ready", func() { close(ready) }); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := rt.RunString(`ready(); for (;;) {}`)
			done <- err
		}()
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			rt.Abort("startup timeout")
			t.Fatal("active execution did not start")
		}
		start := make(chan struct{})
		causes := []error{errors.New("abort A"), errors.New("abort B"), errors.New("abort C")}
		var wg sync.WaitGroup
		for _, cause := range causes {
			wg.Go(func() { <-start; rt.Abort(cause) })
		}
		for range 3 {
			wg.Go(func() {
				<-start
				for range 20 {
					rt.Interrupt("ordinary")
					rt.ClearInterrupt()
				}
			})
		}
		close(start)
		wg.Wait()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("active execution did not stop")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("active execution did not return after Abort")
		}
		_, err := rt.RunString(`1`)
		var interrupted *InterruptedError
		if !errors.As(err, &interrupted) {
			t.Fatalf("terminal error type = %T", err)
		}
		winner, ok := interrupted.Value().(error)
		if !ok || winner != causes[0] && winner != causes[1] && winner != causes[2] {
			t.Fatal("ordinary interrupt replaced the terminal cause")
		}
		rt.ClearInterrupt()
		rt.Interrupt("late ordinary")
		rt.Abort(errors.New("late abort"))
		_, err = rt.RunString(`1`)
		requireAbortCause(t, err, winner)
	}
}
