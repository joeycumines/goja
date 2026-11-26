package goja

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dlclark/regexp2/v2"
)

func TestRegexpTimeLimitSharedProgramIsolation(t *testing.T) {
	program := MustCompile("shared-regexp-policy", `
		(function () {
			const rx = /^(a+)+$/;
			const test = function () { return rx.test("aaaaaaaaaaaaaaaa"); };
			test.regexp = rx;
			return test;
		})()
	`, false)
	type outcome struct {
		worker int
		base   *regexp2.Regexp
		timed  *regexp2.Regexp
		limit  time.Duration
		err    error
	}
	const workers = 4
	start := make(chan struct{})
	finished := make(chan outcome, workers)
	for worker := range workers {
		go func() {
			result := outcome{worker: worker}
			defer func() { finished <- result }()
			rt := New()
			<-start
			value, err := rt.RunProgram(program)
			if err != nil {
				result.err = err
				return
			}
			test, ok := AssertFunction(value)
			if !ok {
				result.err = errors.New("shared Program did not return a callable")
				return
			}
			for iteration := 0; iteration < 128; iteration++ {
				switch worker {
				case 0:
					result.limit = 0
				case 1:
					result.limit = time.Millisecond
				case 2:
					result.limit = 17 * time.Millisecond
				default:
					result.limit = time.Duration(iteration%3) * time.Millisecond
				}
				rt.SetRegexpTimeLimit(result.limit)
				matched, matchErr := test(Undefined())
				if matchErr != nil || matched == nil || !matched.ToBoolean() {
					result.err = fmt.Errorf("iteration %d: value=%v error=%v", iteration, matched, matchErr)
					return
				}
			}
			rx := value.(*Object).Get("regexp").(*Object).self.(*regexpObject)
			result.base, result.timed = rx.pattern.regexp2Wrapper.rx, rx.pattern.regexp2Wrapper.timed
		}()
	}
	close(start)
	results := make([]outcome, workers)
	for range workers {
		result := <-finished
		results[result.worker] = result
	}
	// Inspect only after every runtime owner has finished. Concurrent execution
	// above is also intended to exercise this ownership boundary under -race.
	for _, result := range results {
		if result.err != nil {
			t.Fatalf("runtime %d: %v", result.worker, result.err)
		}
		if result.base != results[0].base {
			t.Fatal("fixture did not exercise a compiled matcher shared by Program instances")
		}
		if result.base.MatchTimeout != regexp2.DefaultMatchTimeout {
			t.Fatalf("runtime %d changed the shared default matcher timeout", result.worker)
		}
		if result.worker == 0 {
			if result.timed != nil {
				t.Fatal("default runtime acquired another runtime's timed matcher")
			}
			continue
		}
		if result.timed == nil || result.timed == result.base || result.timed.MatchTimeout != result.limit {
			t.Fatalf("runtime %d does not retain its own configured timed matcher", result.worker)
		}
		for _, previous := range results[:result.worker] {
			if result.timed == previous.timed {
				t.Fatalf("runtimes %d and %d share a mutable timed matcher", previous.worker, result.worker)
			}
		}
	}
}

func TestRegexpTimeLimitExistingPatternPolicyToggles(t *testing.T) {
	rt := New()
	if _, err := rt.RunString(`
		var original = /^(a+)+$/;
		var copied = new RegExp(original);
		var recompiled = /x/;
		recompiled.compile(original);
		var patterns = [original, copied, recompiled];
		function positiveMatches() { return patterns.every(rx => rx.test("aaaa")); }
		function countedTimeouts() {
			let count = 0;
			for (const rx of patterns) {
				try { rx.test("a".repeat(40) + "b"); }
				catch (e) {
					if (!(e instanceof RangeError) || !e.message.includes("match timeout")) throw e;
					count++;
				}
			}
			return count;
		}
	`); err != nil {
		t.Fatal(err)
	}
	positive, _ := AssertFunction(rt.Get("positiveMatches"))
	timeouts, _ := AssertFunction(rt.Get("countedTimeouts"))
	for _, limit := range []time.Duration{time.Nanosecond, 2 * time.Millisecond} {
		rt.SetRegexpTimeLimit(limit)
		count, err := timeouts(Undefined())
		if err != nil || count == nil || count.ToInteger() != 3 {
			t.Fatalf("existing pattern policy %v: count=%v error=%v", limit, count, err)
		}
		rt.SetRegexpTimeLimit(0)
		// Never run an adversarial rejection with limits disabled: the test must
		// not leave an unkillable match worker behind if the timeout is broken.
		value, err := positive(Undefined())
		if err != nil || value == nil || !value.ToBoolean() {
			t.Fatalf("reuse after disabling policy: value=%v error=%v", value, err)
		}
	}
	if rt.Get("original").(*Object).self.(*regexpObject).pattern.regexp2Wrapper.rx.MatchTimeout != regexp2.DefaultMatchTimeout {
		t.Fatal("policy toggles changed the untimed matcher")
	}
}

func regexpLimitConcurrencyCheckCancellation(t *testing.T, rt *Runtime, err error, cause error, terminal bool) {
	t.Helper()
	var interrupted *InterruptedError
	if !errors.As(err, &interrupted) || interrupted.Value() != cause {
		t.Fatalf("cancellation was replaced: got %v, want InterruptedError carrying %v", err, cause)
	}
	for _, name := range []string{"caught", "finalized", "afterMatch"} {
		if rt.Get(name).ToBoolean() {
			t.Fatalf("JavaScript %s ran despite cancellation", name)
		}
	}
	rt.ClearInterrupt()
	value, nextErr := rt.RunString("42")
	if terminal {
		var next *InterruptedError
		if !errors.As(nextErr, &next) || next.Value() != cause {
			t.Fatalf("terminal cause did not survive clear/reentry: value=%v error=%v", value, nextErr)
		}
	} else if nextErr != nil || value == nil || value.ToInteger() != 42 {
		t.Fatalf("ordinary Interrupt prevented reuse after clear: value=%v error=%v", value, nextErr)
	}
}

func TestRegexpTimeLimitCancellationBeforeSearch(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		name := "Interrupt"
		if terminal {
			name = "Abort"
		}
		t.Run(name, func(t *testing.T) {
			rt := New()
			rt.SetRegexpTimeLimit(time.Millisecond)
			cause := fmt.Errorf("%s during regexp input coercion", name)
			argument := rt.NewObject()
			coerced := false
			if err := argument.Set("toString", func(FunctionCall) Value {
				coerced = true
				if terminal {
					rt.Abort(cause)
				} else {
					rt.Interrupt(cause)
				}
				return rt.ToValue(strings.Repeat("a", 40) + "b")
			}); err != nil {
				t.Fatal(err)
			}
			if err := rt.Set("argument", argument); err != nil {
				t.Fatal(err)
			}
			_, err := rt.RunString(`
				var caught = false, finalized = false, afterMatch = false;
				try { /^(a+)+$/.test(argument); afterMatch = true; }
				catch (e) { caught = true; }
				finally { finalized = true; }
			`)
			if !coerced {
				t.Fatal("fixture did not reach native regexp input coercion")
			}
			regexpLimitConcurrencyCheckCancellation(t, rt, err, cause, terminal)
		})
	}
}

func regexpLimitConcurrencyRunSearch(rt *Runtime, program *Program, completed chan<- error) {
	_, err := rt.RunProgram(program)
	completed <- err
}

func regexpLimitConcurrencySearchOnStack() bool {
	buffer := make([]byte, 256<<10)
	for {
		length := runtime.Stack(buffer, true)
		if length == len(buffer) && len(buffer) < 4<<20 {
			buffer = make([]byte, len(buffer)*2)
			continue
		}
		for _, stack := range bytes.Split(buffer[:length], []byte("\n\n")) {
			if bytes.Contains(stack, []byte("regexpLimitConcurrencyRunSearch(")) &&
				bytes.Contains(stack, []byte("regexp2/v2.executeDefault(")) {
				return true
			}
		}
		return false
	}
}

func TestRegexpTimeLimitCancellationDuringSearch(t *testing.T) {
	program := MustCompile("cancel-in-native-regexp", `
		try { rx.test(input); afterMatch = true; }
		catch (e) { caught = true; }
		finally { finalized = true; }
	`, false)
	for _, terminal := range []bool{false, true} {
		name := "Interrupt"
		if terminal {
			name = "Abort"
		}
		t.Run(name, func(t *testing.T) {
			rt := New()
			rt.SetRegexpTimeLimit(250 * time.Millisecond)
			if _, err := rt.RunString(`
				var rx = /^(a+)+$/;
				var caught = false, finalized = false, afterMatch = false;
				rx.test("aaaa");
			`); err != nil {
				t.Fatal(err)
			}
			if err := rt.Set("input", strings.Repeat("a", 40)+"b"); err != nil {
				t.Fatal(err)
			}
			cause := fmt.Errorf("%s while regexp matcher is running", name)
			completed := make(chan error, 1)
			go regexpLimitConcurrencyRunSearch(rt, program, completed)
			observed := false
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				select {
				case err := <-completed:
					t.Fatalf("bounded search ended before its native execution was observed: %v", err)
				default:
				}
				if regexpLimitConcurrencySearchOnStack() {
					observed = true
					break
				}
				time.Sleep(time.Millisecond)
			}
			if terminal {
				rt.Abort(cause)
			} else {
				rt.Interrupt(cause)
			}
			// Join the synchronous match's owner; do not abandon a regex worker
			// in a timeout/select branch. go test's process timeout is the outer
			// failure guard if the matcher implementation stops honoring limits.
			err := <-completed
			if !observed {
				t.Fatal("did not observe this worker inside regexp2's native matcher")
			}
			regexpLimitConcurrencyCheckCancellation(t, rt, err, cause, terminal)
		})
	}
}
