package goja

import "testing"

// Keep this assertion independent of the embedding fork's queue and Abort
// APIs so the generator/iterator regressions can accompany upstream fixes.
func assertGeneratorBoundaryClean(t *testing.T, rt *Runtime) {
	t.Helper()
	if rt.vm.prg != nil || rt.vm.sb != -1 || rt.vm.sp != 0 || len(rt.vm.stack) != 0 ||
		len(rt.vm.callStack) != 0 || len(rt.vm.tryStack) != 0 || len(rt.vm.iterStack) != 0 || len(rt.vm.refStack) != 0 {
		t.Fatalf("dirty generator boundary: prg=%v sb=%d sp=%d stack=%d call=%d try=%d iter=%d ref=%d",
			rt.vm.prg, rt.vm.sb, rt.vm.sp, len(rt.vm.stack), len(rt.vm.callStack), len(rt.vm.tryStack),
			len(rt.vm.iterStack), len(rt.vm.refStack))
	}
}
