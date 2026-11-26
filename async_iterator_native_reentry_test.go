package goja

import "testing"

func TestAsyncIteratorNativeReentryRestoresResultSlots(t *testing.T) {
	for _, syncFallback := range []bool{false, true} {
		name := "async"
		if syncFallback {
			name = "sync_fallback"
		}
		t.Run(name, func(t *testing.T) {
			rt := New()
			nested := MustCompile("native-iterator-reentry.js", `
				(function grow(n) {
					if (n === 0) return 1;
					try { return grow(n - 1) + 1; } finally {}
				})(64)
			`, false)
			reenter := func() {
				value, err := rt.RunProgram(nested)
				if err != nil || value.ToInteger() != 65 {
					t.Fatalf("native reentry: %v, %v", value, err)
				}
			}
			iterable, iterator := rt.NewObject(), rt.NewObject()
			nextCalls, returnCalls := 0, 0
			if err := iterator.Set("next", func() Value {
				reenter()
				nextCalls++
				return rt.createIterResultObject(intToValue(42), false)
			}); err != nil {
				t.Fatal(err)
			}
			if err := iterator.Set("return", func() Value {
				reenter()
				returnCalls++
				return rt.createIterResultObject(_undefined, true)
			}); err != nil {
				t.Fatal(err)
			}
			symbol := SymAsyncIterator
			if syncFallback {
				symbol = SymIterator
			}
			if err := iterable.SetSymbol(symbol, func() Value { return iterator }); err != nil {
				t.Fatal(err)
			}
			if err := rt.Set("iterable", iterable); err != nil {
				t.Fatal(err)
			}
			value, err := rt.RunString(`(async () => {
				let result = 0;
				for await (const value of iterable) { result = value; break; }
				return result;
			})()`)
			if err != nil {
				t.Fatal(err)
			}
			promise := value.Export().(*Promise)
			if promise.State() != PromiseStateFulfilled || promise.Result().ToInteger() != 42 || nextCalls != 1 || returnCalls != 1 {
				t.Fatalf("state=%v result=%v next=%d return=%d", promise.State(), promise.Result(), nextCalls, returnCalls)
			}
			assertGeneratorBoundaryClean(t, rt)
		})
	}
}
