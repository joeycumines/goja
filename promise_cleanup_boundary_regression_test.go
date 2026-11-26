package goja

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

type promiseCleanupGrabTracker struct{ grab func() }

func (t promiseCleanupGrabTracker) Grab() interface{} { t.grab(); return nil }
func (promiseCleanupGrabTracker) Resumed(interface{}) {}
func (promiseCleanupGrabTracker) Exited()             {}

func requirePromiseCleanupIdle(t *testing.T, rt *Runtime) {
	t.Helper()
	vm := rt.vm
	if vm.prg != nil || vm.sb != -1 || vm.sp != 0 || len(vm.stack) != 0 || len(vm.callStack) != 0 || len(vm.tryStack) != 0 || len(vm.iterStack) != 0 || len(vm.refStack) != 0 {
		t.Fatalf("dirty VM after cleanup: prg=%v sb=%d sp=%d stack=%d calls=%d tries=%d iters=%d refs=%d", vm.prg, vm.sb, vm.sp, len(vm.stack), len(vm.callStack), len(vm.tryStack), len(vm.iterStack), len(vm.refStack))
	}
}

func TestAbortedPromiseCleanupDirectAPIBoundary(t *testing.T) {
	for _, callback := range []string{"tracker_string", "tracker_getter", "enqueuer_getter", "grab_getter"} {
		for _, nested := range []bool{false, true} {
			name := callback + "/idle"
			if nested {
				name = callback + "/nested"
			}
			t.Run(name, func(t *testing.T) {
				rt := New()
				interpreted, observations, delivered := 0, 0, 0
				if err := rt.Set("enteredJavaScript", func() { interpreted++ }); err != nil {
					t.Fatal(err)
				}
				reason, err := rt.RunString(`({
					toString() { enteredJavaScript(); return "reason"; },
					get message() { enteredJavaScript(); return "reason"; }
				})`)
				if err != nil {
					t.Fatal(err)
				}
				p, _, reject := rt.NewPromise()
				p.ObserveSettlement(func() { observations++ })
				if callback == "enqueuer_getter" || callback == "grab_getter" {
					then, _ := AssertFunction(p.val.Get("then"))
					for range 2 {
						if _, err := then(p.val, Undefined(), Undefined()); err != nil {
							t.Fatal(err)
						}
					}
					if callback == "grab_getter" {
						rt.SetAsyncContextTracker(promiseCleanupGrabTracker{grab: func() { _ = reason.(*Object).Get("message") }})
					}
					rt.SetPromiseJobEnqueuer(func(func()) {
						delivered++
						if delivered == 1 {
							if callback == "grab_getter" {
								if _, err := then(p.val); err != nil {
									panic(err)
								}
							} else {
								_ = reason.(*Object).Get("message")
							}
						}
					})
				} else {
					rt.SetPromiseRejectionTracker(func(p *Promise, operation PromiseRejectionOperation) {
						if operation != PromiseRejectionReject {
							return
						}
						if callback == "tracker_string" {
							_ = p.Result().String()
						} else {
							_ = p.Result().(*Object).Get("message")
						}
					})
				}
				cause := errors.New("terminal cancellation")
				if nested {
					if err := rt.Set("dispose", func() {
						rt.Abort(cause)
						vm := rt.vm
						before := context{}
						vm.saveCtx(&before)
						sp, calls, tries := vm.sp, len(vm.callStack), len(vm.tryStack)
						requireAbortCause(t, reject(reason), cause)
						after := context{}
						vm.saveCtx(&after)
						if !reflect.DeepEqual(before, after) || vm.sp != sp || len(vm.callStack) != calls || len(vm.tryStack) != tries {
							t.Errorf("terminal cleanup changed its caller's VM context: sp=%d/%d calls=%d/%d tries=%d/%d", vm.sp, sp, len(vm.callStack), calls, len(vm.tryStack), tries)
						}
					}); err != nil {
						t.Fatal(err)
					}
					_, err = rt.RunString(`function outer() { dispose(); } outer();`)
					requireAbortCause(t, err, cause)
				} else {
					rt.Abort(cause)
					requireAbortCause(t, reject(reason), cause)
				}
				if p.State() != PromiseStateRejected || p.Result() != reason || observations != 1 || interpreted != 0 {
					t.Fatalf("cleanup outcome: state=%v observations=%d interpreted=%d", p.State(), observations, interpreted)
				}
				if (callback == "enqueuer_getter" || callback == "grab_getter") && delivered != 2 {
					t.Fatalf("enqueuer deliveries=%d, want 2", delivered)
				}
				requirePromiseCleanupIdle(t, rt)
			})
		}
	}
}

func TestNativePromiseArgumentConversionReturnsException(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		for _, resolving := range []bool{false, true} {
			name := "reject"
			if resolving {
				name = "resolve"
			}
			if terminal {
				name += "/aborted"
			} else {
				name += "/ordinary"
			}
			t.Run(name, func(t *testing.T) {
				rt := New()
				p, resolve, reject := rt.NewPromise()
				settle := reject
				if resolving {
					settle = resolve
				}
				if terminal {
					rt.Abort(errors.New("stop"))
				}
				err := settle(New().NewObject())
				var exception *Exception
				if !errors.As(err, &exception) || !strings.Contains(exception.Error(), "Illegal runtime transition") {
					t.Fatalf("conversion error=%v, want JavaScript TypeError", err)
				}
				if p.State() != PromiseStatePending {
					t.Fatal("failed conversion claimed resolving pair")
				}
				if err := reject("valid cleanup"); err != nil {
					t.Fatal(err)
				}
				if p.State() != PromiseStateRejected || p.Result().String() != "valid cleanup" {
					t.Fatal("promise could not settle after failed conversion")
				}
				requirePromiseCleanupIdle(t, rt)
			})
		}
	}
}

func TestAbortedPromiseTrackerCatchableException(t *testing.T) {
	rt := New()
	reason, err := rt.RunString(`Symbol("reason")`)
	if err != nil {
		t.Fatal(err)
	}
	p, _, reject := rt.NewPromise()
	observed := 0
	p.ObserveSettlement(func() { observed++ })
	rt.SetPromiseRejectionTracker(func(p *Promise, operation PromiseRejectionOperation) {
		if operation == PromiseRejectionReject {
			_ = p.Result().ToInteger()
		}
	})
	rt.Abort(errors.New("stop"))
	err = reject(reason)
	var exception *Exception
	if !errors.As(err, &exception) {
		t.Fatalf("tracker error=%v, want *Exception", err)
	}
	if p.State() != PromiseStateRejected || p.Result() != reason || observed != 1 {
		t.Fatalf("state=%v observed=%d", p.State(), observed)
	}
	requirePromiseCleanupIdle(t, rt)
}

func TestPromiseReactionEnqueuerPanicPreservesDueJobsAndFirstFailure(t *testing.T) {
	for _, rejecting := range []bool{false, true} {
		for _, terminal := range []bool{false, true} {
			name := "fulfill"
			if rejecting {
				name = "reject"
			}
			if terminal {
				name += "/aborted"
			} else {
				name += "/ordinary"
			}
			t.Run(name, func(t *testing.T) {
				rt := New()
				p, resolve, reject := rt.NewPromise()
				settle := resolve
				want := PromiseStateFulfilled
				if rejecting {
					settle, want = reject, PromiseStateRejected
				}
				then, _ := AssertFunction(p.val.Get("then"))
				for range 3 {
					if _, err := then(p.val, Undefined(), Undefined()); err != nil {
						t.Fatal(err)
					}
				}
				first := errors.New("first enqueuer failure")
				var jobs []func()
				rt.SetPromiseJobEnqueuer(func(job func()) {
					jobs = append(jobs, job)
					if len(jobs) == 1 {
						panic(first)
					}
				})
				observed, later := 0, false
				p.ObserveSettlement(func() {
					observed++
					if got := len(jobs) + len(rt.jobQueue); got != 3 {
						t.Errorf("due jobs scheduled at observer=%d, want 3", got)
					}
					panic("later observer failure")
				})
				p.ObserveSettlement(func() { later = true })
				if terminal {
					rt.Abort(errors.New("stop"))
				}
				func() {
					defer func() {
						if got := recover(); got != first {
							t.Errorf("propagated failure=%v, want original enqueuer failure", got)
						}
					}()
					_ = settle("result")
					t.Error("enqueuer failure did not propagate")
				}()
				if p.State() != want || p.Result().String() != "result" || observed != 1 || later || len(p.settlementObservers) != 0 {
					t.Fatalf("state=%v observed=%d later=%v retained=%d", p.State(), observed, later, len(p.settlementObservers))
				}
				if len(jobs) != 3 || len(rt.jobQueue) != 0 {
					t.Fatalf("delivered=%d queued=%d", len(jobs), len(rt.jobQueue))
				}
				requirePromiseCleanupIdle(t, rt)
			})
		}
	}
}

func TestPromiseObserverRegistrationDuringSettlementHooks(t *testing.T) {
	for _, hook := range []string{"tracker", "enqueuer"} {
		t.Run(hook, func(t *testing.T) {
			rt := New()
			p, _, reject := rt.NewPromise()
			var order []string
			p.ObserveSettlement(func() { order = append(order, "pending-first") })
			p.ObserveSettlement(func() { order = append(order, "pending-second") })
			register := func() { p.ObserveSettlement(func() { order = append(order, "hook") }) }
			if hook == "tracker" {
				rt.SetPromiseRejectionTracker(func(*Promise, PromiseRejectionOperation) { register() })
			} else {
				then, _ := AssertFunction(p.val.Get("then"))
				if _, err := then(p.val, Undefined(), Undefined()); err != nil {
					t.Fatal(err)
				}
				rt.SetPromiseJobEnqueuer(func(func()) { register() })
			}
			if err := reject("done"); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(order, ","); got != "hook,pending-first,pending-second" {
				t.Fatalf("observer order=%s", got)
			}
		})
	}
}

func TestAbortedPromiseQueueCleanupHookRestoresVM(t *testing.T) {
	rt := New()
	obj, err := rt.RunString(`({get value() { throw new Error("must not run"); }})`)
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("abort")
	delivered := 0
	if err := rt.Set("installAndAbort", func() {
		rt.SetPromiseJobEnqueuer(func(func()) {
			delivered++
			if delivered == 1 {
				_ = obj.(*Object).Get("value")
			} else if rt.vm.sp != 0 || len(rt.vm.callStack) != 0 || len(rt.vm.tryStack) != 0 {
				t.Errorf("later hook inherited abandoned interpreter state: sp=%d calls=%d tries=%d", rt.vm.sp, len(rt.vm.callStack), len(rt.vm.tryStack))
			}
		})
		rt.Abort(cause)
	}); err != nil {
		t.Fatal(err)
	}
	_, err = rt.RunString(`Promise.resolve().then(function () {}); Promise.resolve().then(function () {}); installAndAbort();`)
	requireAbortCause(t, err, cause)
	if delivered != 2 {
		t.Fatalf("delivered=%d, want 2", delivered)
	}
	requirePromiseCleanupIdle(t, rt)
}
