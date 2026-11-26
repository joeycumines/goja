package goja

import "testing"

func BenchmarkGeneratorIteratorIntegration(b *testing.B) {
	for name, generator := range map[string]string{
		"ordinary": `function* values() { for (let i = 0; i < 100; i++) yield i; }`,
		"ordinary_object_path": `var neverDelegate = false;
			function* values() { if (neverDelegate) yield* []; for (let i = 0; i < 100; i++) yield i; }`,
		"delegated": `function* values() { yield* numbers; }`,
	} {
		b.Run(name, func(b *testing.B) {
			rt := New()
			if _, err := rt.RunString(`var numbers = Array.from({length: 100}, (_, i) => i); ` + generator); err != nil {
				b.Fatal(err)
			}
			program := MustCompile("generator-iterator-benchmark.js", `(function() {
				let sum = 0;
				for (const value of values()) sum += value;
				return sum;
			})()`, false)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				value, err := rt.RunProgram(program)
				if err != nil || value.ToInteger() != 4950 {
					b.Fatalf("value=%v err=%v", value, err)
				}
			}
		})
	}
}

func BenchmarkNativeIteratorLifecycle(b *testing.B) {
	for _, earlyReturn := range []bool{false, true} {
		name := "exhaust"
		if earlyReturn {
			name = "early_return"
		}
		b.Run(name, func(b *testing.B) {
			rt := New()
			stopped := 0
			if err := rt.Set("sequence", func(yield func(int) bool) {
				defer func() { stopped++ }()
				for i := range 100 {
					if !yield(i) {
						return
					}
				}
			}); err != nil {
				b.Fatal(err)
			}
			body := ""
			if earlyReturn {
				body = "break;"
			}
			program := MustCompile("native-iterator-benchmark.js", `for (const value of sequence) { `+body+` }`, false)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := rt.RunProgram(program); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if stopped != b.N || len(rt.nativeIterators) != 0 {
				b.Fatalf("stopped=%d iterations=%d registered=%d", stopped, b.N, len(rt.nativeIterators))
			}
		})
	}
}
