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
		if _, err := rt.RunString(`p.then(function (reason) { jsReactionCalls(reason); })`); err != nil {
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
