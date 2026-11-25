package goja

import (
	"errors"
	"reflect"
	"testing"
)

func assertPromiseBoundaryClean(t *testing.T, rt *Runtime) {
	t.Helper()
	if rt.vm.prg != nil || rt.vm.sb != -1 || rt.vm.sp != 0 || len(rt.vm.stack) != 0 ||
		len(rt.vm.callStack) != 0 || len(rt.vm.tryStack) != 0 || len(rt.vm.iterStack) != 0 || len(rt.vm.refStack) != 0 ||
		len(rt.jobQueue) != 0 || rt.promiseJobQueuePriority || rt.asyncTrackerActive || rt.leavingAbrupt {
		t.Fatalf("dirty native boundary: prg=%v sb=%d sp=%d stack=%d call=%d try=%d iter=%d ref=%d jobs=%d priority=%v tracker=%v leaving=%v",
			rt.vm.prg, rt.vm.sb, rt.vm.sp, len(rt.vm.stack), len(rt.vm.callStack), len(rt.vm.tryStack),
			len(rt.vm.iterStack), len(rt.vm.refStack), len(rt.jobQueue), rt.promiseJobQueuePriority,
			rt.asyncTrackerActive, rt.leavingAbrupt)
	}
}

func TestPromiseQueueHookPanicPreservesUndeliveredJobs(t *testing.T) {
	for _, entry := range []string{"leave", "RunPromiseJob"} {
		for _, failure := range []string{"native_panic", "interrupt", "abort"} {
			t.Run(entry+"/"+failure, func(t *testing.T) {
				rt := New()
				primary := errors.New("first enqueuer failure")
				secondary := errors.New("later cleanup failure")
				var delivered []func()
				var ran []int
				if err := rt.Set("record", func(n int) { ran = append(ran, n) }); err != nil {
					t.Fatal(err)
				}
				extra, resolveExtra, _ := rt.NewPromise()
				then, _ := AssertFunction(extra.val.Get("then"))
				if _, err := then(extra.val, rt.ToValue(func() { ran = append(ran, 4) })); err != nil {
					t.Fatal(err)
				}
				hook := func(job func()) {
					delivered = append(delivered, job)
					switch len(delivered) {
					case 1:
						// New work from the failed delivery remains behind the
						// older, still-undelivered jobs.
						if err := resolveExtra(nil); err != nil {
							panic(err)
						}
						switch failure {
						case "native_panic":
							panic(primary)
						case "interrupt":
							rt.Interrupt(primary)
						case "abort":
							rt.Abort(primary)
						}
						rt.vm.checkInterrupt()
					case 2:
						panic(secondary)
					}
				}
				const pending = `
					Promise.resolve().then(function () { record(1); });
					Promise.resolve().then(function () { record(2); });
					Promise.resolve().then(function () { record(3); });
				`
				var run func() error
				if entry == "leave" {
					if err := rt.Set("installHook", func() { rt.SetPromiseJobEnqueuer(hook) }); err != nil {
						t.Fatal(err)
					}
					run = func() error { _, err := rt.RunString(pending + `installHook();`); return err }
				} else {
					var initial []func()
					rt.SetPromiseJobEnqueuer(func(job func()) { initial = append(initial, job) })
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
					if len(initial) != 1 {
						t.Fatalf("initial jobs = %d, want 1", len(initial))
					}
					run = func() error { return rt.RunPromiseJob(initial[0]) }
				}

				var gotPanic interface{}
				var gotErr error
				func() {
					defer func() { gotPanic = recover() }()
					gotErr = run()
				}()
				if failure == "native_panic" {
					if gotPanic != primary || gotErr != nil {
						t.Fatalf("completion = (%v, %v), want original native panic", gotPanic, gotErr)
					}
				} else {
					if gotPanic != nil {
						t.Fatalf("unexpected panic: %v", gotPanic)
					}
					requireAbortCause(t, gotErr, primary)
				}
				if len(delivered) != 4 || len(ran) != 0 {
					t.Fatalf("delivery=%d execution=%v, want four delivered jobs and no execution", len(delivered), ran)
				}
				assertPromiseBoundaryClean(t, rt)
				rt.SetPromiseJobEnqueuer(func(job func()) { delivered = append(delivered, job) })
				for _, job := range delivered {
					err := rt.RunPromiseJob(job)
					if failure == "abort" {
						requireAbortCause(t, err, primary)
					} else if err != nil {
						t.Fatal(err)
					}
				}
				if failure == "abort" {
					if len(ran) != 0 {
						t.Fatalf("jobs executed after Abort: %v", ran)
					}
				} else if !reflect.DeepEqual(ran, []int{1, 2, 3, 4}) {
					t.Fatalf("execution order=%v, want [1 2 3 4]", ran)
				}
				assertPromiseBoundaryClean(t, rt)
			})
		}
	}
}

func TestPromiseQueueAbortCleanupReentryPreservesJobs(t *testing.T) {
	for _, abort := range []bool{false, true} {
		name := "interrupt"
		if abort {
			name = "abort"
		}
		t.Run(name, func(t *testing.T) {
			rt := New()
			rt.SetAsyncContextTracker(noopAsyncContextTracker{})
			cleanupPromise, _, cleanupReject := rt.NewPromise()
			ran := 0
			then, _ := AssertFunction(cleanupPromise.val.Get("then"))
			if _, err := then(cleanupPromise.val, Undefined(), rt.ToValue(func() { ran++ })); err != nil {
				t.Fatal(err)
			}

			cause := errors.New("stop while tracking a reaction")
			initial := true
			deliveredDuringCleanup := 0
			rt.SetPromiseJobEnqueuer(func(job func()) {
				if initial {
					_ = rt.RunPromiseJob(job)
					return
				}
				deliveredDuringCleanup++
				if err := cleanupReject("disposed"); err != nil {
					t.Errorf("cleanup: %v", err)
				}
				requireAbortCause(t, rt.RunPromiseJob(job), cause)
			})
			if err := rt.Set("stopNow", func() {
				initial = false
				if abort {
					rt.Abort(cause)
				} else {
					rt.Interrupt(cause)
				}
			}); err != nil {
				t.Fatal(err)
			}
			_, err := rt.RunString(`
				Promise.resolve().then(function () {
					Promise.resolve().then(function () {});
					stopNow();
					for (;;) {}
				});
				for (;;) {}
			`)
			requireAbortCause(t, err, cause)
			if cleanupPromise.State() != PromiseStateRejected || deliveredDuringCleanup != 2 || ran != 0 {
				t.Fatalf("state=%v delivered=%d executed=%d, want rejected and two undrained deliveries", cleanupPromise.State(), deliveredDuringCleanup, ran)
			}
			assertPromiseBoundaryClean(t, rt)
		})
	}
}

func TestPromiseQueueNativePanicAfterInstallingHook(t *testing.T) {
	rt := New()
	primary := errors.New("native reaction failure")
	var delivered []func()
	var ran []int
	if err := rt.Set("record", func(n int) { ran = append(ran, n) }); err != nil {
		t.Fatal(err)
	}
	if err := rt.Set("installAndPanic", func() {
		rt.SetPromiseJobEnqueuer(func(job func()) { delivered = append(delivered, job) })
		if _, err := rt.RunString(`Promise.resolve().then(function () { record(4); });`); err != nil {
			panic(err)
		}
		panic(primary)
	}); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if got := recover(); got != primary {
				t.Fatalf("panic=%v, want original failure", got)
			}
		}()
		_, _ = rt.RunString(`
			const p = Promise.resolve();
			p.then(installAndPanic);
			p.then(function () { record(2); });
			p.then(function () { record(3); });
		`)
	}()
	if len(delivered) != 3 || len(ran) != 0 {
		t.Fatalf("delivery=%d execution=%v, want all unattempted jobs delivered", len(delivered), ran)
	}
	assertPromiseBoundaryClean(t, rt)
	for _, job := range delivered {
		if err := rt.RunPromiseJob(job); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(ran, []int{2, 3, 4}) {
		t.Fatalf("execution order=%v, want [2 3 4]", ran)
	}
	assertPromiseBoundaryClean(t, rt)
}

func TestPromiseQueueNativePanicWithoutHookDropsPendingJobs(t *testing.T) {
	rt := New()
	primary := errors.New("native reaction failure")
	ran := false
	if err := rt.Set("panicNow", func() { panic(primary) }); err != nil {
		t.Fatal(err)
	}
	if err := rt.Set("record", func() { ran = true }); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if got := recover(); got != primary {
				t.Fatalf("panic=%v, want original failure", got)
			}
		}()
		_, _ = rt.RunString(`Promise.resolve().then(panicNow); Promise.resolve().then(record);`)
	}()
	assertPromiseBoundaryClean(t, rt)
	if _, err := rt.RunString(`21 + 21`); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("abandoned internal reaction ran in a later turn")
	}
}

func TestPromiseQueueObserverPanicRunPromiseJobCleansVM(t *testing.T) {
	rt := New()
	var jobs []func()
	rt.SetPromiseJobEnqueuer(func(job func()) { jobs = append(jobs, job) })
	p, resolve, _ := rt.NewPromise()
	p.ObserveSettlement(func() { panic("observer failed") })
	if err := rt.Set("settle", func() { _ = resolve("done") }); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RunString(`Promise.resolve().then(function () { settle(); });`); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if got := recover(); got != "observer failed" {
				t.Fatalf("panic=%v, want observer failure", got)
			}
		}()
		_ = rt.RunPromiseJob(jobs[0])
	}()
	assertPromiseBoundaryClean(t, rt)
	value, err := rt.RunString(`21 + 21`)
	if err != nil || value.ToInteger() != 42 {
		t.Fatalf("runtime not reusable: value=%v error=%v", value, err)
	}
}

func TestRunProgramStackOverflowBeforeRecursiveEntryKeepsOuterFrame(t *testing.T) {
	rt := New()
	var innerErr error
	if err := rt.Set("native", func() {
		previous := rt.vm.maxCallStackSize
		rt.SetMaxCallStackSize(0)
		defer rt.SetMaxCallStackSize(previous)
		_, innerErr = rt.RunString(`42`)
	}); err != nil {
		t.Fatal(err)
	}
	var value Value
	var err error
	func() {
		defer func() {
			if got := recover(); got != nil {
				t.Fatalf("nested entry failure damaged outer execution: %v", got)
			}
		}()
		value, err = rt.RunString(`native(); 21 + 21;`)
	}()
	var overflow *StackOverflowError
	if !errors.As(innerErr, &overflow) || err != nil || value.ToInteger() != 42 {
		t.Fatalf("inner=%v outer=(%v,%v), want handled inner overflow and outer result 42", innerErr, value, err)
	}
	assertPromiseBoundaryClean(t, rt)
}
