package goja

import (
	"errors"
	"testing"
)

type promiseIdleQueueTracker struct{}

func (promiseIdleQueueTracker) Grab() interface{}   { return nil }
func (promiseIdleQueueTracker) Resumed(interface{}) {}
func (promiseIdleQueueTracker) Exited()             {}

func TestAbortedPromiseCleanupFlushesDeferredQueue(t *testing.T) {
	runPromiseCleanupDeferredQueue(t, true)
}

func TestInterruptedPromiseCleanupFlushesDeferredQueue(t *testing.T) {
	runPromiseCleanupDeferredQueue(t, false)
}

func runPromiseCleanupDeferredQueue(t *testing.T, terminal bool) {
	t.Helper()
	rt := New()
	rt.SetAsyncContextTracker(promiseIdleQueueTracker{})
	var jobs []func()
	rt.SetPromiseJobEnqueuer(func(job func()) { jobs = append(jobs, job) })
	p, _, reject := rt.NewPromise()
	ran := 0
	if err := rt.Set("mark", func() { ran++ }); err != nil {
		t.Fatal(err)
	}
	if err := rt.Set("p", p); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RunString(`
		p.catch(mark);
		Promise.resolve().then(function () {
			Promise.resolve().then(mark);
		});
	`); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("root jobs=%d", len(jobs))
	}
	rootJob := jobs[0]
	jobs = nil
	rt.SetPromiseJobEnqueuer(nil)
	if err := rt.RunPromiseJob(rootJob); err != nil {
		t.Fatal(err)
	}
	if len(rt.jobQueue) != 1 || !rt.promiseJobQueuePriority {
		t.Fatalf("setup did not leave deferred jobs: jobs=%d priority=%v", len(rt.jobQueue), rt.promiseJobQueuePriority)
	}
	rt.SetPromiseJobEnqueuer(func(job func()) { jobs = append(jobs, job) })
	cause := errors.New("stop")
	if terminal {
		rt.Abort(cause)
	} else {
		rt.Interrupt(cause)
	}
	if err := reject("terminal disposal"); err != nil {
		t.Fatal(err)
	}
	if p.State() != PromiseStateRejected {
		t.Fatal("promise did not reject")
	}
	if len(jobs) != 2 || len(rt.jobQueue) != 0 || rt.promiseJobQueuePriority {
		t.Fatalf("rejection did not hand off queued jobs: delivered=%d stranded=%d priority=%v", len(jobs), len(rt.jobQueue), rt.promiseJobQueuePriority)
	}
	if terminal {
		for _, job := range jobs {
			requireAbortCause(t, rt.RunPromiseJob(job), cause)
		}
	}
	if ran != 0 {
		t.Fatalf("native reaction executed after cancellation: %d", ran)
	}
}
