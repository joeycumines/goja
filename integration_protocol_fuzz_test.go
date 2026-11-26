package goja

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// The oracle records only the public settlement contract, independently of the
// resolving closures and VM's internal queue representation. Corpus traces are
// bounded for fuzz execution; the accompanying Lean model is unbounded.
func FuzzIntegrationPromiseStateMachine(f *testing.F) {
	for _, seed := range [][]byte{{}, {2, 8, 5, 1, 3, 7}, {7, 2, 8, 3, 1}, {2, 5, 1, 8}, {5, 0, 1, 7}, {7, 7, 1, 7}} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 256 {
			data = data[:256]
		}
		r := New()
		p, resolve, reject := r.NewPromise()
		var jobs []func()
		r.SetPromiseJobEnqueuer(func(j func()) { jobs = append(jobs, j) })
		var innerResolve, innerReject Callable
		thenable := r.NewObject()
		if err := thenable.Set("then", func(c FunctionCall) Value {
			innerResolve, _ = AssertFunction(c.Argument(0))
			innerReject, _ = AssertFunction(c.Argument(1))
			return Undefined()
		}); err != nil {
			t.Fatal(err)
		}
		state := PromiseStatePending
		result := ""
		outerClaimed, innerClaimed, aborted := false, false, false
		registered, observed := 0, 0
		settle := func(next PromiseState, v string) {
			if state == PromiseStatePending {
				state = next
				result = v
			}
		}
		for i, b := range data {
			v := fmt.Sprintf("result-%d", i)
			switch b % 9 {
			case 0:
				if !outerClaimed && state == PromiseStatePending {
					outerClaimed = true
					settle(PromiseStateFulfilled, v)
				}
				if err := resolve(v); err != nil {
					t.Fatal(err)
				}
			case 1:
				if state == PromiseStatePending && (aborted || !outerClaimed) {
					outerClaimed = true
					settle(PromiseStateRejected, v)
				}
				if err := reject(v); err != nil {
					t.Fatal(err)
				}
			case 2:
				if !outerClaimed && state == PromiseStatePending {
					outerClaimed = true
				}
				if err := resolve(thenable); err != nil {
					t.Fatal(err)
				}
			case 3, 4:
				if innerResolve != nil {
					fn := innerResolve
					next := PromiseStateFulfilled
					if b%9 == 4 {
						fn = innerReject
						next = PromiseStateRejected
					}
					if !innerClaimed && state == PromiseStatePending {
						innerClaimed = true
						settle(next, v)
					}
					if _, err := fn(Undefined(), r.ToValue(v)); err != nil {
						t.Fatal(err)
					}
				}
			case 5:
				aborted = true
				r.Abort("stop")
			case 6:
				r.ClearInterrupt()
			case 7:
				registered++
				p.ObserveSettlement(func() { observed++ })
			case 8:
				if len(jobs) > 0 {
					j := jobs[0]
					jobs = jobs[1:]
					err := r.RunPromiseJob(j)
					if aborted {
						var ex *InterruptedError
						if !errors.As(err, &ex) {
							t.Fatalf("job error=%v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
				}
			}
			if p.State() != state {
				t.Fatalf("step %d state=%v want=%v trace=%x", i, p.State(), state, data)
			}
			if state != PromiseStatePending && p.Result().String() != result {
				t.Fatalf("step %d result=%v want=%v", i, p.Result(), result)
			}
			wantObserved := registered
			if state == PromiseStatePending {
				wantObserved = 0
			}
			if observed != wantObserved {
				t.Fatalf("observers=%d want=%d", observed, wantObserved)
			}
			if r.nativeCallDepth != 0 {
				t.Fatal("leaked native ownership")
			}
			requirePromiseCleanupIdle(t, r)
		}
	})
}

func FuzzIntegrationPromiseQueueOwnership(f *testing.F) {
	for _, seed := range [][]byte{{0}, {1, 0, 2}, {3, 4, 5, 0}, {4, 4, 1, 2}} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 32 {
			data = data[:32]
		}
		r := New()
		r.SetMaxCallStackSize(32)
		p, resolve, _ := r.NewPromise()
		then, _ := AssertFunction(p.val.Get("then"))
		var got, want []string
		for i, b := range data {
			want = append(want, fmt.Sprintf("%d:start", i), fmt.Sprintf("%d:end", i))
			_, err := then(p.val, r.ToValue(func() {
				got = append(got, fmt.Sprintf("%d:start", i))
				switch b % 6 {
				case 0:
					_, err := r.RunString(`1`)
					if err != nil {
						t.Fatal(err)
					}
				case 1:
					fn, _ := AssertFunction(r.ToValue(func() { panic("local") }))
					func() {
						defer func() {
							if recover() != "local" {
								t.Error("panic changed")
							}
						}()
						_, _ = fn(Undefined())
					}()
				case 2:
					_, err := r.RunString(`(function f(){f()})()`)
					var ex *StackOverflowError
					if !errors.As(err, &ex) {
						t.Fatal(err)
					}
				case 3:
					if err := r.RunPromiseJob(func() {
						_, err := r.RunString(`2`)
						if err != nil {
							t.Fatal(err)
						}
					}); err != nil {
						t.Fatal(err)
					}
				case 4:
					child, res, _ := r.NewPromise()
					child.ObserveSettlement(func() {
						if child.State() != PromiseStateFulfilled {
							t.Error("unpublished settlement")
						}
					})
					if err := res(1); err != nil {
						t.Fatal(err)
					}
				case 5:
					_, err := r.RunString(`try{throw 1}catch(e){}`)
					if err != nil {
						t.Fatal(err)
					}
				}
				got = append(got, fmt.Sprintf("%d:end", i))
			}))
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := resolve(1); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got=%v want=%v", got, want)
		}
		if len(r.jobQueue) != 0 || r.nativeCallDepth != 0 {
			t.Fatal("queue or ownership stranded")
		}
		requirePromiseCleanupIdle(t, r)
	})
}

func FuzzIntegrationPromiseAbruptHookRemoval(f *testing.F) {
	f.Add([]byte{0, 1, 2}, byte(4))
	f.Add([]byte{1, 3, 2}, byte(8))
	f.Add([]byte{}, byte(2))
	f.Fuzz(func(t *testing.T, data []byte, count byte) {
		if len(data) > 32 {
			data = data[:32]
		}
		n := int(count%32) + 1
		r := New()
		failure := errors.New("primary")
		var delivered []int
		removed := false
		var hook PromiseJobEnqueuer
		hook = func(job func()) {
			if removed {
				t.Fatal("removed hook resurrected")
			}
			job()
			i := len(delivered) - 1
			if i < len(data) {
				switch data[i] % 4 {
				case 0:
				case 1:
					panic("secondary")
				case 2:
					removed = true
					r.SetPromiseJobEnqueuer(nil)
				case 3:
					r.SetPromiseJobEnqueuer(func(j func()) { hook(j) })
				}
			}
		}
		r.SetPromiseJobEnqueuer(hook)
		func() {
			defer func() {
				if recover() != failure {
					t.Fatal("primary failure replaced")
				}
			}()
			_ = r.RunPromiseJob(func() {
				for i := range n {
					r.jobQueue = append(r.jobQueue, func() { delivered = append(delivered, i) })
				}
				panic(failure)
			})
		}()
		for i, id := range delivered {
			if id != i {
				t.Fatalf("delivery=%v", delivered)
			}
		}
		if !removed && len(delivered) != n {
			t.Fatalf("lost jobs: %d/%d", len(delivered), n)
		}
		if len(r.jobQueue) != 0 || r.promiseJobQueuePriority || r.nativeCallDepth != 0 {
			t.Fatal("incomplete cleanup")
		}
		requirePromiseCleanupIdle(t, r)
	})
}
