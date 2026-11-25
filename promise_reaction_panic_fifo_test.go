package goja

import (
	"errors"
	"strings"
	"testing"
)

func TestPromiseEnqueuerPanicPreservesBufferedJobOrder(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "normal"
		if fail {
			name = "panicking"
		}
		t.Run(name, func(t *testing.T) {
			rt := New()
			rt.SetAsyncContextTracker(noopAsyncContextTracker{})
			p, resolve, _ := rt.NewPromise()
			q, resolveQ, _ := rt.NewPromise()
			var order []string
			attach := func(p *Promise, f func()) {
				then, _ := AssertFunction(p.val.Get("then"))
				if _, err := then(p.val, rt.ToValue(f)); err != nil {
					t.Fatal(err)
				}
			}
			attach(p, func() {
				order = append(order, "first")
				if err := resolveQ("q"); err != nil {
					panic(err)
				}
			})
			attach(p, func() { order = append(order, "second") })
			attach(p, func() { order = append(order, "third") })
			attach(q, func() { order = append(order, "callback-work") })
			first := true
			failure := errors.New("hook failed after queueing callback work")
			var jobs []func()
			var hook PromiseJobEnqueuer
			hook = func(job func()) {
				if first {
					first = false
					rt.SetPromiseJobEnqueuer(nil)
					if err := rt.RunPromiseJob(job); err != nil {
						t.Fatal(err)
					}
					rt.SetPromiseJobEnqueuer(hook)
					if fail {
						panic(failure)
					}
					return
				}
				jobs = append(jobs, job)
			}
			rt.SetPromiseJobEnqueuer(hook)
			func() {
				defer func() {
					got := recover()
					if (fail && got != failure) || (!fail && got != nil) {
						t.Errorf("panic=%v fail=%v", got, fail)
					}
				}()
				if err := resolve("p"); err != nil {
					t.Fatal(err)
				}
			}()
			for len(jobs) > 0 {
				job := jobs[0]
				jobs = jobs[1:]
				if err := rt.RunPromiseJob(job); err != nil {
					t.Fatal(err)
				}
			}
			if got := strings.Join(order, ","); got != "first,callback-work,second,third" {
				t.Fatalf("order=%s", got)
			}
		})
	}
}
