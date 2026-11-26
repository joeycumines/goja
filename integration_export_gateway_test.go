package goja

import (
	"errors"
	"testing"
)

func TestReviewExportedFunctionErrorIdentity(t *testing.T) {
	rt := New()
	original := errors.New("original error")
	replacement := errors.New("replacement error")
	if err := rt.Set("nativeError", rt.NewGoError(original)); err != nil {
		t.Fatal(err)
	}
	if err := rt.Set("replacement", replacement); err != nil {
		t.Fatal(err)
	}
	fn, err := rt.RunString(`nativeError.value=replacement; (function(){ throw nativeError; })`)
	if err != nil {
		t.Fatal(err)
	}
	var gateway func() error
	if err := rt.ExportTo(fn, &gateway); err != nil {
		t.Fatal(err)
	}
	got := gateway()
	t.Logf("got=%v is_original=%v is_replacement=%v", got, errors.Is(got, original), errors.Is(got, replacement))
	if !errors.Is(got, original) {
		t.Fatalf("exported function lost original native error: %v", got)
	}
}

func TestReviewExportedFunctionTerminalErrorGetter(t *testing.T) {
	rt := New()
	original := errors.New("original error")
	cause := errors.New("terminal abort")
	nativeError := rt.NewGoError(original)
	if err := rt.Set("nativeError", nativeError); err != nil {
		t.Fatal(err)
	}
	if err := rt.Set("throwNow", func() { rt.Abort(cause); panic(nativeError) }); err != nil {
		t.Fatal(err)
	}
	fn, err := rt.RunString(`Object.defineProperty(nativeError,"value",{get(){return "should not run";}}); (function(){ throwNow(); })`)
	if err != nil {
		t.Fatal(err)
	}
	var gateway func() error
	if err := rt.ExportTo(fn, &gateway); err != nil {
		t.Fatal(err)
	}
	var got error
	var caught interface{}
	func() { defer func() { caught = recover() }(); got = gateway() }()
	t.Logf("got=%v panic=%v sp=%d calls=%d tries=%d", got, caught, rt.vm.sp, len(rt.vm.callStack), len(rt.vm.tryStack))
	if caught != nil || !errors.Is(got, original) {
		t.Fatalf("exported function reentered JS during error conversion: got=%v panic=%v", got, caught)
	}
}
