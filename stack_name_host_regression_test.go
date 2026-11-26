package goja

import (
	"errors"
	"testing"
)

type stackNameDynamicProbe struct {
	get func(string) Value
}

func (d *stackNameDynamicProbe) Get(key string) Value { return d.get(key) }
func (*stackNameDynamicProbe) Set(string, Value) bool { return true }
func (*stackNameDynamicProbe) Has(string) bool        { return false }
func (*stackNameDynamicProbe) Delete(string) bool     { return true }
func (*stackNameDynamicProbe) Keys() []string         { return nil }

func TestStackNameDoesNotReadDynamicPrototype(t *testing.T) {
	for _, ownUndefined := range []bool{false, true} {
		name := "deleted"
		if ownUndefined {
			name = "undefined"
		}
		t.Run(name, func(t *testing.T) {
			rt := New()
			reads := 0
			proto := rt.NewDynamicObject(&stackNameDynamicProbe{get: func(string) Value {
				reads++
				panic("stack capture called a dynamic property handler")
			}})
			if err := rt.Set("native", func() { rt.CaptureCallStack(0, nil) }); err != nil {
				t.Fatal(err)
			}
			fn := rt.Get("native").(*Object)
			if ownUndefined {
				if err := fn.DefineDataProperty("name", Undefined(), FLAG_FALSE, FLAG_TRUE, FLAG_FALSE); err != nil {
					t.Fatal(err)
				}
			} else if err := fn.Delete("name"); err != nil {
				t.Fatal(err)
			}
			if err := fn.SetPrototype(proto); err != nil {
				t.Fatal(err)
			}
			want := ""
			if ownUndefined {
				want = "undefined"
			}
			if got := getFuncName([]Value{fn, _undefined}, 1).String(); got != want {
				t.Fatalf("name = %q, want %q", got, want)
			}
			if _, err := rt.RunString(`native()`); err != nil {
				t.Fatal(err)
			}
			if reads != 0 {
				t.Fatalf("diagnostic lookup called the dynamic handler %d times", reads)
			}
		})
	}
}

func TestStackNameAbortDoesNotRecursivelyReenterDynamicHandler(t *testing.T) {
	for _, ownUndefined := range []bool{false, true} {
		rt := New()
		cause := errors.New("terminal abort")
		entries := 0
		proto := rt.NewDynamicObject(&stackNameDynamicProbe{get: func(key string) Value {
			if key != "name" {
				return nil
			}
			entries++
			if entries > 4 {
				panic("recursive stack capture")
			}
			value, err := rt.RunString(`"fallback name"`)
			if err != nil {
				panic(err)
			}
			return value
		}})
		if err := rt.Set("native", func() { rt.Abort(cause); rt.NewGoError(cause) }); err != nil {
			t.Fatal(err)
		}
		fn := rt.Get("native").(*Object)
		if ownUndefined {
			if err := fn.DefineDataProperty("name", Undefined(), FLAG_FALSE, FLAG_TRUE, FLAG_FALSE); err != nil {
				t.Fatal(err)
			}
		} else if err := fn.Delete("name"); err != nil {
			t.Fatal(err)
		}
		if err := fn.SetPrototype(proto); err != nil {
			t.Fatal(err)
		}
		_, err := rt.RunString(`native()`)
		requireAbortCause(t, err, cause)
		if entries != 0 {
			t.Fatalf("stack capture reentered a dynamic handler %d times", entries)
		}
	}
}

func TestStackNameInheritedOrdinaryProperties(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
		want string
	}{
		{"data", `({name: "inherited"})`, "inherited"},
		{"number", `({name: 123})`, "123"},
		{"boolean", `({name: false})`, "false"},
		{"null", `({name: null})`, "null"},
		{"undefined_shadows_parent", `Object.assign(Object.create({name:"wrong"}), {name:undefined})`, "undefined"},
		{"accessor", `({get name() { touched(); throw "must not run"; }})`, ""},
		{"object_value", `({name: {toString(){touched(); throw "must not run";}}})`, ""},
		{"symbol_value", `({name: Symbol("must not stringify")})`, ""},
		{"array", `Object.assign([], {name: "array parent"})`, "array parent"},
		{"promise", `Object.assign(Promise.resolve(), {name: "promise parent"})`, "promise parent"},
		{"error", `Object.assign(new Error(), {name: "error parent"})`, "error parent"},
		{"regexp", `Object.assign(/x/, {name: "regexp parent"})`, "regexp parent"},
		{"proxy", `new Proxy({}, {get(){touched();}, getOwnPropertyDescriptor(){touched();}, getPrototypeOf(){touched();}})`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := New()
			touches := 0
			if err := rt.Set("touched", func() { touches++ }); err != nil {
				t.Fatal(err)
			}
			fn, err := rt.RunString(`(function original(){})`)
			if err != nil {
				t.Fatal(err)
			}
			proto, err := rt.RunString(tc.code)
			if err != nil {
				t.Fatal(err)
			}
			f := fn.(*Object)
			if err := f.Delete("name"); err != nil {
				t.Fatal(err)
			}
			if err := f.SetPrototype(proto.(*Object)); err != nil {
				t.Fatal(err)
			}
			// SetPrototype itself may check a proxy; only capture is under test.
			touches = 0
			if got := getFuncName([]Value{f, _undefined}, 1).String(); got != tc.want {
				t.Fatalf("name = %q, want %q", got, tc.want)
			}
			if touches != 0 {
				t.Fatalf("stack name lookup invoked JavaScript %d times", touches)
			}
		})
	}
}

func TestStackNamePreservesLazyBuiltinNames(t *testing.T) {
	for _, name := range []string{"Object", "Number", "Date", "BigInt"} {
		rt := New()
		if got := getFuncName([]Value{rt.Get(name), _undefined}, 1).String(); got != name {
			t.Fatalf("lazy builtin %s name = %q", name, got)
		}
	}
	rt := New()
	fn := rt.ToValue(func() {}).(*Object)
	if err := fn.Delete("name"); err != nil {
		t.Fatal(err)
	}
	if err := fn.SetPrototype(rt.Get("BigInt").(*Object)); err != nil {
		t.Fatal(err)
	}
	if got := getFuncName([]Value{fn, _undefined}, 1); got != "BigInt" {
		t.Fatalf("inherited lazy builtin name = %q", got)
	}
}

func TestStackNameDoesNotConvertHostPrototypeValues(t *testing.T) {
	rt := New()
	// Reading the virtual field would attempt an illegal cross-runtime value
	// conversion. Diagnostic lookup must stop at the host object instead.
	proto := rt.ToValue(map[string]interface{}{"name": New().NewObject()}).(*Object)
	fn := rt.ToValue(func() {}).(*Object)
	if err := fn.Delete("name"); err != nil {
		t.Fatal(err)
	}
	if err := fn.SetPrototype(proto); err != nil {
		t.Fatal(err)
	}
	if got := getFuncName([]Value{fn, _undefined}, 1); got != "" {
		t.Fatalf("host prototype name = %q", got)
	}
}

func TestStackNameDestructWrapperCycleIsBounded(t *testing.T) {
	rt := New()
	wrapper := rt.newDestructKeyedSource(rt.ToValue(func() {}))
	wrapper.self.(*destructKeyedSource).wrapped = wrapper
	if got := getFuncName([]Value{wrapper, _undefined}, 1); got != "" {
		t.Fatalf("cyclic wrapper name = %q", got)
	}
}
