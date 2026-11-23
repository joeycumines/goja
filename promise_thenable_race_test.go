package goja

import (
	"errors"
	"testing"
	"time"
)

func TestAbortTerminalRejectionDoesNotEnqueueStaleThenableJob(t *testing.T) {
	rt := New()
	var jobs []func()
	rt.SetPromiseJobEnqueuer(func(job func()) { jobs = append(jobs, job) })

	callableThen := rt.NewObject()
	if err := callableThen.Set("call", func(FunctionCall) Value { return Undefined() }); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("host abort")
	promise, resolve, reject := rt.NewPromise()

	// The initially installed `then` getter terminally rejects the SAME
	// promise whose resolve function is adopted below.
	thenable := rt.NewObject()
	if err := thenable.Set("then", func(FunctionCall) Value {
		panic("not used")
	}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Set("thenGetter", func(FunctionCall) Value {
		rt.Abort(cause)
		if err := reject(errors.New("terminal disposal")); err != nil {
			t.Errorf("terminal reject returned %v", err)
		}
		return callableThen.Get("call")
	}); err != nil {
		t.Fatal(err)
	}
	if !thenable.defineOwnProperty(asciiString("then"), PropertyDescriptor{
		Getter:       rt.Get("thenGetter"),
		Configurable: FLAG_TRUE,
	}, true) {
		t.Fatal("could not install then getter")
	}

	hooked := make(chan struct{})
	release := make(chan struct{})
	resolveErr := make(chan error, 1)
	if err := rt.Set("performResolve", func() {
		close(hooked)
		<-release
		resolveErr <- resolve(thenable)
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := rt.RunString(`performResolve()`)
		done <- err
	}()
	select {
	case <-hooked:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("native resolution did not start")
	}
	close(release)
	select {
	case err := <-done:
		requireAbortCause(t, err, cause)
	case <-time.After(5 * time.Second):
		t.Fatal("resolution did not return after abort")
	}
	if err := <-resolveErr; err != nil {
		t.Fatalf("resolve returned %v", err)
	}
	if promise.State() != PromiseStateRejected || promise.Result().String() != "terminal disposal" {
		t.Fatalf("terminal settlement = (%v, %v)", promise.State(), promise.Result())
	}
	if len(jobs) != 0 {
		t.Fatalf("stale thenable jobs = %d, want 0", len(jobs))
	}
	if err := reject(errors.New("second rejection")); err != nil {
		t.Fatalf("second terminal reject returned %v", err)
	}
	if promise.Result().String() != "terminal disposal" || len(jobs) != 0 {
		t.Fatalf("late settle changed outcome: result=%v jobs=%d", promise.Result(), len(jobs))
	}
}
