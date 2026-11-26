package goja

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// FuzzGeneratorIteratorFastPath compares the value-only path against the
// IteratorResult path for the same generator and operation sequence. The
// dormant yield* selects the latter without changing observable execution.
// Native failures exercise completion and frame restoration as well as values.
func FuzzGeneratorIteratorFastPath(f *testing.F) {
	for _, seed := range [][]byte{
		{4, 0, 0, 0, 5},
		{4, 1, 1, 1, 5, 0},
		{5, 2, 2, 1, 4, 3, 0},
		{3, 3, 0, 0, 5, 0, 1, 2},
		{7, 0, 3, 1, 0, 1, 0, 0, 3, 4, 5},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 5 {
			return
		}
		if len(data) > 36 {
			data = data[:36]
		}
		count, failureMode, failureAt := int(data[0]%8)+1, data[1]%4, int(data[2]%9)
		yieldFinally := data[3]&1 != 0
		results := make([][]string, 2)
		for variant := range 2 {
			rt := New()
			rt.SetMaxCallStackSize(128)
			cause := errors.New("generator fuzz failure")
			if err := rt.Set("fault", func() {
				switch failureMode {
				case 1:
					panic(cause)
				case 2:
					rt.Interrupt(cause)
				case 3:
					rt.SetMaxCallStackSize(0)
					if _, err := rt.RunString(`1`); err != nil {
						panic(err)
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			guard := ""
			if variant != 0 {
				guard = `if (neverDelegate) yield* [];`
			}
			source := fmt.Sprintf(`
				var neverDelegate = false, log = [];
				function* body() {
					%s
					try {
						for (let i = 0; i < %d; i++) {
							if (i === %d) fault();
							yield i;
						}
						return 100;
					} catch (e) { log.push("caught:" + String(e)); yield 200; }
					finally { log.push("finally"); if (%t) yield 300; }
				}
				var g = body();
			`, guard, count, failureAt, yieldFinally)
			if _, err := rt.RunString(source); err != nil {
				t.Fatal(err)
			}
			if got := rt.Get("g").(*Object).self.(*generatorObject).hasYieldDelegate; got != (variant != 0) {
				t.Fatalf("generator path flag=%v variant=%d", got, variant)
			}
			for _, operation := range data[4:] {
				n := int(operation>>3) % 4
				var action string
				switch operation % 6 {
				case 0:
					action = fmt.Sprintf(`g.next(%d)`, n)
				case 1:
					action = fmt.Sprintf(`g.return(%d)`, n)
				case 2:
					action = fmt.Sprintf(`g.throw("throw%d")`, n)
				case 3:
					action = fmt.Sprintf(`g.take(%d).toArray()`, n)
				case 4:
					action = fmt.Sprintf(`g.drop(%d).next()`, n)
				case 5:
					action = `[...g]`
				}
				var value Value
				var err error
				panicked := tryFunc(func() {
					value, err = rt.RunString(`(() => { let result; try { result = ` + action + `; } catch (e) { result = ["throw", String(e)]; } return JSON.stringify([result, log]); })()`)
				})
				var outcome string
				switch {
				case panicked != nil:
					if panicked != cause {
						t.Fatalf("unexpected panic %v trace=%x", panicked, data)
					}
					outcome = "native panic"
				case errors.Is(err, cause):
					outcome = "interrupt"
				case err != nil:
					var overflow *StackOverflowError
					if !errors.As(err, &overflow) {
						t.Fatalf("unexpected error %v trace=%x", err, data)
					}
					outcome = "overflow"
				default:
					outcome = value.String()
				}
				results[variant] = append(results[variant], outcome)
				rt.ClearInterrupt()
				rt.SetMaxCallStackSize(128)
				assertPromiseBoundaryClean(t, rt)
			}
		}
		if !reflect.DeepEqual(results[0], results[1]) {
			t.Fatalf("trace=%x\nfast=%q\nobject=%q", data, results[0], results[1])
		}
	})
}

// FuzzNativeIteratorOwnership checks an independent finite resource model:
// each source starts at most once, each started source stops exactly once on
// completion, return, a native failure, or terminal abort. Other live sources
// remain resumable after a nonterminal failure.
func FuzzNativeIteratorOwnership(f *testing.F) {
	for _, seed := range [][]byte{
		{3, 5, 7, 0, 0, 0, 0, 1},
		{2, 2, 2, 0, 8, 16, 3, 9, 17},
		{5, 3, 1, 0, 8, 16, 4, 0, 8, 16},
		{6, 6, 6, 3, 11, 19},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 4 {
			return
		}
		if len(data) > 67 {
			data = data[:67]
		}
		rt := New()
		type state struct {
			object                                     *Object
			next, ret                                  Callable
			limit, panicAt, calls                      int
			started, stopped, wantStarted, wantStopped int
			closed                                     bool
			cause                                      error
		}
		states := make([]*state, 3)
		for index := range states {
			s := &state{limit: int(data[index] % 5), panicAt: int(data[index]>>3) % 6, cause: fmt.Errorf("source %d", index)}
			states[index] = s
			source := func(yield func(int) bool) {
				s.started++
				defer func() { s.stopped++ }()
				for n := 0; n < s.limit; n++ {
					if n == s.panicAt {
						panic(s.cause)
					}
					if !yield(n) {
						return
					}
				}
			}
			s.object = rt.newGoIter(rt.wrapIterSeq(reflect.ValueOf(source)))
			s.next, _ = AssertFunction(s.object.Get("next"))
			s.ret, _ = AssertFunction(s.object.Get("return"))
		}
		bodyPanic := errors.New("loop body panic")
		if err := rt.Set("bodyPanic", func() { panic(bodyPanic) }); err != nil {
			t.Fatal(err)
		}
		aborted := false
		for step, operation := range data[3:] {
			s := states[(operation>>3)%3]
			op := operation % 5
			var expectedPanic interface{}
			var expectedDone bool
			expectedValue := s.calls
			if op == 0 || op == 3 {
				if s.closed {
					expectedDone = true
				} else {
					s.wantStarted = 1
					switch {
					case s.calls >= s.limit:
						expectedDone, s.closed, s.wantStopped = true, true, 1
					case s.calls == s.panicAt:
						expectedPanic, s.closed, s.wantStopped = s.cause, true, 1
					default:
						s.calls++
						if op == 3 {
							expectedPanic, s.closed, s.wantStopped = bodyPanic, true, 1
						}
					}
				}
			}
			var value Value
			var err error
			gotPanic := tryFunc(func() {
				switch op {
				case 0:
					value, err = s.next(s.object)
				case 1:
					value, err = s.ret(s.object)
					s.closed, s.wantStopped = true, s.wantStarted
				case 2:
					_, err = rt.RunString(`40 + 2`)
				case 3:
					if aborted {
						value, err = s.next(s.object)
						return
					}
					if err = rt.Set("selected", s.object); err != nil {
						return
					}
					_, err = rt.RunString(`for (const value of { [Symbol.iterator]() { return selected; } }) bodyPanic();`)
				case 4:
					aborted = true
					rt.Abort(bodyPanic)
					_, err = rt.RunString(`0`)
					for _, state := range states {
						state.closed, state.wantStopped = true, state.wantStarted
					}
				}
			})
			if gotPanic != expectedPanic {
				t.Fatalf("step=%d trace=%x panic=%v want=%v", step, data, gotPanic, expectedPanic)
			}
			if err != nil && !(aborted && errors.Is(err, bodyPanic)) {
				t.Fatalf("step=%d trace=%x err=%v", step, data, err)
			}
			if op == 0 && gotPanic == nil && err == nil {
				result := value.(*Object)
				if result.Get("done").ToBoolean() != expectedDone || (!expectedDone && result.Get("value").ToInteger() != int64(expectedValue)) {
					t.Fatalf("step=%d unexpected iterator result %v", step, value)
				}
			}
			registered := 0
			for _, state := range states {
				if state.started != state.wantStarted || state.stopped != state.wantStopped {
					t.Fatalf("step=%d trace=%x starts=%d/%d stops=%d/%d", step, data, state.started, state.wantStarted, state.stopped, state.wantStopped)
				}
				if !state.closed {
					registered++
				}
			}
			if len(rt.nativeIterators) != registered {
				t.Fatalf("step=%d trace=%x registry=%d want=%d", step, data, len(rt.nativeIterators), registered)
			}
			assertPromiseBoundaryClean(t, rt)
		}
		rt.closeNativeIterators()
		for _, state := range states {
			if state.started != state.stopped {
				t.Fatalf("source leaked at end: started=%d stopped=%d", state.started, state.stopped)
			}
		}
	})
}
