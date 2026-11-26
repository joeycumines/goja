package goja

import (
	"errors"
	"reflect"
	"testing"
)

type promiseJobBoundaryTracker struct {
	resumed func()
	exited  func()
}

func (promiseJobBoundaryTracker) Grab() interface{} { return nil }
func (t promiseJobBoundaryTracker) Resumed(interface{}) {
	if t.resumed != nil {
		t.resumed()
	}
}
func (t promiseJobBoundaryTracker) Exited() {
	if t.exited != nil {
		t.exited()
	}
}

func TestRunPromiseJobDirectAPIRestoresBoundary(t *testing.T) {
	for _, phase := range []string{"resumed", "exited", "capability", "enqueuer"} {
		for _, nested := range []bool{false, true} {
			for _, terminal := range []bool{false, true} {
				name := phase + "/idle"
				if nested {
					name = phase + "/nested"
				}
				if terminal {
					name += "/abort"
				} else {
					name += "/catchable"
				}
				t.Run(name, func(t *testing.T) {
					rt := New()
					interpreted, handled := 0, 0
					if err := rt.Set("enteredJavaScript", func() { interpreted++ }); err != nil {
						t.Fatal(err)
					}
					bad, err := rt.RunString(`({get value() { enteredJavaScript(); throw "getter failure"; }})`)
					if err != nil {
						t.Fatal(err)
					}
					cause := errors.New("abort inside host callback")
					fail := func() {
						if terminal {
							rt.Abort(cause)
						}
						_ = bad.(*Object).Get("value")
					}
					switch phase {
					case "resumed":
						rt.SetAsyncContextTracker(promiseJobBoundaryTracker{resumed: fail})
					case "exited":
						rt.SetAsyncContextTracker(promiseJobBoundaryTracker{exited: fail})
					case "capability":
						if err := rt.Set("resolveDerived", fail); err != nil {
							t.Fatal(err)
						}
					}
					if err := rt.Set("handler", func() { handled++ }); err != nil {
						t.Fatal(err)
					}
					var jobs []func()
					rt.SetPromiseJobEnqueuer(func(job func()) { jobs = append(jobs, job) })
					script := `Promise.resolve().then(handler);`
					if phase == "capability" {
						script = `
							const p = Promise.resolve();
							p.constructor = {[Symbol.species]: function (executor) {
								executor(resolveDerived, function () {});
							}};
							p.then(handler);
						`
					} else if phase == "enqueuer" {
						script = `
							const derived = Promise.resolve().then(handler);
							derived.then(handler);
							derived.then(handler);
						`
					}
					if _, err := rt.RunString(script); err != nil {
						t.Fatal(err)
					}
					if len(jobs) != 1 {
						t.Fatalf("initial jobs=%d, want 1", len(jobs))
					}
					if phase == "enqueuer" {
						delivered := 0
						rt.SetPromiseJobEnqueuer(func(job func()) {
							delivered++
							jobs = append(jobs, job)
							if delivered == 1 {
								fail()
							}
						})
					}
					invoke := func() {
						before := rt.vm.saveExecutionFrame()
						var gotPanic interface{}
						var gotErr error
						func() {
							defer func() { gotPanic = recover() }()
							gotErr = rt.RunPromiseJob(jobs[0])
						}()
						if terminal {
							if gotPanic != nil {
								t.Errorf("unexpected native panic: %v", gotPanic)
							}
							requireAbortCause(t, gotErr, cause)
						} else {
							exception, ok := gotPanic.(*Exception)
							if !ok || exception.Value().String() != "getter failure" || gotErr != nil {
								t.Errorf("completion=(%v, %v), want propagated catchable panic", gotPanic, gotErr)
							}
						}
						if nested {
							after := rt.vm.saveExecutionFrame()
							if !reflect.DeepEqual(before, after) {
								t.Errorf("callback changed its caller's VM execution frame: before=%+v after=%+v", before, after)
							}
						}
					}
					if nested {
						if err := rt.Set("invokeJob", invoke); err != nil {
							t.Fatal(err)
						}
						value, err := rt.RunString(`function outer() { invokeJob(); return 42; } outer();`)
						if terminal {
							requireAbortCause(t, err, cause)
						} else if err != nil || value.ToInteger() != 42 {
							t.Fatalf("outer execution=(%v, %v), want 42", value, err)
						}
					} else {
						invoke()
					}
					wantHandled := 1
					if phase == "resumed" {
						wantHandled = 0
					}
					wantInterpreted := 1
					if terminal {
						wantInterpreted = 0
					}
					if handled != wantHandled || interpreted != wantInterpreted {
						t.Fatalf("handlers=%d getter=%d, want %d and %d", handled, interpreted, wantHandled, wantInterpreted)
					}
					if phase == "enqueuer" && len(jobs) != 3 {
						t.Fatalf("delivered jobs=%d, want initial job plus both derived reactions", len(jobs))
					}
					assertPromiseBoundaryClean(t, rt)
				})
			}
		}
	}
}

func TestPromiseForwardingDirectAPIRestoresBoundary(t *testing.T) {
	for _, entry := range []string{"leave", "RunPromiseJob"} {
		t.Run(entry, func(t *testing.T) {
			rt := New()
			interpreted := 0
			if err := rt.Set("enteredJavaScript", func() { interpreted++ }); err != nil {
				t.Fatal(err)
			}
			bad, err := rt.RunString(`({get value() { enteredJavaScript(); return 1; }})`)
			if err != nil {
				t.Fatal(err)
			}
			cause := errors.New("abort while forwarding")
			delivered := 0
			hook := func(func()) {
				delivered++
				rt.Abort(cause)
				_ = bad.(*Object).Get("value")
			}
			const pending = `Promise.resolve().then(function () {}); Promise.resolve().then(function () {});`
			if entry == "leave" {
				if err := rt.Set("installHook", func() { rt.SetPromiseJobEnqueuer(hook) }); err != nil {
					t.Fatal(err)
				}
				_, err = rt.RunString(pending + `installHook();`)
			} else {
				var jobs []func()
				rt.SetPromiseJobEnqueuer(func(job func()) { jobs = append(jobs, job) })
				if err := rt.Set("prepare", func() {
					rt.SetPromiseJobEnqueuer(nil)
					if _, err := rt.RunString(pending); err != nil {
						panic(err)
					}
					rt.SetPromiseJobEnqueuer(hook)
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := rt.RunString(`Promise.resolve().then(prepare);`); err != nil {
					t.Fatal(err)
				}
				err = rt.RunPromiseJob(jobs[0])
			}
			requireAbortCause(t, err, cause)
			if delivered != 2 || interpreted != 0 {
				t.Fatalf("deliveries=%d getter executions=%d, want 2 and 0", delivered, interpreted)
			}
			assertPromiseBoundaryClean(t, rt)
		})
	}
}
