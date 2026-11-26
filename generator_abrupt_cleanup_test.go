package goja

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestGeneratorAbruptCleanupMatrix(t *testing.T) {
	cases := map[string]string{
		"for_return":                `function* gen(){for(const x of it){yield x;}} const g=gen();g.next(); [g,"return"];`,
		"for_throw":                 `function* gen(){for(const x of it){yield x;}} const g=gen();g.next(); [g,"throw"];`,
		"for_break":                 `function* gen(){for(const x of it){yield x;break;}} const g=gen();g.next(); [g,"next"];`,
		"for_return_finally_yield":  `function* gen(){try {for(const x of it){yield x;}} finally {yield 2;}} const g=gen();g.next(); [g,"return"];`,
		"for_throw_finally_yield":   `function* gen(){try {for(const x of it){yield x;}} finally {yield 2;}} const g=gen();g.next(); [g,"throw"];`,
		"for_return_nested":         `function* gen(){for(const x of it){for(const y of it){yield x;}}} const g=gen();g.next(); [g,"return"];`,
		"for_throw_nested":          `function* gen(){for(const x of it){for(const y of it){yield x;}}} const g=gen();g.next(); [g,"throw"];`,
		"delegate_return":           `function* gen(){yield* it;} const g=gen();g.next(); [g,"return"];`,
		"delegate_throw":            `function* gen(){yield* it;} const g=gen();g.next(); [g,"throw"];`,
		"delegate_return_finally":   `function* gen(){try {yield* it;}finally{yield 2;}} const g=gen();g.next(); [g,"return"];`,
		"delegate_throw_finally":    `function* gen(){try {yield* it;}finally{yield 2;}} const g=gen();g.next(); [g,"throw"];`,
		"delegate_generator_return": `function* inner(){for(const x of it) yield x;} function* gen(){yield* inner();} const g=gen();g.next(); [g,"return"];`,
		"delegate_generator_throw":  `function* inner(){for(const x of it) yield x;} function* gen(){yield* inner();} const g=gen();g.next(); [g,"throw"];`,
	}
	for name, src := range cases {
		for _, mode := range []string{"panic", "interrupt", "overflow"} {
			for _, grow := range []int{0, 64} {
				t.Run(fmt.Sprintf("%s/%s/%d", name, mode, grow), func(t *testing.T) {
					rt := New()
					cause := errors.New("stop")
					if err := rt.Set("stop", func() {
						switch mode {
						case "panic":
							panic(cause)
						case "interrupt":
							rt.Interrupt(cause)
						case "overflow":
							rt.SetMaxCallStackSize(0)
							_, err := rt.RunString(`1`)
							if err != nil {
								panic(err)
							}
						}
					}); err != nil {
						t.Fatal(err)
					}
					setup := fmt.Sprintf(`function nested(n){if(n){try{nested(n-1);}finally{}}} const it = {[Symbol.iterator](){return {next(){return {value:1,done:false};},return(){nested(%d);stop();return {done:true};}};}}; `, grow)
					val, err := rt.RunString(setup + src)
					if err != nil {
						t.Fatal(err)
					}
					a := val.(*Object)
					target := a.Get("0").(*Object)
					fn, _ := AssertFunction(target.Get(a.Get("1").String()))
					if err := rt.Set("invoke", func() int {
						before := rt.vm.saveExecutionFrame()
						var gotP interface{}
						var gotE error
						func() { defer func() { gotP = recover() }(); _, gotE = fn(target, rt.ToValue("x")) }()
						after := rt.vm.saveExecutionFrame()
						if !reflect.DeepEqual(before, after) {
							t.Errorf("frame differs: before=%+v after=%+v", before, after)
						}
						switch mode {
						case "panic":
							if gotP != cause {
								t.Errorf("gotP=%v err=%v", gotP, gotE)
							}
						case "interrupt":
							if gotP != nil || !errors.Is(gotE, cause) {
								t.Errorf("gotP=%v err=%v", gotP, gotE)
							}
						case "overflow":
							var ex *StackOverflowError
							if gotP != nil || !errors.As(gotE, &ex) {
								t.Errorf("gotP=%v err=%v", gotP, gotE)
							}
						}
						rt.ClearInterrupt()
						rt.SetMaxCallStackSize(1024)
						return 2
					}); err != nil {
						t.Fatal(err)
					}
					val, err = rt.RunString(`(function outer(){let o={x:40};try{for(const x of [1]){o.x+=invoke();}}finally{}return o.x;})()`)
					if err != nil || val == nil || val.ToInteger() != 42 {
						t.Fatalf("outer returned %v %v", val, err)
					}
					next, _ := AssertFunction(target.Get("next"))
					val, err = next(target)
					if err != nil || !val.(*Object).Get("done").ToBoolean() {
						t.Fatalf("not completed after error: %v %v", val, err)
					}
					assertGeneratorBoundaryClean(t, rt)
				})
			}
		}
	}
}
