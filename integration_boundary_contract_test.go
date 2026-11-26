package goja

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestIntegrationNativeBoundaryKeepsQueueOwnership(t *testing.T) {
	for _, action := range []string{"program", "panic", "overflow", "promise_job"} {
		t.Run(action, func(t *testing.T) {
			r := New()
			r.SetMaxCallStackSize(32)
			p, resolve, _ := r.NewPromise()
			var order []string
			check := func(err error) {
				if err != nil {
					t.Fatal(err)
				}
			}
			check(r.Set("p", p))
			check(r.Set("first", func() {
				order = append(order, "start")
				switch action {
				case "program":
					_, err := r.RunString(`0`)
					check(err)
				case "panic":
					fn, _ := AssertFunction(r.ToValue(func() { panic("local") }))
					func() {
						defer func() {
							if recover() != "local" {
								t.Error("lost local panic")
							}
						}()
						_, _ = fn(Undefined())
					}()
				case "overflow":
					_, err := r.RunString(`(function f(){f()})()`)
					var overflow *StackOverflowError
					if !errors.As(err, &overflow) {
						t.Fatalf("overflow = %v", err)
					}
				case "promise_job":
					check(r.RunPromiseJob(func() { _, err := r.RunString(`0`); check(err) }))
				}
				order = append(order, "end")
			}))
			check(r.Set("second", func() { order = append(order, "second") }))
			check(r.Set("child", func() { order = append(order, "child") }))
			_, err := r.RunString(`p.then(first).then(child); p.then(second)`)
			check(err)
			check(resolve(1))
			if !reflect.DeepEqual(order, []string{"start", "end", "second", "child"}) {
				t.Fatalf("order=%v", order)
			}
			if r.nativeCallDepth != 0 || len(r.jobQueue) != 0 {
				t.Fatalf("depth=%d queue=%d", r.nativeCallDepth, len(r.jobQueue))
			}
			requirePromiseCleanupIdle(t, r)
		})
	}
}

func TestIntegrationAbruptHookRemovalAndReplacement(t *testing.T) {
	for _, abort := range []bool{false, true} {
		t.Run(fmt.Sprint(abort), func(t *testing.T) {
			r := New()
			failure := errors.New("primary")
			var calls []string
			r.SetPromiseJobEnqueuer(func(func()) {
				calls = append(calls, "A")
				r.SetPromiseJobEnqueuer(func(func()) {
					calls = append(calls, "B")
					r.SetPromiseJobEnqueuer(nil)
					r.enqueuePromiseJob(func() { t.Error("cleanup executed job") })
					panic("secondary")
				})
			})
			func() {
				defer func() {
					if got := recover(); got != failure {
						t.Fatalf("panic=%v", got)
					}
				}()
				_ = r.RunPromiseJob(func() {
					for range 4 {
						r.jobQueue = append(r.jobQueue, func() { t.Error("job executed") })
					}
					if abort {
						r.Abort("terminal")
					} else {
						r.Interrupt("ordinary")
					}
					panic(failure)
				})
			}()
			if !reflect.DeepEqual(calls, []string{"A", "B"}) {
				t.Fatalf("hook sequence=%v", calls)
			}
			if len(r.jobQueue) != 0 || r.promiseJobQueuePriority {
				t.Fatal("cleanup stranded queue")
			}
			_, err := r.RunString(`1`)
			var interrupted *InterruptedError
			if !errors.As(err, &interrupted) {
				t.Fatalf("panic consumed interrupt: %v", err)
			}
			requirePromiseCleanupIdle(t, r)
		})
	}
}

type integrationReentryTracker struct {
	r    *Runtime
	test func(string)
}

func (t integrationReentryTracker) Grab() interface{}   { return nil }
func (t integrationReentryTracker) Resumed(interface{}) { t.test("resumed") }
func (t integrationReentryTracker) Exited()             { t.test("exited") }

func TestIntegrationExplicitJobReentryRetainsHostOwnership(t *testing.T) {
	r := New()
	var jobs []func()
	r.SetPromiseJobEnqueuer(func(job func()) { jobs = append(jobs, job) })
	var job func()
	ran := 0
	stages := []string{}
	attempt := func(stage string) {
		stages = append(stages, stage)
		if err := r.RunPromiseJob(job); !errors.Is(err, ErrPromiseJobReentry) {
			t.Errorf("%s: %v", stage, err)
		}
	}
	r.SetAsyncContextTracker(integrationReentryTracker{r: r, test: func(stage string) { attempt(stage) }})
	if err := r.Set("first", func() { attempt("handler") }); err != nil {
		t.Fatal(err)
	}
	if err := r.Set("second", func() { ran++ }); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunString(`Promise.resolve().then(first); Promise.resolve().then(second)`); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs=%d", len(jobs))
	}
	job = jobs[1]
	if err := r.RunPromiseJob(jobs[0]); err != nil {
		t.Fatal(err)
	}
	if ran != 0 || !reflect.DeepEqual(stages, []string{"resumed", "handler", "exited"}) {
		t.Fatalf("ran=%d stages=%v", ran, stages)
	}
	attempt = func(string) {}
	if err := r.RunPromiseJob(job); err != nil {
		t.Fatal(err)
	}
	if ran != 1 {
		t.Fatal("host retry did not execute")
	}
	requirePromiseCleanupIdle(t, r)
}

func TestIntegrationExceptionClassificationNativeTrees(t *testing.T) {
	r := New()
	stopped := New()
	stopped.Interrupt("native")
	_, fatal := stopped.RunString(`0`)
	wrapped := &Exception{val: r.NewGoError(fatal)}
	for name, e := range map[string]error{
		"direct": fatal, "single": fmt.Errorf("wrapped: %w", fatal), "joined": errors.Join(errors.New("other"), fatal),
	} {
		if !isUncatchableException(e) || asUncatchableException(e) != e {
			t.Errorf("%s lost fatal wrapper", name)
		}
	}
	for name, e := range map[string]error{
		"exception": wrapped, "single": fmt.Errorf("wrapped: %w", wrapped), "joined": errors.Join(errors.New("other"), wrapped),
	} {
		if isUncatchableException(e) {
			t.Errorf("%s crossed catchable exception boundary", name)
		}
	}
	if !isJsException(wrapped) || isJsException(fatal) {
		t.Fatal("representation classification disagrees")
	}
}

func TestIntegrationDiagnosticNameShadowAndBound(t *testing.T) {
	r := New()
	fn := r.ToValue(func() {}).(*Object)
	proto := r.NewObject()
	_ = proto.Set("name", "inherited")
	_ = fn.SetPrototype(proto)
	fn.self.(interface{ stackNameBase() *baseObject }).stackNameBase().values["name"] = &valueProperty{value: nil}
	if got := getFuncName([]Value{fn}, 1); got != "" {
		t.Fatalf("nil property shadow=%q", got)
	}
	delete(fn.self.(interface{ stackNameBase() *baseObject }).stackNameBase().values, "name")
	base := proto.self.(*baseObject)
	delete(base.values, "name")
	base.prototype = proto
	if got := getFuncName([]Value{fn}, 1); got != "" {
		t.Fatalf("cycle name=%q", got)
	}
}
