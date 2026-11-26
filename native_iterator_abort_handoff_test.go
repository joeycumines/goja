package goja

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestNativeIteratorCleanupJobForwardingWaves(t *testing.T) {
	for _, waves := range []int{1, 3, 12} {
		for _, nativePanic := range []bool{false, true} {
			t.Run(fmt.Sprintf("waves%d/panic%v", waves, nativePanic), func(t *testing.T) {
				rt := New()
				cause := errors.New("terminal abort")
				primary := errors.New("native panic")
				var rejectors []func(interface{}) error
				for i := 0; i < waves; i++ {
					promise, _, reject := rt.NewPromise()
					then, _ := AssertFunction(promise.val.Get("then"))
					if _, err := then(promise.val, _undefined, rt.ToValue(func() {})); err != nil {
						t.Fatal(err)
					}
					rejectors = append(rejectors, reject)
				}
				delivered, stopped := 0, 0
				var jobs []func()
				hook := func(job func()) {
					jobs = append(jobs, job)
					wave := delivered
					delivered++
					if wave < waves {
						sequence := func(yield func(int) bool) {
							defer func() {
								stopped++
								if err := rejectors[wave]("stopped"); err != nil {
									panic(err)
								}
							}()
							yield(wave)
						}
						iter := rt.newGoIter(rt.wrapIterSeq(reflect.ValueOf(sequence)))
						iter.self.(*goIteratorObject).nextResult(_undefined)
					}
				}
				if err := rt.Set("fail", func() {
					rt.SetPromiseJobEnqueuer(hook)
					rt.Abort(cause)
					if nativePanic {
						panic(primary)
					}
				}); err != nil {
					t.Fatal(err)
				}
				var err error
				gotPanic := tryFunc(func() {
					_, err = rt.RunString(`Promise.resolve().then(() => {}); fail(); 1;`)
				})
				if nativePanic {
					if gotPanic != primary || err != nil {
						t.Fatalf("panic=%v err=%v", gotPanic, err)
					}
				} else if gotPanic != nil || !errors.Is(err, cause) {
					t.Fatalf("panic=%v err=%v", gotPanic, err)
				}
				if delivered != waves+1 || stopped != waves || len(rt.nativeIterators) != 0 {
					t.Fatalf("delivered=%d stopped=%d registered=%d", delivered, stopped, len(rt.nativeIterators))
				}
				for _, job := range jobs {
					if err := rt.RunPromiseJob(job); !errors.Is(err, cause) {
						t.Fatalf("aborted job: %v", err)
					}
				}
				assertPromiseBoundaryClean(t, rt)
			})
		}
	}
}
