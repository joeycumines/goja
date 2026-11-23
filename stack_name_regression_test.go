package goja

import (
	"testing"

	"github.com/joeycumines/goja/unistring"
)

func TestGetFuncNameHandlesNilInitialCallee(t *testing.T) {
	var callee *Object
	stack := []Value{callee, _undefined}
	if got := getFuncName(stack, 1); got != "" {
		t.Fatalf("name = %q, want empty", got)
	}
}

func TestGetFuncNameHandlesNilObjectImpl(t *testing.T) {
	obj := &Object{}
	stack := []Value{obj, _undefined}
	if got := getFuncName(stack, 1); got != "" {
		t.Fatalf("name = %q, want empty", got)
	}
}

func TestGetFuncNamePreservesNonStringDataName(t *testing.T) {
	rt := New()
	fn, err := rt.RunString(`(function originalName() {})`)
	if err != nil {
		t.Fatal(err)
	}
	fnObj := fn.(*Object)
	fnObj.self._putProp("name", valueInt(123), false, false, true)
	if got := getFuncName([]Value{fnObj, _undefined}, 1); got != unistring.String("123") {
		t.Fatalf("name = %q, want 123", got)
	}
}

func TestGetFuncNameIgnoresAccessorAndProxyTraps(t *testing.T) {
	rt := New()
	reads := 0
	if err := rt.Set("nameRead", func() { reads++ }); err != nil {
		t.Fatal(err)
	}
	fn, err := rt.RunString(`
		(function named() {})
	`)
	if err != nil {
		t.Fatal(err)
	}
	fnObj := fn.(*Object)
	if !fnObj.defineOwnProperty(asciiString("name"), PropertyDescriptor{
		Getter:       rt.Get("nameRead"),
		Configurable: FLAG_TRUE,
	}, true) {
		t.Fatal("could not install name getter")
	}
	if got := getFuncName([]Value{fnObj, _undefined}, 1); got != "" {
		t.Fatalf("accessor name = %q, want empty", got)
	}
	if reads != 0 {
		t.Fatalf("name getter ran %d times during stack capture", reads)
	}

	traps := 0
	if err := rt.Set("descriptorTrap", func() { traps++ }); err != nil {
		t.Fatal(err)
	}
	proxy, err := rt.RunString(`
		new Proxy(function target() {}, {
			getOwnPropertyDescriptor(target, key) {
				descriptorTrap();
				return Reflect.getOwnPropertyDescriptor(target, key);
			}
		})
	`)
	if err != nil {
		t.Fatal(err)
	}
	if got := getFuncName([]Value{proxy, _undefined}, 1); got != "proxy" {
		t.Fatalf("proxy name = %q, want proxy", got)
	}
	if traps != 0 {
		t.Fatalf("proxy descriptor trap ran %d times during stack capture", traps)
	}
}
