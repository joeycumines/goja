package goja

import (
	"errors"
	"testing"
)

func TestPromiseHandleTrackingSurvivesEnqueuerPanic(t *testing.T) {
	rt := New()
	rejected, handled := 0, 0
	rt.SetPromiseRejectionTracker(func(_ *Promise, op PromiseRejectionOperation) {
		if op == PromiseRejectionReject {
			rejected++
		} else {
			handled++
		}
	})
	p, _, reject := rt.NewPromise()
	if err := reject("reason"); err != nil {
		t.Fatal(err)
	}
	then, _ := AssertFunction(p.val.Get("then"))
	failure := errors.New("hook accepted job then failed")
	var jobs []func()
	rt.SetPromiseJobEnqueuer(func(job func()) { jobs = append(jobs, job); panic(failure) })
	func() {
		defer func() {
			if got := recover(); got != failure {
				t.Errorf("panic=%v", got)
			}
		}()
		_, _ = then(p.val)
	}()
	rt.SetPromiseJobEnqueuer(func(job func()) { jobs = append(jobs, job) })
	if _, err := then(p.val); err != nil {
		t.Fatal(err)
	}
	if rejected != 1 || handled != 1 || len(jobs) != 2 {
		t.Fatalf("tracking repeated first-handle notification: rejected=%d handled=%d jobs=%d", rejected, handled, len(jobs))
	}
}

func TestPromiseHandleTrackerPanicPreservesRegisteredReaction(t *testing.T) {
	rt := New()
	rejected, handled := 0, 0
	failure := errors.New("first-handle tracker failure")
	rt.SetPromiseRejectionTracker(func(_ *Promise, op PromiseRejectionOperation) {
		if op == PromiseRejectionReject {
			rejected++
		} else {
			handled++
			panic(failure)
		}
	})
	p, _, reject := rt.NewPromise()
	if err := reject("reason"); err != nil {
		t.Fatal(err)
	}
	then, _ := AssertFunction(p.val.Get("then"))
	var jobs []func()
	rt.SetPromiseJobEnqueuer(func(job func()) { jobs = append(jobs, job) })
	func() {
		defer func() {
			if got := recover(); got != failure {
				t.Errorf("panic=%v", got)
			}
		}()
		_, _ = then(p.val)
	}()
	if len(jobs) != 1 {
		t.Fatalf("committed reaction deliveries=%d, want 1", len(jobs))
	}
	if _, err := then(p.val); err != nil {
		t.Fatal(err)
	}
	if rejected != 1 || handled != 1 || len(jobs) != 2 {
		t.Fatalf("rejected=%d handled=%d jobs=%d", rejected, handled, len(jobs))
	}
	requirePromiseCleanupIdle(t, rt)
}

func TestPromiseHandleTrackerReentryNotifiesOnce(t *testing.T) {
	rt := New()
	var then Callable
	var p *Promise
	rejected, handled := 0, 0
	rt.SetPromiseRejectionTracker(func(_ *Promise, op PromiseRejectionOperation) {
		if op == PromiseRejectionReject {
			rejected++
			return
		}
		handled++
		if handled > 1 {
			t.Error("first-handle tracker reentered itself")
			return
		}
		if _, err := then(p.val); err != nil {
			t.Fatal(err)
		}
	})
	var reject func(interface{}) error
	p, _, reject = rt.NewPromise()
	if err := reject("reason"); err != nil {
		t.Fatal(err)
	}
	then, _ = AssertFunction(p.val.Get("then"))
	jobs := 0
	rt.SetPromiseJobEnqueuer(func(func()) { jobs++ })
	if _, err := then(p.val); err != nil {
		t.Fatal(err)
	}
	if rejected != 1 || handled != 1 || jobs != 2 {
		t.Fatalf("rejected=%d handled=%d jobs=%d", rejected, handled, jobs)
	}
}

func TestPromiseFailedContextGrabDoesNotCommitHandling(t *testing.T) {
	rt := New()
	handled, jobs := 0, 0
	rt.SetPromiseRejectionTracker(func(_ *Promise, op PromiseRejectionOperation) {
		if op == PromiseRejectionHandle {
			handled++
		}
	})
	p, _, reject := rt.NewPromise()
	if err := reject("reason"); err != nil {
		t.Fatal(err)
	}
	then, _ := AssertFunction(p.val.Get("then"))
	rt.SetPromiseJobEnqueuer(func(func()) { jobs++ })
	failure := errors.New("context capture failed")
	rt.SetAsyncContextTracker(promiseCleanupGrabTracker{grab: func() { panic(failure) }})
	func() {
		defer func() {
			if got := recover(); got != failure {
				t.Errorf("panic=%v", got)
			}
		}()
		_, _ = then(p.val)
	}()
	if p.handled || handled != 0 || jobs != 0 {
		t.Fatalf("failed Grab committed handling: handled=%v notifications=%d jobs=%d", p.handled, handled, jobs)
	}
	rt.SetAsyncContextTracker(nil)
	if _, err := then(p.val); err != nil {
		t.Fatal(err)
	}
	if !p.handled || handled != 1 || jobs != 1 {
		t.Fatalf("successful retry: handled=%v notifications=%d jobs=%d", p.handled, handled, jobs)
	}
}
