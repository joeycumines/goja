package goja

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestNativeIteratorLifecycle(t *testing.T) {
	for name, expression := range map[string]string{
		"direct":    `sequence`,
		"delegated": `(function* () { yield* sequence; })()`,
		"map":       `Iterator.from(sequence).map(x => x)`,
		"flatMap":   `(function* () { yield 0; })().flatMap(() => sequence)`,
		"concat":    `Iterator.concat(sequence)`,
		"zip":       `Iterator.zip([sequence, [0, 1, 2]])`,
		"borrowed_next": `(() => { const i = sequence[Symbol.iterator](); return {
			[Symbol.iterator]() { return this; }, next: i.next, return: i.return,
		}; })()`,
	} {
		for _, mode := range []string{"exhaust", "break", "throw", "panic", "interrupt", "overflow", "abort"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				rt := New()
				started, stopped := 0, 0
				sequence := func(yield func(int) bool) {
					started++
					defer func() { stopped++ }()
					for n := range 3 {
						if !yield(n) {
							return
						}
					}
				}
				if err := rt.Set("sequence", sequence); err != nil {
					t.Fatal(err)
				}
				primary := errors.New("native iterator exit")
				if err := rt.Set("fail", func() {
					switch mode {
					case "panic":
						panic(primary)
					case "interrupt":
						rt.Interrupt(primary)
					case "overflow":
						rt.SetMaxCallStackSize(0)
						if _, err := rt.RunString(`1`); err != nil {
							panic(err)
						}
					case "abort":
						rt.Abort(primary)
					}
				}); err != nil {
					t.Fatal(err)
				}
				body := `fail();`
				switch mode {
				case "exhaust":
					body = ``
				case "break":
					body = `break;`
				case "throw":
					body = `throw "expected";`
				}
				var err error
				panicked := tryFunc(func() {
					_, err = rt.RunString(fmt.Sprintf(`for (const value of %s) { %s }`, expression, body))
				})
				switch mode {
				case "panic":
					if panicked != primary || err != nil {
						t.Fatalf("panic=%v err=%v", panicked, err)
					}
				case "interrupt", "abort":
					if panicked != nil || !errors.Is(err, primary) {
						t.Fatalf("panic=%v err=%v", panicked, err)
					}
				case "overflow":
					var overflow *StackOverflowError
					if panicked != nil || !errors.As(err, &overflow) {
						t.Fatalf("panic=%v err=%v", panicked, err)
					}
				case "throw":
					var exception *Exception
					if panicked != nil || !errors.As(err, &exception) || exception.Value().String() != "expected" {
						t.Fatalf("panic=%v err=%v", panicked, err)
					}
				default:
					if panicked != nil || err != nil {
						t.Fatalf("panic=%v err=%v", panicked, err)
					}
				}
				if started != 1 || stopped != 1 || len(rt.nativeIterators) != 0 {
					t.Fatalf("started=%d stopped=%d registered=%d", started, stopped, len(rt.nativeIterators))
				}
				assertPromiseBoundaryClean(t, rt)
			})
		}
	}
}

func TestNativeIteratorInterruptPreservesUnrelatedSuspension(t *testing.T) {
	rt := New()
	stopped := 0
	if err := rt.Set("sequence", func(yield func(int) bool) {
		defer func() { stopped++ }()
		for i := range 3 {
			if !yield(i) {
				return
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RunString(`var saved = sequence[Symbol.iterator](); saved.next();`); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("interrupt current loop")
	if err := rt.Set("interrupt", func() { rt.Interrupt(cause) }); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RunString(`for (const value of sequence) interrupt();`); !errors.Is(err, cause) {
		t.Fatalf("got %v", err)
	}
	if stopped != 1 || len(rt.nativeIterators) != 1 {
		t.Fatalf("stopped=%d registered=%d", stopped, len(rt.nativeIterators))
	}
	if _, err := rt.RunString(`if (saved.next().value !== 1) throw "saved iterator lost"; saved.return();`); err != nil {
		t.Fatal(err)
	}
	if stopped != 2 || len(rt.nativeIterators) != 0 {
		t.Fatalf("stopped=%d registered=%d", stopped, len(rt.nativeIterators))
	}
	assertPromiseBoundaryClean(t, rt)
}

func TestNativeIteratorAbortSuspendedForAwait(t *testing.T) {
	rt := New()
	pending, _, _ := rt.NewPromise()
	stopped := 0
	if err := rt.Set("sequence", func(yield func(Value) bool) {
		defer func() { stopped++ }()
		yield(rt.ToValue(pending))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RunString(`(async () => { for await (const value of sequence) throw "must not run"; })()`); err != nil {
		t.Fatal(err)
	}
	if stopped != 0 || len(rt.nativeIterators) != 1 {
		t.Fatalf("before abort: stopped=%d registered=%d", stopped, len(rt.nativeIterators))
	}
	cause := errors.New("abort suspended await")
	rt.Abort(cause)
	if _, err := rt.RunString(`1`); !errors.Is(err, cause) {
		t.Fatalf("got %v", err)
	}
	if stopped != 1 || len(rt.nativeIterators) != 0 {
		t.Fatalf("after abort: stopped=%d registered=%d", stopped, len(rt.nativeIterators))
	}
	assertPromiseBoundaryClean(t, rt)
}

func TestNativeIteratorStopDeferredDuringNext(t *testing.T) {
	rt := New()
	stopped := 0
	source := func(yield func(int) bool) {
		defer func() { stopped++ }()
		// Model owner cleanup reentered from a currently running source.
		rt.Abort("stop")
		rt.closeNativeIterators()
		if stopped != 0 {
			t.Error("stop ran before source suspended")
		}
		yield(42)
	}
	obj := rt.newGoIter(rt.wrapIterSeq(reflect.ValueOf(source)))
	i := obj.self.(*goIteratorObject)
	i.nextResult(_undefined)
	i.closeOnAbrupt()
	if stopped != 1 || len(rt.nativeIterators) != 0 || i.stopPending || i.inNext {
		t.Fatalf("stopped=%d registered=%d pending=%v inNext=%v", stopped, len(rt.nativeIterators), i.stopPending, i.inNext)
	}
}

func TestNativeIteratorSecondaryStopPanicPreservesFrame(t *testing.T) {
	rt := New()
	primary, secondary := errors.New("body panic"), errors.New("stop panic")
	if err := rt.Set("secondary", func() { panic(secondary) }); err != nil {
		t.Fatal(err)
	}
	v, err := rt.RunString(`({ get value() { for (const x of [1]) secondary(); } })`)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := v.(*Object)
	stopped := 0
	if err := rt.Set("sequence", func(yield func(int) bool) {
		defer func() {
			stopped++
			cleanup.Get("value")
		}()
		yield(1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Set("primary", func() { panic(primary) }); err != nil {
		t.Fatal(err)
	}
	got := tryFunc(func() { _, _ = rt.RunString(`for (const x of sequence) primary();`) })
	if got != primary || stopped != 1 || len(rt.nativeIterators) != 0 {
		t.Fatalf("panic=%v stopped=%d registered=%d", got, stopped, len(rt.nativeIterators))
	}
	assertPromiseBoundaryClean(t, rt)
	if _, err := rt.RunString(`21 + 21`); err != nil {
		t.Fatal(err)
	}
}

func TestNativeIteratorEagerConsumerPanic(t *testing.T) {
	for _, api := range []string{"Promise.all", "ForOf"} {
		t.Run(api, func(t *testing.T) {
			rt := New()
			primary := errors.New("eager iterator consumer")
			stopped := 0
			source := rt.ToValue(func(yield func(int) bool) {
				defer func() { stopped++ }()
				yield(42)
			})
			if err := rt.Set("sequence", source); err != nil {
				t.Fatal(err)
			}
			if err := rt.Set("fail", func() { panic(primary) }); err != nil {
				t.Fatal(err)
			}
			panicked := tryFunc(func() {
				if api == "ForOf" {
					rt.ForOf(source, func(Value) bool { panic(primary) })
				} else {
					_, _ = rt.RunString(`Promise.resolve = fail; Promise.all(sequence);`)
				}
			})
			if panicked != primary || stopped != 1 || len(rt.nativeIterators) != 0 {
				t.Fatalf("panic=%v stopped=%d registered=%d", panicked, stopped, len(rt.nativeIterators))
			}
			assertPromiseBoundaryClean(t, rt)
		})
	}
}

func TestNativeIteratorSeq2ErrorPrecedesStopPanic(t *testing.T) {
	for _, catchable := range []bool{false, true} {
		t.Run(fmt.Sprintf("catchable=%v", catchable), func(t *testing.T) {
			rt := New()
			var primary error
			if catchable {
				_, primary = rt.RunString(`throw "yielded error";`)
			} else {
				rt.Interrupt("yielded interruption")
				_, primary = rt.RunString(`0`)
			}
			secondary := errors.New("stop panic")
			stopped := 0
			if err := rt.Set("sequence", func(yield func(int, error) bool) {
				defer func() {
					stopped++
					panic(secondary)
				}()
				yield(42, primary)
			}); err != nil {
				t.Fatal(err)
			}
			var err error
			panicked := tryFunc(func() { _, err = rt.RunString(`for (const value of sequence) {}`) })
			matchesPrimary := err == primary
			if catchable {
				var exception *Exception
				matchesPrimary = errors.As(err, &exception) && exception.Value().SameAs(primary.(*Exception).Value())
			}
			if panicked != nil || !matchesPrimary || stopped != 1 || len(rt.nativeIterators) != 0 {
				t.Fatalf("panic=%v err=%v want=%v stopped=%d registered=%d", panicked, err, primary, stopped, len(rt.nativeIterators))
			}
			assertPromiseBoundaryClean(t, rt)
		})
	}
}
