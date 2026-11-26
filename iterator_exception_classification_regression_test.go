package goja

import (
	"errors"
	"fmt"
	"testing"
)

func iteratorClassificationCause(t *testing.T, overflow bool) error {
	t.Helper()
	stopped := New()
	var err error
	if overflow {
		stopped.SetMaxCallStackSize(1)
		_, err = stopped.RunString(`(function f() { f(); })()`)
		var target *StackOverflowError
		if !errors.As(err, &target) {
			t.Fatalf("expected stack overflow, got %v", err)
		}
	} else {
		stopped.Interrupt(errors.New("previous interruption"))
		_, err = stopped.RunString(`1`)
		var target *InterruptedError
		if !errors.As(err, &target) {
			t.Fatalf("expected interruption, got %v", err)
		}
	}
	return err
}

func TestIteratorCloseForCatchableNativeErrorWrapper(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		for _, wrapped := range []bool{false, true} {
			for _, nativeThrow := range []bool{false, true} {
				name := fmt.Sprintf("overflow=%v/wrapped=%v/nativeThrow=%v", overflow, wrapped, nativeThrow)
				t.Run(name, func(t *testing.T) {
					rt := New()
					cause := iteratorClassificationCause(t, overflow)
					if wrapped {
						cause = fmt.Errorf("operation failed: %w", cause)
					}
					original := rt.NewGoError(cause)
					if err := rt.Set("original", original); err != nil {
						t.Fatal(err)
					}
					if err := rt.Set("nativeThrow", func() { panic(original) }); err != nil {
						t.Fatal(err)
					}
					thrower := `() => { throw original; }`
					if nativeThrow {
						thrower = `nativeThrow`
					}
					value, err := rt.RunString(`
						let closes = 0, caught = false;
						const iterable = {
							[Symbol.iterator]() { return {
								next() { return {value: 1, done: false}; },
								return() { closes++; return {done: true}; }
							}; }
						};
						try { Array.from(iterable, ` + thrower + `); }
						catch (e) { caught = e === original; }
						JSON.stringify([closes, caught]);
					`)
					if err != nil {
						t.Fatal(err)
					}
					if got := value.String(); got != `[1,true]` {
						t.Fatalf("iterator cleanup and exception identity = %s, want [1,true]", got)
					}
					assertPromiseBoundaryClean(t, rt)
				})
			}
		}
	}
}

func TestIteratorCloseStillSkipsNativeUncatchableErrors(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		for _, wrapped := range []bool{false, true} {
			t.Run(fmt.Sprintf("overflow=%v/wrapped=%v", overflow, wrapped), func(t *testing.T) {
				rt := New()
				cause := iteratorClassificationCause(t, overflow)
				if wrapped {
					cause = fmt.Errorf("operation failed: %w", cause)
				}
				if err := rt.Set("fail", func() { panic(cause) }); err != nil {
					t.Fatal(err)
				}
				_, err := rt.RunString(`
					var closes = 0, caught = false;
					const iterable = {
						[Symbol.iterator]() { return {
							next() { return {value: 1, done: false}; },
							return() { closes++; return {done: true}; }
						}; }
					};
					try { Array.from(iterable, fail); }
					catch (e) { caught = true; }
				`)
				if err == nil || asUncatchableException(err) == nil {
					t.Fatalf("native uncatchable error did not propagate: %v", err)
				}
				if closes, caught := rt.Get("closes").ToInteger(), rt.Get("caught").ToBoolean(); closes != 0 || caught {
					t.Fatalf("native unwind ran JavaScript cleanup: closes=%d caught=%v", closes, caught)
				}
				assertPromiseBoundaryClean(t, rt)
			})
		}
	}
}
