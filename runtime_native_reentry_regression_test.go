package goja

import (
	"errors"
	"reflect"
	"testing"
)

func TestReviewNativeReactionRecoversNestedPanicKeepsSibling(t *testing.T) {
	r := New()
	p, resolve, _ := r.NewPromise()
	failure := errors.New("handled nested failure")
	bad, _ := AssertFunction(r.ToValue(func() { panic(failure) }))
	then, _ := AssertFunction(p.val.Get("then"))
	var order []string
	first := func() {
		func() {
			defer func() {
				if got := recover(); got != failure {
					t.Errorf("panic=%v", got)
				}
			}()
			_, _ = bad(Undefined())
		}()
		order = append(order, "first")
	}
	for _, handler := range []func(){first, func() { order = append(order, "second") }} {
		if _, err := then(p.val, r.ToValue(handler)); err != nil {
			t.Fatal(err)
		}
	}
	if err := resolve(1); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"first", "second"}) {
		t.Fatalf("order=%v; sibling reaction was lost", order)
	}
}

func TestReviewNativeReactionReentryFinishesBeforeSibling(t *testing.T) {
	r := New()
	p, resolve, _ := r.NewPromise()
	then, _ := AssertFunction(p.val.Get("then"))
	var order []string
	first := func() {
		order = append(order, "first-start")
		if _, err := r.RunString(`0`); err != nil {
			t.Fatal(err)
		}
		order = append(order, "first-end")
	}
	for _, handler := range []func(){first, func() { order = append(order, "second") }} {
		if _, err := then(p.val, r.ToValue(handler)); err != nil {
			t.Fatal(err)
		}
	}
	if err := resolve(1); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"first-start", "first-end", "second"}) {
		t.Fatalf("order=%v; sibling reaction interleaved", order)
	}
}

func TestReviewNativeEnqueuerRecoveredPanicKeepsQueue(t *testing.T) {
	r := New()
	var initial []func()
	r.SetPromiseJobEnqueuer(func(job func()) { initial = append(initial, job) })
	p, resolveP, _ := r.NewPromise()
	q, resolveQ, _ := r.NewPromise()
	thenP, _ := AssertFunction(p.val.Get("then"))
	thenQ, _ := AssertFunction(q.val.Get("then"))
	ran := 0
	for range 2 {
		if _, err := thenQ(q.val, r.ToValue(func() { ran++ })); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := thenP(p.val, r.ToValue(func() {
		if err := resolveQ(1); err != nil {
			panic(err)
		}
	})); err != nil {
		t.Fatal(err)
	}
	if err := resolveP(1); err != nil {
		t.Fatal(err)
	}
	if len(initial) != 1 {
		t.Fatalf("initial=%d", len(initial))
	}
	r.SetPromiseJobEnqueuer(nil)
	if err := r.RunPromiseJob(initial[0]); err != nil {
		t.Fatal(err)
	}
	if len(r.jobQueue) != 2 {
		t.Fatalf("pending=%d", len(r.jobQueue))
	}
	failure := errors.New("locally recovered")
	bad, _ := AssertFunction(r.ToValue(func() { panic(failure) }))
	deliveries := 0
	r.SetPromiseJobEnqueuer(func(func()) {
		deliveries++
		r.SetPromiseJobEnqueuer(nil)
		func() {
			defer func() {
				if got := recover(); got != failure {
					t.Errorf("panic=%v", got)
				}
			}()
			_, _ = bad(Undefined())
		}()
	})
	noop, _ := AssertFunction(r.ToValue(func() {}))
	if _, err := noop(Undefined()); err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 || ran != 1 {
		t.Fatalf("deliveries=%d ran=%d; unattempted sibling was lost", deliveries, ran)
	}
}
