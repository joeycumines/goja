package goja

import (
	"errors"
	"strings"
	"testing"
)

// captureAdoptedPromise creates a pending adopted result and retains the native
// resolving functions handed to its thenable. Calling those functions later
// exercises the inner resolving pair independently of NewPromise's outer pair.
func captureAdoptedPromise(t *testing.T, rt *Runtime) (*Promise, func(any) error, Callable, Callable) {
	t.Helper()
	promise, resolve, reject := rt.NewPromise()
	var innerResolve, innerReject Callable
	thenable := rt.NewObject()
	if err := thenable.Set("then", func(call FunctionCall) Value {
		innerResolve, _ = AssertFunction(call.Argument(0))
		innerReject, _ = AssertFunction(call.Argument(1))
		return Undefined()
	}); err != nil {
		t.Fatal(err)
	}
	if err := resolve(thenable); err != nil {
		t.Fatal(err)
	}
	if promise.State() != PromiseStatePending || innerResolve == nil || innerReject == nil {
		t.Fatal("thenable adoption did not retain a pending result and its resolving pair")
	}
	return promise, reject, innerResolve, innerReject
}

func TestPromiseAbortAdoptionPreservesOrdinaryResolution(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		name := "ordinary_adoption"
		if interrupted {
			name = "ordinary_interrupt"
		}
		t.Run(name, func(t *testing.T) {
			rt := New()
			promise, reject, resolveAdopted, _ := captureAdoptedPromise(t, rt)
			if interrupted {
				rt.Interrupt(errors.New("reusable interruption"))
			}
			if err := reject("must not replace adoption"); err != nil {
				t.Fatal(err)
			}
			if promise.State() != PromiseStatePending {
				t.Fatalf("ordinary rejection overrode adopted result: %v", promise.State())
			}
			rt.ClearInterrupt()
			if _, err := resolveAdopted(Undefined(), rt.ToValue(42)); err != nil {
				t.Fatal(err)
			}
			if promise.State() != PromiseStateFulfilled || promise.Result().ToInteger() != 42 {
				t.Fatalf("eventual adoption = (%v, %v), want fulfillment with 42", promise.State(), promise.Result())
			}
		})
	}
}

func TestPromiseAbortRejectsAdoptionOnce(t *testing.T) {
	for _, stale := range []string{"resolve", "reject"} {
		t.Run(stale, func(t *testing.T) {
			rt := New()
			promise, reject, resolveAdopted, rejectAdopted := captureAdoptedPromise(t, rt)
			observed := 0
			promise.ObserveSettlement(func() {
				observed++
				if promise.State() != PromiseStateRejected || promise.Result().String() != "terminal disposal" {
					t.Errorf("observer saw (%v, %v), want terminal rejection", promise.State(), promise.Result())
				}
			})
			rt.Abort(errors.New("irreversible cancellation"))
			if err := reject("terminal disposal"); err != nil {
				t.Fatal(err)
			}
			if promise.State() != PromiseStateRejected || promise.Result().String() != "terminal disposal" {
				t.Fatalf("aborted adoption = (%v, %v), want terminal rejection", promise.State(), promise.Result())
			}
			if observed != 1 {
				t.Fatalf("terminal settlement observers = %d, want one", observed)
			}
			// Native resolving functions remain callable for host cleanup after
			// Abort. Their independent latch must not replace the final outcome.
			late := resolveAdopted
			if stale == "reject" {
				late = rejectAdopted
			}
			if _, err := late(Undefined(), rt.ToValue("late adopted result")); err != nil {
				t.Fatal(err)
			}
			if err := reject("second terminal rejection"); err != nil {
				t.Fatal(err)
			}
			if promise.State() != PromiseStateRejected || promise.Result().String() != "terminal disposal" || observed != 1 {
				t.Fatalf("late result changed terminal disposition: state=%v result=%v observations=%d", promise.State(), promise.Result(), observed)
			}
		})
	}
}

func TestPromiseAbortKeepsCompletedOutcomes(t *testing.T) {
	for _, fulfilled := range []bool{false, true} {
		rt := New()
		promise, resolve, reject := rt.NewPromise()
		settle := reject
		want := PromiseStateRejected
		if fulfilled {
			settle = resolve
			want = PromiseStateFulfilled
		}
		if err := settle("recorded result"); err != nil {
			t.Fatal(err)
		}
		rt.Abort(errors.New("late host cancellation"))
		if err := reject("terminal disposal"); err != nil {
			t.Fatal(err)
		}
		if promise.State() != want || promise.Result().String() != "recorded result" {
			t.Fatalf("completed outcome changed after abort: state=%v result=%v", promise.State(), promise.Result())
		}
	}
}

func TestPromiseSettlementObserverRegistrationOrder(t *testing.T) {
	rt := New()
	promise, resolve, _ := rt.NewPromise()
	var order []string
	promise.ObserveSettlement(func() {
		order = append(order, "first")
		promise.ObserveSettlement(func() { order = append(order, "third") })
	})
	promise.ObserveSettlement(func() { order = append(order, "second") })
	if err := resolve("done"); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(order, ","), "first,third,second"; got != want {
		t.Fatalf("observer order = %s, want %s", got, want)
	}
	promise.ObserveSettlement(func() { order = append(order, "late") })
	if got, want := strings.Join(order, ","), "first,third,second,late"; got != want {
		t.Fatalf("settled observer registration = %s, want %s", got, want)
	}
}

func TestPromiseSettlementObserverPreservesRejectionTracking(t *testing.T) {
	rt := New()
	var rejected, handled, jobs, observed int
	rt.SetPromiseJobEnqueuer(func(func()) { jobs++ })
	rt.SetPromiseRejectionTracker(func(_ *Promise, operation PromiseRejectionOperation) {
		switch operation {
		case PromiseRejectionReject:
			rejected++
		case PromiseRejectionHandle:
			handled++
		}
	})
	promise, _, reject := rt.NewPromise()
	promise.ObserveSettlement(func() {
		observed++
		if promise.State() != PromiseStateRejected || promise.Result().String() != "ordinary rejection" {
			t.Errorf("observer did not receive a settled rejection: %v, %v", promise.State(), promise.Result())
		}
	})
	if err := reject("ordinary rejection"); err != nil {
		t.Fatal(err)
	}
	if observed != 1 || rejected != 1 || handled != 0 || jobs != 0 {
		t.Fatalf("observer altered promise behavior: observations=%d rejections=%d handled=%d jobs=%d", observed, rejected, handled, jobs)
	}
}
