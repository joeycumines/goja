package goja

import (
	"errors"
	"testing"
)

func TestPromiseSettlementObserverPanicStillEnqueuesFulfillReactions(t *testing.T) {
	rt := New()
	var jobs []func()
	rt.SetPromiseJobEnqueuer(func(job func()) { jobs = append(jobs, job) })
	if _, err := rt.RunString(`var sawFulfillment = false`); err != nil {
		t.Fatal(err)
	}

	promise, resolve, _ := rt.NewPromise()
	then, ok := AssertFunction(promise.val.Get("then"))
	if !ok {
		t.Fatalf("Promise.prototype.then is not callable: %T", promise.val.Get("then"))
	}
	_, err := then(promise.val, rt.ToValue(func(call FunctionCall) Value {
		if call.Argument(0).ToInteger() == 42 {
			if _, err := rt.RunString(`sawFulfillment = true`); err != nil {
				t.Fatal(err)
			}
		}
		return Undefined()
	}))
	if err != nil {
		t.Fatal(err)
	}

	promise.ObserveSettlement(func() {
		panic("observer failed")
	})

	func() {
		defer func() {
			if got := recover(); got != "observer failed" {
				t.Fatalf("panic = %v, want observer failure", got)
			}
		}()
		if err := resolve(42); err != nil {
			t.Fatalf("fulfillment returned an error: %v", err)
		}
		t.Fatal("observer panic did not propagate")
	}()

	if promise.State() != PromiseStateFulfilled || promise.Result().ToInteger() != 42 {
		t.Fatalf("settlement = (%v, %v), want fulfillment with 42", promise.State(), promise.Result())
	}
	if len(jobs) == 0 {
		t.Fatal("settlement observer panic dropped the already-registered JavaScript reaction")
	}
	for _, job := range jobs {
		if err := rt.RunPromiseJob(job); err != nil {
			t.Fatalf("reaction job failed: %v", err)
		}
	}
	value, err := rt.RunString(`sawFulfillment`)
	if err != nil || !value.ToBoolean() {
		t.Fatalf("reaction did not run: value=%v error=%v", value, err)
	}
}

func TestPromiseSettlementObserverPanicStillTracksRejection(t *testing.T) {
	rt := New()
	var jobs []func()
	rejected, handled := 0, 0
	rt.SetPromiseJobEnqueuer(func(job func()) { jobs = append(jobs, job) })
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
		panic("observer failed")
	})

	func() {
		defer func() {
			if got := recover(); got != "observer failed" {
				t.Fatalf("panic = %v, want observer failure", got)
			}
		}()
		if err := reject(errors.New("rejected")); err != nil {
			t.Fatalf("rejection returned an error: %v", err)
		}
		t.Fatal("observer panic did not propagate")
	}()

	if promise.State() != PromiseStateRejected {
		t.Fatalf("settlement state = %v, want rejected", promise.State())
	}
	if rejected != 1 || handled != 0 {
		t.Fatalf("rejection tracking = (rejected=%d, handled=%d), want (1, 0)", rejected, handled)
	}
	if len(jobs) != 0 {
		t.Fatalf("unexpected reaction jobs = %d", len(jobs))
	}
}

func TestPromiseSettlementObserverPanicStopsLaterObservers(t *testing.T) {
	rt := New()
	promise, resolve, _ := rt.NewPromise()
	later := false
	promise.ObserveSettlement(func() { panic("observer failed") })
	promise.ObserveSettlement(func() { later = true })

	func() {
		defer func() {
			if got := recover(); got != "observer failed" {
				t.Fatalf("panic = %v, want observer failure", got)
			}
		}()
		if err := resolve("done"); err != nil {
			t.Fatalf("fulfillment returned an error: %v", err)
		}
		t.Fatal("observer panic did not propagate")
	}()

	if later {
		t.Fatal("observer registered after the panicking observer still ran")
	}
	if len(promise.settlementObservers) != 0 {
		t.Fatalf("settlement observers retained after notification: %d", len(promise.settlementObservers))
	}
}
