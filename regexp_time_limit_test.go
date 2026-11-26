package goja

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestRegexpTimeLimitAllMatchingPaths(t *testing.T) {
	operations := []struct {
		name, expression string
	}{
		{"test", `rx.test(input)`},
		{"exec", `rx.exec(input)`},
		{"match", `input.match(rx)`},
		{"replace", `input.replace(rx, "x")`},
		{"split", `input.split(rx)`},
		{"matchAll", `Array.from(input.matchAll(rx))`},
	}
	for _, flags := range []string{"g", "gu"} {
		for _, operation := range operations {
			t.Run(operation.name+"/"+flags, func(t *testing.T) {
				rt := New()
				// This uses the dependency's coarse clock, so do not assert an
				// exact wall-clock duration. Matching must still return normally.
				rt.SetRegexpTimeLimit(time.Nanosecond)
				value, err := rt.RunString(fmt.Sprintf(`
					const rx = /^é(a+)+$/%s;
					const input = "é" + "a".repeat(40) + "b";
					let caught = false;
					try { %s; }
					catch (e) {
						caught = e instanceof RangeError && e.message.includes("match timeout");
					}
					caught;
				`, flags, operation.expression))
				if err != nil || value == nil || !value.ToBoolean() {
					t.Fatalf("expected catchable timeout: value=%v error=%v", value, err)
				}
				// Reuse both the runtime and the same RegExp after the timeout.
				value, err = rt.RunString(`rx.lastIndex = 0; rx.test("éaaa") && 21 + 21 === 42`)
				if err != nil || value == nil || !value.ToBoolean() {
					t.Fatalf("reuse after timeout: value=%v error=%v", value, err)
				}
			})
		}
	}
}

func TestRegexpTimeLimitPreservesPatternSemantics(t *testing.T) {
	// Fixed expected results are checked independently with Node 26.5.0.
	// Include capture reset, Unicode conversion, lazy/global operations, and
	// every flag passed to the private matcher compiler.
	cases := []struct{ expression, want string }{
		{`JSON.stringify(/(a|(b))+/.exec("aba"))`, `["aba","a",null]`},
		{`JSON.stringify("É\nA".match(/^é.a$/ims))`, `["É\nA"]`},
		{`JSON.stringify("💩a💩".match(/./gu))`, `["💩","a","💩"]`},
		{`JSON.stringify("💩a💩".match(/./g).map(x => x.charCodeAt(0)))`, `[55357,56489,97,55357,56489]`},
		{`JSON.stringify("é1é2".replace(/é(\d)/gu, "$1é"))`, `"1é2é"`},
		{`JSON.stringify("a💩b".split(/💩/u))`, `["a","b"]`},
		{`JSON.stringify(Array.from("é1é2".matchAll(/(?<n>é)(\d)/gu), m => [m.index,m.groups.n,m[2]]))`, `[[0,"é","1"],[2,"é","2"]]`},
		{`JSON.stringify((() => {const r=/💩/uy; r.lastIndex=1; const m=r.exec("a💩b"); return [m[0],m.index,r.lastIndex]})())`, `["💩",1,3]`},
		{`JSON.stringify("💩a".match(/(?:)/gu))`, `["","",""]`},
		{`JSON.stringify(/\ud800/u.exec("\ud800"))`, `["\ud800"]`},
		{`JSON.stringify(/(a)\1/.exec("baa"))`, `["aa","a"]`},
		{`JSON.stringify(/(?<=é)a/u.exec("éa"))`, `["a"]`},
	}
	for i, test := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			program, err := Compile("regexp-policy", test.expression, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, limit := range []time.Duration{0, time.Second} {
				rt := New()
				rt.SetRegexpTimeLimit(limit)
				value, err := rt.RunProgram(program)
				if err != nil || value == nil || value.String() != test.want {
					t.Fatalf("limit=%v value=%v error=%v want=%s", limit, value, err, test.want)
				}
			}
		})
	}
}

func TestRegexpTimeLimitRejectsInvalidConfiguration(t *testing.T) {
	rt := New()
	rt.SetRegexpTimeLimit(time.Second)
	for _, limit := range []time.Duration{-1, 24*time.Hour + 1, math.MaxInt64} {
		func() {
			defer func() {
				failure := recover()
				if failure == nil || !strings.Contains(fmt.Sprint(failure), "regexp time limit") {
					t.Errorf("limit=%v panic=%v", limit, failure)
				}
			}()
			rt.SetRegexpTimeLimit(limit)
		}()
		if rt.regexpTimeLimit != time.Second {
			t.Fatal("invalid configuration changed the active policy")
		}
	}
	for _, limit := range []time.Duration{0, 1, 24 * time.Hour} {
		rt.SetRegexpTimeLimit(limit)
		if rt.regexpTimeLimit != limit {
			t.Fatalf("accepted limit=%v, got %v", limit, rt.regexpTimeLimit)
		}
	}
}

func BenchmarkRegexpTimeLimit(b *testing.B) {
	program := MustCompile("regexp-policy-benchmark", `rx.test("alpha-123")`, false)
	for _, limit := range []time.Duration{0, time.Second} {
		b.Run(limit.String(), func(b *testing.B) {
			rt := New()
			rt.SetRegexpTimeLimit(limit)
			if _, err := rt.RunString(`var rx = /(\w+)-(\d+)/`); err != nil {
				b.Fatal(err)
			}
			if _, err := rt.RunProgram(program); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				value, err := rt.RunProgram(program)
				if err != nil || !value.ToBoolean() {
					b.Fatalf("value=%v error=%v", value, err)
				}
			}
		})
	}
}

func FuzzRegexpTimeLimitParity(f *testing.F) {
	for _, seed := range [][]byte{
		{0, 0}, {1, 1}, {2, 2}, {3, 3}, {4, 4}, {5, 5}, {6, 6}, {7, 7},
		append([]byte{4, 3}, []byte("é💩é")...),
		append([]byte{5, 2}, []byte("alpha-123 beta-456")...),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 || len(data) > 66 {
			return
		}
		// A bounded non-catastrophic grammar compares policy execution paths.
		// It does not establish general regex correctness; the fixed Node
		// observations above provide an independent, separately bounded oracle.
		patterns := []string{`(?:)`, `[a-z]+|[0-9]+`, `a(b)?`, `[\s\S]`, `(?<x>é|💩)`, `(\w+)-(\d+)`, `(?<=a)b`, `^(a|b)*$`}
		flags := []string{"", "u", "g", "gu", "i", "im", "s", "su", "y", "uy"}
		program, err := Compile("regexp-policy-fuzz", fmt.Sprintf(`globalThis.rx = /%s/%s`, patterns[int(data[0])%len(patterns)], flags[int(data[1])%len(flags)]), false)
		if err != nil {
			t.Fatal(err)
		}
		const observe = `JSON.stringify([rx.exec(input), rx.lastIndex, rx.test(input), input.match(rx), input.replace(rx, "[$&]"), input.split(rx)])`
		var expected string
		for index, limit := range []time.Duration{0, time.Second, 0} {
			rt := New()
			if _, err := rt.RunProgram(program); err != nil {
				t.Fatal(err)
			}
			// Apply the policy after creating the object, as a host may do.
			rt.SetRegexpTimeLimit(limit)
			if err := rt.Set("input", string(data[2:])); err != nil {
				t.Fatal(err)
			}
			value, err := rt.RunString(observe)
			if err != nil {
				t.Fatalf("limit=%v: %v", limit, err)
			}
			if index == 0 {
				expected = value.String()
			} else if value.String() != expected {
				t.Fatalf("limit=%v got=%s want=%s", limit, value.String(), expected)
			}
		}
	})
}

func BenchmarkRegexpTimeLimitLiteral(b *testing.B) {
	// Unlike the steady-state benchmark, this includes a new literal object
	// and its private matcher compilation on every enabled-policy call.
	program := MustCompile("regexp-policy-literal", `/(\w+)-(\d+)/.test("alpha-123")`, false)
	for _, limit := range []time.Duration{0, time.Second} {
		b.Run(limit.String(), func(b *testing.B) {
			rt := New()
			rt.SetRegexpTimeLimit(limit)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				value, err := rt.RunProgram(program)
				if err != nil || !value.ToBoolean() {
					b.Fatalf("value=%v error=%v", value, err)
				}
			}
		})
	}
}
