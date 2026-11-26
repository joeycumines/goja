package goja

import (
	"errors"
	"testing"
)

func BenchmarkIntegrationNativeEntry(b *testing.B) {
	r := New()
	fn, _ := AssertFunction(r.ToValue(func() {}))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := fn(Undefined()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkIntegrationProgramEntry(b *testing.B) {
	r := New()
	p, err := Compile("bench", `1+1`, false)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.RunProgram(p); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkIntegrationPromiseBurst(b *testing.B) {
	for _, external := range []bool{false, true} {
		for _, tracked := range []bool{false, true} {
			name := "internal"
			if external {
				name = "external"
			}
			if tracked {
				name += "/context"
			}
			b.Run(name, func(b *testing.B) {
				r := New()
				if tracked {
					r.SetAsyncContextTracker(noopAsyncContextTracker{})
				}
				var jobs []func()
				if external {
					r.SetPromiseJobEnqueuer(func(j func()) { jobs = append(jobs, j) })
				}
				p, err := Compile("bench", `for(let i=0;i<100;i++) Promise.resolve(i).then(x=>x+1)`, false)
				if err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if _, err := r.RunProgram(p); err != nil {
						b.Fatal(err)
					}
					for len(jobs) > 0 {
						j := jobs[0]
						jobs[0] = nil
						jobs = jobs[1:]
						if err := r.RunPromiseJob(j); err != nil {
							b.Fatal(err)
						}
					}
					jobs = jobs[:0]
				}
			})
		}
	}
}

func BenchmarkIntegrationNativePromise(b *testing.B) {
	r := New()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		p, resolve, _ := r.NewPromise()
		p.ObserveSettlement(func() {})
		if err := resolve(1); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkIntegrationAbortAlreadyTerminal(b *testing.B) {
	r := New()
	cause := errors.New("stop")
	r.Abort(cause)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r.Abort(cause)
		r.ClearInterrupt()
	}
}
