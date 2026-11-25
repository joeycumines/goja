package goja

import (
	"errors"
	"testing"
)

func attachAbortReaction(rt *Runtime, promise *Object, reached *int, scenario string) {
	switch scenario {
	case "native":
		if err := rt.Set("nativeReaction", func() { *reached++ }); err != nil {
			panic(err)
		}
		handler := rt.Get("nativeReaction")
		then := promiseThenForAbortTest(promise)
		if _, err := then(promise, _undefined, handler); err != nil {
			panic(err)
		}
	case "nil":
		then := promiseThenForAbortTest(promise)
		if _, err := then(promise, _undefined, _undefined); err != nil {
			panic(err)
		}
	case "js":
		if err := rt.Set("jsReactionCalls", func(Value) { *reached++ }); err != nil {
			panic(err)
		}
		if err := rt.Set("p", promise); err != nil {
			panic(err)
		}
		if _, err := rt.RunString(`p.then(undefined, function (reason) { jsReactionCalls(reason); })`); err != nil {
			panic(err)
		}
	default:
		panic("unknown scenario")
	}
}

func promiseThenForAbortTest(promise *Object) Callable {
	then, ok := AssertFunction(promise.Get("then"))
	if !ok {
		panic("Promise.prototype.then is not callable")
	}
	return then
}

func TestAbortTerminalRejectionDoesNotRunInternalReactionJobs(t *testing.T) {
	for _, scenario := range []string{"native", "nil", "js"} {
		t.Run(scenario, func(t *testing.T) {
			rt := New()
			p, _, reject := rt.NewPromise()
			reached := 0
			attachAbortReaction(rt, p.val, &reached, scenario)

			rt.Abort(errors.New("host abort"))
			if err := reject(errors.New("terminal reason")); err != nil {
				t.Fatalf("terminal reject returned %v", err)
			}
			if p.State() != PromiseStateRejected {
				t.Fatalf("state = %v, want rejected", p.State())
			}
			if reached != 0 {
				t.Fatalf("reaction ran during terminal rejection: %d calls", reached)
			}
		})
	}
}

func TestAbortTerminalRejectionDefersJobsToHook(t *testing.T) {
	for _, scenario := range []string{"native", "nil", "js"} {
		t.Run(scenario, func(t *testing.T) {
			rt := New()
			var jobs []func()
			rt.SetPromiseJobEnqueuer(func(job func()) { jobs = append(jobs, job) })
			p, _, reject := rt.NewPromise()
			reached := 0
			attachAbortReaction(rt, p.val, &reached, scenario)

			cause := errors.New("host abort")
			rt.Abort(cause)
			if err := reject(errors.New("terminal reason")); err != nil {
				t.Fatalf("terminal reject returned %v", err)
			}
			if reached != 0 {
				t.Fatalf("reaction ran during terminal rejection: %d calls", reached)
			}
			if len(jobs) != 1 {
				t.Fatalf("captured jobs = %d, want 1", len(jobs))
			}
			for _, job := range jobs {
				err := rt.RunPromiseJob(job)
				var interrupted *InterruptedError
				if !errors.As(err, &interrupted) || interrupted.Value() != cause {
					t.Fatalf("aborted job error = %v, want abort cause %v", err, cause)
				}
			}
			if reached != 0 {
				t.Fatalf("reaction ran through RunPromiseJob after abort: %d calls", reached)
			}
		})
	}
}

func TestAbortNativeErrorReturnDoesNotDrainQueuedJobs(t *testing.T) {
	for _, withHook := range []bool{false, true} {
		name := "internal queue"
		if withHook {
			name = "host queue"
		}
		t.Run(name, func(t *testing.T) {
			rt := New()
			reached := 0
			if withHook {
				rt.SetPromiseJobEnqueuer(func(func()) {})
			}
			if err := rt.Set("nativeReaction", func() { reached++ }); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("native failure")
			if err := rt.Set("nativeAbort", func() error {
				rt.Abort(errors.New("host abort"))
				return failure
			}); err != nil {
				t.Fatal(err)
			}
			_, err := rt.RunString(`
				Promise.resolve().then(nativeReaction);
				nativeAbort();
			`)
			if !errors.Is(err, failure) {
				t.Fatalf("current call error = %v, want native failure %v", err, failure)
			}
			if reached != 0 {
				t.Fatalf("queued native reaction ran after abort: %d calls", reached)
			}
		})
	}
}

type noopAsyncContextTracker struct{}

func (noopAsyncContextTracker) Grab() interface{}   { return "ctx" }
func (noopAsyncContextTracker) Resumed(interface{}) {}
func (noopAsyncContextTracker) Exited()             {}

func TestAttachAbortReactionBaselineWithoutAbort(t *testing.T) {
	for _, scenario := range []string{"native", "js"} {
		t.Run(scenario, func(t *testing.T) {
			rt := New()
			p, _, reject := rt.NewPromise()
			reached := 0
			attachAbortReaction(rt, p.val, &reached, scenario)
			if err := reject(errors.New("ordinary rejection")); err != nil {
				t.Fatalf("ordinary reject returned %v", err)
			}
			if reached != 1 {
				t.Fatalf("reaction calls = %d, want 1", reached)
			}
		})
	}
}

func TestAbortDuringLeaveStopsRemainingQueuedJobs(t *testing.T) {
	t.Run("internal_queue", func(t *testing.T) {
		rt := New()
		cause := errors.New("abort inside queued reaction")
		ranAfterAbort := 0
		if err := rt.Set("afterAbort", func() { ranAfterAbort++ }); err != nil {
			t.Fatal(err)
		}
		if err := rt.Set("abortNow", func() error {
			rt.Abort(cause)
			return errors.New("cooperative stop")
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := rt.RunString(`
			const p = Promise.resolve();
			p.then(abortNow).catch(afterAbort);
			p.then(afterAbort);
			p.then().then(afterAbort);
		`); err != nil {
			t.Fatalf("top-level run failed: %v", err)
		}
		if ranAfterAbort != 0 {
			t.Fatalf("reactions executed after mid-leave abort: %d", ranAfterAbort)
		}
		if len(rt.jobQueue) != 0 {
			t.Fatalf("internal jobQueue retained %d dead jobs", len(rt.jobQueue))
		}
	})

	t.Run("mid_leave_hook_install", func(t *testing.T) {
		rt := New()
		cause := errors.New("abort after installing hook")
		ranAfterAbort := 0
		var jobs []func()
		if err := rt.Set("afterAbort", func() { ranAfterAbort++ }); err != nil {
			t.Fatal(err)
		}
		p, _, reject := rt.NewPromise()
		attachAbortReaction(rt, p.val, &ranAfterAbort, "native")
		if err := rt.Set("installAndAbort", func() {
			rt.SetPromiseJobEnqueuer(func(job func()) { jobs = append(jobs, job) })
			rt.Abort(cause)
			if err := reject(errors.New("disposed")); err != nil {
				t.Errorf("reject failed: %v", err)
			}
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := rt.RunString(`
			Promise.resolve().then(installAndAbort);
			Promise.resolve().then(afterAbort);
		`); err != nil {
			t.Fatalf("top-level run failed: %v", err)
		}
		if ranAfterAbort != 0 {
			t.Fatalf("reactions ran directly after abort: %d", ranAfterAbort)
		}
		if len(jobs) != 2 {
			t.Fatalf("forwarded jobs = %d, want 2", len(jobs))
		}
		for _, job := range jobs {
			requireAbortCause(t, rt.RunPromiseJob(job), cause)
		}
	})
}

func TestAbortUnwindPreservesEnqueuerAfterTrackerPriority(t *testing.T) {
	rt := New()
	rt.SetAsyncContextTracker(noopAsyncContextTracker{})
	cause := errors.New("tracker priority abort")
	var jobs []func()
	runInline := true
	rt.SetPromiseJobEnqueuer(func(job func()) {
		if runInline {
			_ = rt.RunPromiseJob(job)
			return
		}
		jobs = append(jobs, job)
	})
	reached := 0
	if err := rt.Set("neverRuns", func() { reached++ }); err != nil {
		t.Fatal(err)
	}
	if err := rt.Set("triggerAbort", func() {
		runInline = false
		rt.Abort(cause)
	}); err != nil {
		t.Fatal(err)
	}
	_, err := rt.RunString(`
		Promise.resolve().then(function () {
			Promise.resolve().then(neverRuns);
			triggerAbort();
			for (;;) {}
		});
		for (;;) {}
	`)
	requireAbortCause(t, err, cause)
	if len(jobs) != 1 {
		t.Fatalf("unwind forwarded jobs = %d, want 1", len(jobs))
	}
	p, _, reject := rt.NewPromise()
	attachAbortReaction(rt, p.val, &reached, "native")
	if err := reject(errors.New("post-abort disposal")); err != nil {
		t.Fatalf("post-abort reject failed: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("total jobs after terminal disposal = %d, want 2 (priority stuck=%v)", len(jobs), rt.promiseJobQueuePriority)
	}
	for _, job := range jobs {
		requireAbortCause(t, rt.RunPromiseJob(job), cause)
	}
	if reached != 0 {
		t.Fatalf("reactions executed after abort: %d", reached)
	}
}

func TestAbortTerminalRejectionQueueAndObserverSafety(t *testing.T) {
	t.Run("no_enqueuer_does_not_leak_job_queue", func(t *testing.T) {
		rt := New()
		p, _, reject := rt.NewPromise()
		reached := 0
		attachAbortReaction(rt, p.val, &reached, "native")
		attachAbortReaction(rt, p.val, &reached, "nil")
		attachAbortReaction(rt, p.val, &reached, "js")
		rt.Abort(errors.New("host abort"))
		if err := reject(errors.New("terminal disposal")); err != nil {
			t.Fatalf("reject returned %v", err)
		}
		if reached != 0 {
			t.Fatalf("reactions ran = %d, want 0", reached)
		}
		if len(rt.jobQueue) != 0 {
			t.Fatalf("jobQueue retained %d dead jobs without enqueuer", len(rt.jobQueue))
		}
	})

	t.Run("observer_notified_when_tracker_aborts", func(t *testing.T) {
		rt := New()
		cause := errors.New("host abort")
		rt.SetPromiseRejectionTracker(func(*Promise, PromiseRejectionOperation) {
			if _, err := rt.RunString(`1 + 1`); err != nil {
				panic(err)
			}
		})
		p, _, reject := rt.NewPromise()
		observed := 0
		p.ObserveSettlement(func() {
			observed++
		})
		rt.Abort(cause)
		err := reject(errors.New("terminal disposal"))
		requireAbortCause(t, err, cause)
		if p.State() != PromiseStateRejected {
			t.Fatalf("state = %v, want rejected", p.State())
		}
		if observed != 1 {
			t.Fatalf("observed = %d, want 1", observed)
		}
		if len(p.settlementObservers) != 0 {
			t.Fatalf("settlementObservers retained = %d, want 0", len(p.settlementObservers))
		}
	})
}
