package goja

import (
	"errors"
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

func TestGetFuncNameOutOfBoundsAndNonPrimitiveDataNames(t *testing.T) {
	if got := getFuncName(nil, 1); got != "" {
		t.Fatalf("nil stack name = %q, want empty", got)
	}
	if got := getFuncName([]Value{_undefined}, 2); got != "" {
		t.Fatalf("out-of-bounds stack name = %q, want empty", got)
	}

	rt := New()
	coercions := 0
	if err := rt.Set("recordCoercion", func() { coercions++ }); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("abort with object name")
	if err := rt.Set("targetFn", func() {
		if frames := rt.CaptureCallStack(0, nil); len(frames) == 0 {
			t.Error("expected call stack frames")
		}
		rt.Abort(cause)
		_ = rt.NewGoError(cause)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RunString(`
		const badName = new Proxy({
			toString() { recordCoercion(); return "coerced"; },
			[Symbol.toPrimitive]() { recordCoercion(); return "coerced"; }
		}, {
			get(target, key, receiver) {
				recordCoercion();
				return Reflect.get(target, key, receiver);
			}
		});
		Object.defineProperty(targetFn, "name", { value: badName, configurable: true });
	`); err != nil {
		t.Fatal(err)
	}
	if got := getFuncName([]Value{rt.Get("targetFn"), _undefined}, 1); got != "" {
		t.Fatalf("object data name = %q, want empty", got)
	}
	_, err := rt.RunString(`targetFn(); 1 + 1`)
	requireAbortCause(t, err, cause)
	if coercions != 0 {
		t.Fatalf("stack capture ran %d object/proxy coercions", coercions)
	}

	rtSym := New()
	symCause := errors.New("abort with symbol name")
	if err := rtSym.Set("symFn", func() {
		rtSym.Abort(symCause)
		_, innerErr := rtSym.RunString(`try { for (;;) {} } catch (e) {}`)
		requireAbortCause(t, innerErr, symCause)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rtSym.RunString(`
		Object.defineProperty(symFn, "name", { value: Symbol("boom"), configurable: true });
	`); err != nil {
		t.Fatal(err)
	}
	if got := getFuncName([]Value{rtSym.Get("symFn"), _undefined}, 1); got != "" {
		t.Fatalf("symbol data name = %q, want empty", got)
	}
	_, err = rtSym.RunString(`try { symFn(); } catch (e) {}`)
	requireAbortCause(t, err, symCause)
}
