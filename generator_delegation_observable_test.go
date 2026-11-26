package goja

import "testing"

func TestGeneratorDelegatedResultObservability(t *testing.T) {
	for name, source := range map[string]string{
		"done_twice_before_value": `
			const log = [];
			let n = 0;
			const iterator = { [Symbol.iterator]() { return this; }, next() {
				if (n++) return { done: true };
				return { get done() { log.push("done"); return false; }, get value() { log.push("value"); return 42; } };
			} };
			function* gen() { yield* iterator; }
			for (const value of gen()) log.push(value);
			if (JSON.stringify(log) !== '["done","done","value",42]') throw log;
		`,
		"consumer_done_can_change": `
			let reads = 0, values = 0;
			const iterator = { [Symbol.iterator]() { return this; }, next() {
				return { get done() { return ++reads > 1; }, get value() { values++; throw "unexpected value read"; } };
			} };
			function* gen() { yield* iterator; }
			let count = 0;
			for (const value of gen()) count++;
			if (count !== 0 || reads !== 2 || values !== 0) throw [count, reads, values];
		`,
		"consumer_getter_throw_keeps_generator_suspended": `
			let n = 0;
			const iterator = { [Symbol.iterator]() { return this; }, next() {
				if (n++) return { value: 12, done: true };
				return { done: false, get value() { throw "consumer"; } };
			} };
			function* gen() { yield* iterator; yield 42; }
			const g = gen();
			try { [...g]; throw "missing exception"; } catch (e) { if (e !== "consumer") throw e; }
			const resumed = g.next();
			if (resumed.done || resumed.value !== 42) throw resumed;
		`,
		"drop_does_not_read_skipped_value": `
			let n = 0, doneReads = 0, valueReads = 0;
			const iterator = { [Symbol.iterator]() { return this; }, next() {
				if (n++) return { done: true };
				return { get done() { doneReads++; return false; }, get value() { valueReads++; throw "unexpected value read"; } };
			} };
			function* gen() { yield* iterator; }
			const result = gen().drop(1).next();
			if (!result.done || doneReads !== 2 || valueReads !== 0) throw [result, doneReads, valueReads];
		`,
	} {
		t.Run(name, func(t *testing.T) {
			rt := New()
			if _, err := rt.RunString(source); err != nil {
				t.Fatal(err)
			}
			assertGeneratorBoundaryClean(t, rt)
		})
	}
}
