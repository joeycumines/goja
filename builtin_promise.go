package goja

import (
	"reflect"

	"github.com/joeycumines/goja/unistring"
)

type PromiseState int
type PromiseRejectionOperation int

type promiseReactionType int

const (
	PromiseStatePending PromiseState = iota
	PromiseStateFulfilled
	PromiseStateRejected
)

const (
	PromiseRejectionReject PromiseRejectionOperation = iota
	PromiseRejectionHandle
)

const (
	promiseReactionFulfill promiseReactionType = iota
	promiseReactionReject
)

type PromiseRejectionTracker func(p *Promise, operation PromiseRejectionOperation)

// PromiseJobEnqueuer is a host hook that receives promise jobs instead of
// queueing them internally (ECMA-262 HostEnqueuePromiseJob). The host is
// responsible for invoking each job in FIFO order via RunPromiseJob.
//
// The hook runs on the runtime's goroutine. Jobs should be captured and run
// via RunPromiseJob only after the current turn (RunString/RunProgram) returns.
// Synchronous invocation from within the hook is supported but does not
// preserve ECMA-262 turn ordering.
//
// If set while jobs are already pending in the internal queue, they are
// forwarded to the hook when the current turn exits. Jobs enqueued after the
// hook is set are delivered directly during the turn, so they reach the hook
// before the forwarded (older) jobs. Install the enqueuer before enqueuing any
// jobs if global FIFO ordering is required.
//
// A hook invocation is considered a delivery even if it panics and is not
// retried. Reactions not yet delivered are retained for the runtime's exit
// processing. On an abrupt exit they are forwarded to the current hook without
// being executed by the runtime; additional hook panics during that cleanup do
// not replace the failure already being propagated.
//
// Set to nil (the default) to restore the internal queue.
// Register via Runtime.SetPromiseJobEnqueuer.
type PromiseJobEnqueuer func(job func())

type jobCallback struct {
	callback func(FunctionCall) Value
}

type promiseCapability struct {
	promise               *Object
	resolveObj, rejectObj *Object
}

type promiseReaction struct {
	capability   *promiseCapability
	typ          promiseReactionType
	handler      *jobCallback
	asyncRunner  *asyncRunner
	asyncCtx     interface{}
	asyncTracker AsyncContextTracker
}

var typePromise = reflect.TypeOf((*Promise)(nil))

// Promise is a Go wrapper around ECMAScript Promise. Calling Runtime.ToValue() on it
// returns the underlying Object. Calling Export() on a Promise Object returns a Promise.
//
// Use Runtime.NewPromise() to create one. Calling Runtime.ToValue() on a zero object or nil returns null Value.
//
// WARNING: Instances of Promise are not goroutine-safe. See Runtime.NewPromise() for more details.
type Promise struct {
	baseObject
	state               PromiseState
	result              Value
	fulfillReactions    []*promiseReaction
	rejectReactions     []*promiseReaction
	handled             bool
	settlementObservers []func()
}

func (p *Promise) State() PromiseState {
	return p.state
}

func (p *Promise) Result() Value {
	return p.result
}

// ObserveSettlement registers a native observer for this Promise's actual
// fulfillment or rejection. Resolving to a thenable does not notify observers
// until the adopted result settles. Callbacks registered while the Promise is
// pending run synchronously in registration order after State and Result have
// been published, rejection tracking has been attempted, and due JavaScript
// reactions have been scheduled. Scheduling may retain jobs internally for
// later delivery if a host hook panics. An already-settled Promise invokes a
// newly registered observer immediately. This includes registration from a
// rejection tracker, enqueuer, or another observer, so such an observer can run
// before the pending observer batch. These callbacks do not add JavaScript
// reactions or mark a rejection as handled.
//
// Like the Promise itself, this method is not goroutine-safe. The callback must
// not execute JavaScript, panic, or call runtime.Goexit. An observer panic stops
// later observers without undoing settlement or preventing due reactions from
// being scheduled. The runtime's exit policy still governs execution or handoff
// of those jobs. The panic propagates through the surrounding runtime boundary,
// which can convert a JavaScript exception or uncatchable exception into a
// returned error. If a
// tracker or enqueuer panic is already unwinding, that original panic wins.
// Observer references are cleared before any observer is invoked. A nil
// callback panics.
func (p *Promise) ObserveSettlement(observer func()) {
	if observer == nil {
		panic("goja: nil Promise settlement observer")
	}
	if p.state != PromiseStatePending {
		observer()
		return
	}
	p.settlementObservers = append(p.settlementObservers, observer)
}

func (p *Promise) notifySettlement() {
	// This method is deferred by fulfill and reject. Preserve an earlier
	// scheduling/tracker failure even if an observer also panics.
	if original := recover(); original != nil {
		defer func() {
			_ = recover()
			panic(original)
		}()
	}
	observers := p.settlementObservers
	p.settlementObservers = nil
	for _, observer := range observers {
		observer()
	}
}

func (p *Promise) toValue(r *Runtime) Value {
	if p == nil || p.val == nil {
		return _null
	}
	promise := p.val
	if promise.runtime != r {
		panic(r.NewTypeError("Illegal runtime transition of a Promise"))
	}
	return promise
}

func (p *Promise) createResolvingFunctions() (resolve, reject *Object) {
	r := p.val.runtime
	alreadyResolved := false
	return p.val.runtime.newNativeFunc(func(call FunctionCall) Value {
			if alreadyResolved || p.state != PromiseStatePending {
				return _undefined
			}
			alreadyResolved = true
			resolution := call.Argument(0)
			if resolution.SameAs(p.val) {
				return p.reject(r.NewTypeError("Promise self-resolution"))
			}
			if obj, ok := resolution.(*Object); ok {
				var thenAction Value
				ex := r.vm.try(func() {
					thenAction = obj.self.getStr("then", nil)
				})
				if ex != nil {
					return p.reject(ex.val)
				}
				// A native `then` getter can terminally settle the promise
				// while this lookup runs; do not adopt an already-settled result.
				if p.state != PromiseStatePending {
					return _undefined
				}
				if call, ok := assertCallable(thenAction); ok {
					job := r.newPromiseResolveThenableJob(p, resolution, &jobCallback{callback: call})
					r.enqueuePromiseJob(job)
					return _undefined
				}
			}
			return p.fulfill(resolution)
		}, "", 1),
		p.val.runtime.newNativeFunc(func(call FunctionCall) Value {
			if alreadyResolved || p.state != PromiseStatePending {
				return _undefined
			}
			alreadyResolved = true
			reason := call.Argument(0)
			return p.reject(reason)
		}, "", 1)
}

func (p *Promise) reject(reason Value) Value {
	// A native host may dispose of an adopted Promise after Runtime.Abort.
	// A resolving pair retained by that thenable must not replace the result.
	if p.state != PromiseStatePending {
		return _undefined
	}
	reactions := p.rejectReactions
	p.result = reason
	p.fulfillReactions, p.rejectReactions = nil, nil
	p.state = PromiseStateRejected
	// Notify the pending observer batch after scheduling, including when a
	// host callback panics. A failed tracker must not discard due reactions.
	defer p.notifySettlement()
	r := p.val.runtime
	defer func() {
		if original := recover(); original != nil {
			r.bufferPromiseReactions(reactions, reason)
			panic(original)
		}
		r.triggerPromiseReactions(reactions, reason)
	}()
	if !p.handled {
		r.trackPromiseRejection(p, PromiseRejectionReject)
	}
	return _undefined
}

func (p *Promise) fulfill(value Value) Value {
	if p.state != PromiseStatePending {
		return _undefined
	}
	reactions := p.fulfillReactions
	p.result = value
	p.fulfillReactions, p.rejectReactions = nil, nil
	p.state = PromiseStateFulfilled
	// triggerPromiseReactions retains an unattempted tail if a hook panics;
	// notifySettlement preserves that failure while notifying observers.
	defer p.notifySettlement()
	p.val.runtime.triggerPromiseReactions(reactions, value)
	return _undefined
}

func (p *Promise) exportType() reflect.Type {
	return typePromise
}

func (p *Promise) export(*objectExportCtx) interface{} {
	return p
}

func (p *Promise) addReactions(fulfillReaction *promiseReaction, rejectReaction *promiseReaction) {
	r := p.val.runtime
	if tracker := r.asyncContextTracker; tracker != nil {
		ctx := tracker.Grab()
		fulfillReaction.asyncCtx = ctx
		fulfillReaction.asyncTracker = tracker
		rejectReaction.asyncCtx = ctx
		rejectReaction.asyncTracker = tracker
	}
	switch p.state {
	case PromiseStatePending:
		p.fulfillReactions = append(p.fulfillReactions, fulfillReaction)
		p.rejectReactions = append(p.rejectReactions, rejectReaction)
		p.handled = true
	case PromiseStateFulfilled:
		p.handled = true
		r.enqueuePromiseJob(r.newPromiseReactionJob(fulfillReaction, p.result))
	default:
		reason := p.result
		wasHandled := p.handled
		// Grab has succeeded and this reaction is now registered. Commit
		// the transition before callbacks so tracker reentry or a later
		// hook panic cannot produce another first-handle notification.
		p.handled = true
		if !wasHandled {
			func() {
				defer func() {
					if original := recover(); original != nil {
						// Registration has committed even if its notification
						// fails. Let the enclosing boundary restore the VM
						// before transferring this still-unattempted job.
						r.bufferPromiseReactions([]*promiseReaction{rejectReaction}, reason)
						panic(original)
					}
				}()
				r.trackPromiseRejection(p, PromiseRejectionHandle)
			}()
		}
		r.enqueuePromiseJob(r.newPromiseReactionJob(rejectReaction, reason))
	}
}

func (r *Runtime) newPromiseResolveThenableJob(p *Promise, thenable Value, then *jobCallback) func() {
	return func() {
		if p.state != PromiseStatePending {
			return
		}
		resolve, reject := p.createResolvingFunctions()
		ex := r.vm.try(func() {
			r.callJobCallback(then, thenable, resolve, reject)
		})
		if ex != nil {
			if fn, ok := reject.self.assertCallable(); ok {
				fn(FunctionCall{This: _undefined, Arguments: []Value{ex.val}})
			}
		}
	}
}

func (r *Runtime) enqueuePromiseJob(job func()) {
	// While a tracker scope is active (between Resumed and Exited in
	// newPromiseReactionJob), buffer jobs internally instead of delivering
	// them to a synchronous hook. A synchronous hook calling RunPromiseJob
	// would run the new reaction while the outer tracker is still resumed,
	// causing nested Resumed() calls. Once buffered jobs exist, later jobs
	// are also buffered to preserve FIFO.
	if hook := r.promiseJobEnqueuer; hook != nil && !r.asyncTrackerActive && !r.promiseJobQueuePriority {
		hook(job)
		return
	}
	if r.promiseJobEnqueuer == nil && r.vm.isAborted() {
		return
	}
	r.jobQueue = append(r.jobQueue, job)
	if r.asyncTrackerActive {
		r.promiseJobQueuePriority = true
	}
}

func (r *Runtime) triggerPromiseReactions(reactions []*promiseReaction, argument Value) {
	next := 0
	defer func() {
		if original := recover(); original != nil {
			// Do not call more host hooks while unwinding their failure: a
			// hook may have entered JavaScript through a direct Value API.
			// Retain the remaining jobs before notifying observers; the
			// enclosing runtime boundary restores the VM before forwarding.
			r.bufferPromiseReactions(reactions[next:], argument)
			panic(original)
		}
	}()
	for i, reaction := range reactions {
		next = i + 1 // An attempted delivery must not be repeated.
		r.enqueuePromiseJob(r.newPromiseReactionJob(reaction, argument))
	}
}

func (r *Runtime) bufferPromiseReactions(reactions []*promiseReaction, argument Value) {
	for _, reaction := range reactions {
		r.jobQueue = append(r.jobQueue, r.newPromiseReactionJob(reaction, argument))
	}
	if len(reactions) > 0 {
		r.promiseJobQueuePriority = true
	}
}

func (r *Runtime) newPromiseReactionJob(reaction *promiseReaction, argument Value) func() {
	return func() {
		var handlerResult Value
		fulfill := false
		// Set up the tracker before checking for a handler so that
		// nil-handler propagation reactions also observe the
		// Grab/Resumed/Exited invariant.
		var tracker AsyncContextTracker
		var exited bool
		exitTracker := func() {
			if tracker != nil && !exited {
				exited = true
				defer func() {
					r.asyncTrackerActive = false
				}()
				tracker.Exited()
			}
		}
		if t := reaction.asyncTracker; t != nil {
			tracker = t
			r.asyncTrackerActive = true
			resumed := false
			defer func() {
				if !resumed {
					r.asyncTrackerActive = false
				}
			}()
			tracker.Resumed(reaction.asyncCtx)
			resumed = true
			defer exitTracker()
		}
		if reaction.handler == nil {
			handlerResult = argument
			if reaction.typ == promiseReactionFulfill {
				fulfill = true
			}
		} else {
			ex := r.vm.try(func() {
				handlerResult = r.callJobCallback(reaction.handler, _undefined, argument)
				fulfill = true
			})
			if ex != nil {
				handlerResult = ex.val
			}
		}
		// Call Exited before resolving/rejecting the capability so the
		// tracker brackets only the handler, not downstream resolution
		// (thenable assimilation, reaction triggering). The defer above
		// is a safety net for panics that skip this call.
		//
		// asyncTrackerActive stays true until Exited() returns so that
		// promise work enqueued by Exited() itself is buffered rather
		// than delivered to a synchronous hook.
		exitTracker()
		if tracker != nil && len(r.jobQueue) > 0 {
			// Keep hook delivery paused through capability resolution
			// so downstream reactions are appended behind the buffered
			// jobs instead of overtaking them.
			previousAsyncTrackerActive := r.asyncTrackerActive
			r.asyncTrackerActive = true
			defer func() {
				r.asyncTrackerActive = previousAsyncTrackerActive
			}()
		}
		if reaction.capability != nil {
			if fulfill {
				reaction.capability.resolve(handlerResult)
			} else {
				reaction.capability.reject(handlerResult)
			}
		}
	}
}

func (r *Runtime) newPromise(proto *Object) *Promise {
	o := &Object{runtime: r}

	po := &Promise{}
	po.class = classObject
	po.val = o
	po.extensible = true
	o.self = po
	po.prototype = proto
	po.init()
	return po
}

func (r *Runtime) builtin_newPromise(args []Value, newTarget *Object) *Object {
	if newTarget == nil {
		panic(r.needNew("Promise"))
	}
	var arg0 Value
	if len(args) > 0 {
		arg0 = args[0]
	}
	executor := r.toCallable(arg0)

	proto := r.getPrototypeFromCtor(newTarget, r.global.Promise, r.getPromisePrototype())
	po := r.newPromise(proto)

	resolve, reject := po.createResolvingFunctions()
	ex := r.vm.try(func() {
		executor(FunctionCall{This: _undefined, Arguments: []Value{resolve, reject}})
	})
	if ex != nil {
		if fn, ok := reject.self.assertCallable(); ok {
			fn(FunctionCall{This: _undefined, Arguments: []Value{ex.val}})
		}
	}
	return po.val
}

func (r *Runtime) promiseProto_then(call FunctionCall) Value {
	thisObj := r.toObject(call.This)
	if p, ok := thisObj.self.(*Promise); ok {
		c := r.speciesConstructorObj(thisObj, r.getPromise())
		resultCapability := r.newPromiseCapability(c)
		return r.performPromiseThen(p, call.Argument(0), call.Argument(1), resultCapability)
	}
	panic(r.NewTypeError("Method Promise.prototype.then called on incompatible receiver %s", r.objectproto_toString(FunctionCall{This: thisObj})))
}

func (r *Runtime) newPromiseCapability(c *Object) *promiseCapability {
	pcap := new(promiseCapability)
	if c == r.getPromise() {
		p := r.newPromise(r.getPromisePrototype())
		pcap.resolveObj, pcap.rejectObj = p.createResolvingFunctions()
		pcap.promise = p.val
	} else {
		var resolve, reject Value
		executor := r.newNativeFunc(func(call FunctionCall) Value {
			if resolve != nil {
				panic(r.NewTypeError("resolve is already set"))
			}
			if reject != nil {
				panic(r.NewTypeError("reject is already set"))
			}
			if arg := call.Argument(0); arg != _undefined {
				resolve = arg
			}
			if arg := call.Argument(1); arg != _undefined {
				reject = arg
			}
			return nil
		}, "", 2)
		pcap.promise = r.toConstructor(c)([]Value{executor}, c)
		pcap.resolveObj = r.toObject(resolve)
		r.toCallable(pcap.resolveObj) // make sure it's callable
		pcap.rejectObj = r.toObject(reject)
		r.toCallable(pcap.rejectObj)
	}
	return pcap
}

func (r *Runtime) performPromiseThen(p *Promise, onFulfilled, onRejected Value, resultCapability *promiseCapability) Value {
	var onFulfilledJobCallback, onRejectedJobCallback *jobCallback
	if f, ok := assertCallable(onFulfilled); ok {
		onFulfilledJobCallback = &jobCallback{callback: f}
	}
	if f, ok := assertCallable(onRejected); ok {
		onRejectedJobCallback = &jobCallback{callback: f}
	}
	fulfillReaction := &promiseReaction{
		capability: resultCapability,
		typ:        promiseReactionFulfill,
		handler:    onFulfilledJobCallback,
	}
	rejectReaction := &promiseReaction{
		capability: resultCapability,
		typ:        promiseReactionReject,
		handler:    onRejectedJobCallback,
	}
	p.addReactions(fulfillReaction, rejectReaction)
	if resultCapability == nil {
		return _undefined
	}
	return resultCapability.promise
}

func (r *Runtime) promiseProto_catch(call FunctionCall) Value {
	return r.invoke(call.This, "then", _undefined, call.Argument(0))
}

func (r *Runtime) promiseResolve(c *Object, x Value) *Object {
	if obj, ok := x.(*Object); ok {
		if _, ok := obj.self.(*Promise); ok {
			xConstructor := nilSafe(obj.self.getStr("constructor", nil))
			if xConstructor.SameAs(c) {
				return obj
			}
		}
	}
	pcap := r.newPromiseCapability(c)
	pcap.resolve(x)
	return pcap.promise
}

func (r *Runtime) promiseProto_finally(call FunctionCall) Value {
	promise := r.toObject(call.This)
	c := r.speciesConstructorObj(promise, r.getPromise())
	onFinally := call.Argument(0)
	var thenFinally, catchFinally Value
	if onFinallyFn, ok := assertCallable(onFinally); !ok {
		thenFinally, catchFinally = onFinally, onFinally
	} else {
		thenFinally = r.newNativeFunc(func(call FunctionCall) Value {
			value := call.Argument(0)
			result := onFinallyFn(FunctionCall{This: _undefined})
			promise := r.promiseResolve(c, result)
			valueThunk := r.newNativeFunc(func(call FunctionCall) Value {
				return value
			}, "", 0)
			return r.invoke(promise, "then", valueThunk)
		}, "", 1)

		catchFinally = r.newNativeFunc(func(call FunctionCall) Value {
			reason := call.Argument(0)
			result := onFinallyFn(FunctionCall{This: _undefined})
			promise := r.promiseResolve(c, result)
			thrower := r.newNativeFunc(func(call FunctionCall) Value {
				panic(reason)
			}, "", 0)
			return r.invoke(promise, "then", thrower)
		}, "", 1)
	}
	return r.invoke(promise, "then", thenFinally, catchFinally)
}

func (pcap *promiseCapability) resolve(result Value) {
	pcap.promise.runtime.toCallable(pcap.resolveObj)(FunctionCall{This: _undefined, Arguments: []Value{result}})
}

func (pcap *promiseCapability) reject(reason Value) {
	pcap.promise.runtime.toCallable(pcap.rejectObj)(FunctionCall{This: _undefined, Arguments: []Value{reason}})
}

func (pcap *promiseCapability) try(f func()) bool {
	ex := pcap.promise.runtime.vm.try(f)
	if ex != nil {
		pcap.reject(ex.val)
		return false
	}
	return true
}

func (r *Runtime) promise_all(call FunctionCall) Value {
	c := r.toObject(call.This)
	pcap := r.newPromiseCapability(c)

	pcap.try(func() {
		promiseResolve := r.toCallable(c.self.getStr("resolve", nil))
		iter := r.getIterator(call.Argument(0), nil)
		var values []Value
		remainingElementsCount := 1
		iter.iterate(func(nextValue Value) {
			index := len(values)
			values = append(values, _undefined)
			nextPromise := promiseResolve(FunctionCall{This: c, Arguments: []Value{nextValue}})
			alreadyCalled := false
			onFulfilled := r.newNativeFunc(func(call FunctionCall) Value {
				if alreadyCalled {
					return _undefined
				}
				alreadyCalled = true
				values[index] = call.Argument(0)
				remainingElementsCount--
				if remainingElementsCount == 0 {
					pcap.resolve(r.newArrayValues(values))
				}
				return _undefined
			}, "", 1)
			remainingElementsCount++
			r.invoke(nextPromise, "then", onFulfilled, pcap.rejectObj)
		})
		remainingElementsCount--
		if remainingElementsCount == 0 {
			pcap.resolve(r.newArrayValues(values))
		}
	})
	return pcap.promise
}

func (r *Runtime) promise_allSettled(call FunctionCall) Value {
	c := r.toObject(call.This)
	pcap := r.newPromiseCapability(c)

	pcap.try(func() {
		promiseResolve := r.toCallable(c.self.getStr("resolve", nil))
		iter := r.getIterator(call.Argument(0), nil)
		var values []Value
		remainingElementsCount := 1
		iter.iterate(func(nextValue Value) {
			index := len(values)
			values = append(values, _undefined)
			nextPromise := promiseResolve(FunctionCall{This: c, Arguments: []Value{nextValue}})
			alreadyCalled := false
			reaction := func(status Value, valueKey unistring.String) *Object {
				return r.newNativeFunc(func(call FunctionCall) Value {
					if alreadyCalled {
						return _undefined
					}
					alreadyCalled = true
					obj := r.NewObject()
					obj.self._putProp("status", status, true, true, true)
					obj.self._putProp(valueKey, call.Argument(0), true, true, true)
					values[index] = obj
					remainingElementsCount--
					if remainingElementsCount == 0 {
						pcap.resolve(r.newArrayValues(values))
					}
					return _undefined
				}, "", 1)
			}
			onFulfilled := reaction(asciiString("fulfilled"), "value")
			onRejected := reaction(asciiString("rejected"), "reason")
			remainingElementsCount++
			r.invoke(nextPromise, "then", onFulfilled, onRejected)
		})
		remainingElementsCount--
		if remainingElementsCount == 0 {
			pcap.resolve(r.newArrayValues(values))
		}
	})
	return pcap.promise
}

func (r *Runtime) promise_any(call FunctionCall) Value {
	c := r.toObject(call.This)
	pcap := r.newPromiseCapability(c)

	pcap.try(func() {
		promiseResolve := r.toCallable(c.self.getStr("resolve", nil))
		iter := r.getIterator(call.Argument(0), nil)
		var errors []Value
		remainingElementsCount := 1
		iter.iterate(func(nextValue Value) {
			index := len(errors)
			errors = append(errors, _undefined)
			nextPromise := promiseResolve(FunctionCall{This: c, Arguments: []Value{nextValue}})
			alreadyCalled := false
			onRejected := r.newNativeFunc(func(call FunctionCall) Value {
				if alreadyCalled {
					return _undefined
				}
				alreadyCalled = true
				errors[index] = call.Argument(0)
				remainingElementsCount--
				if remainingElementsCount == 0 {
					pcap.reject(r.newAggregateErrorErrors(errors))
				}
				return _undefined
			}, "", 1)

			remainingElementsCount++
			r.invoke(nextPromise, "then", pcap.resolveObj, onRejected)
		})
		remainingElementsCount--
		if remainingElementsCount == 0 {
			pcap.reject(r.newAggregateErrorErrors(errors))
		}
	})
	return pcap.promise
}

func (r *Runtime) promise_race(call FunctionCall) Value {
	c := r.toObject(call.This)
	pcap := r.newPromiseCapability(c)

	pcap.try(func() {
		promiseResolve := r.toCallable(c.self.getStr("resolve", nil))
		iter := r.getIterator(call.Argument(0), nil)
		iter.iterate(func(nextValue Value) {
			nextPromise := promiseResolve(FunctionCall{This: c, Arguments: []Value{nextValue}})
			r.invoke(nextPromise, "then", pcap.resolveObj, pcap.rejectObj)
		})
	})
	return pcap.promise
}

func (r *Runtime) promise_reject(call FunctionCall) Value {
	pcap := r.newPromiseCapability(r.toObject(call.This))
	pcap.reject(call.Argument(0))
	return pcap.promise
}

func (r *Runtime) promise_resolve(call FunctionCall) Value {
	return r.promiseResolve(r.toObject(call.This), call.Argument(0))
}

func (r *Runtime) promise_try(call FunctionCall) Value {
	c := r.toObject(call.This)
	var args []Value
	if len(call.Arguments) > 1 {
		args = call.Arguments[1:]
	}
	var result Value
	if ex := r.vm.try(func() {
		result = r.toCallable(call.Argument(0))(FunctionCall{This: _undefined, Arguments: args})
	}); ex != nil {
		pcap := r.newPromiseCapability(c)
		pcap.reject(ex.val)
		return pcap.promise
	}
	return r.promiseResolve(c, result)
}

func (r *Runtime) promise_withResolvers(call FunctionCall) Value {
	pcap := r.newPromiseCapability(r.toObject(call.This))
	obj := r.NewObject()
	obj.self._putProp("promise", pcap.promise, true, true, true)
	obj.self._putProp("resolve", pcap.resolveObj, true, true, true)
	obj.self._putProp("reject", pcap.rejectObj, true, true, true)
	return obj
}

func (r *Runtime) createPromiseProto(val *Object) objectImpl {
	o := newBaseObjectObj(val, r.global.ObjectPrototype, classObject)
	o._putProp("constructor", r.getPromise(), true, false, true)

	o._putProp("catch", r.newNativeFunc(r.promiseProto_catch, "catch", 1), true, false, true)
	o._putProp("finally", r.newNativeFunc(r.promiseProto_finally, "finally", 1), true, false, true)
	o._putProp("then", r.newNativeFunc(r.promiseProto_then, "then", 2), true, false, true)

	o._putSym(SymToStringTag, valueProp(asciiString(classPromise), false, false, true))

	return o
}

func (r *Runtime) createPromise(val *Object) objectImpl {
	o := r.newNativeConstructOnly(val, r.builtin_newPromise, r.getPromisePrototype(), "Promise", 1)

	o._putProp("all", r.newNativeFunc(r.promise_all, "all", 1), true, false, true)
	o._putProp("allSettled", r.newNativeFunc(r.promise_allSettled, "allSettled", 1), true, false, true)
	o._putProp("any", r.newNativeFunc(r.promise_any, "any", 1), true, false, true)
	o._putProp("race", r.newNativeFunc(r.promise_race, "race", 1), true, false, true)
	o._putProp("reject", r.newNativeFunc(r.promise_reject, "reject", 1), true, false, true)
	o._putProp("resolve", r.newNativeFunc(r.promise_resolve, "resolve", 1), true, false, true)
	o._putProp("try", r.newNativeFunc(r.promise_try, "try", 1), true, false, true)
	o._putProp("withResolvers", r.newNativeFunc(r.promise_withResolvers, "withResolvers", 0), true, false, true)

	r.putSpeciesReturnThis(o)

	return o
}

func (r *Runtime) getPromisePrototype() *Object {
	ret := r.global.PromisePrototype
	if ret == nil {
		ret = &Object{runtime: r}
		r.global.PromisePrototype = ret
		ret.self = r.createPromiseProto(ret)
	}
	return ret
}

func (r *Runtime) getPromise() *Object {
	ret := r.global.Promise
	if ret == nil {
		ret = &Object{runtime: r}
		r.global.Promise = ret
		ret.self = r.createPromise(ret)
	}
	return ret
}

func (r *Runtime) wrapPromiseReaction(p *Promise, fObj *Object, rejecting bool) func(interface{}) error {
	f, _ := fObj.self.assertCallable()
	return func(x interface{}) error {
		if rejecting && r.vm.isAborted() {
			// JavaScript can no longer finish thenable adoption after an
			// irreversible abort. Let the host dispose of the still-pending
			// native Promise even if its resolving pair was already claimed.
			// Completed results and ordinary Interrupt semantics are unchanged.
			return r.rejectAbortedPromise(p, x)
		}
		// Conversion can throw, for example for an Object from another
		// runtime. It belongs inside the same boundary as the resolver.
		return r.runWrapped(func() {
			f(FunctionCall{Arguments: []Value{r.ToValue(x)}})
		})
	}
}

// rejectAbortedPromise settles a still-pending native Promise after a terminal
// abort without evaluating JavaScript or running queued jobs. A configured
// PromiseJobEnqueuer still receives the resulting reaction jobs; the host owns
// their execution and RunPromiseJob rejects them with the abort cause.
func (r *Runtime) rejectAbortedPromise(p *Promise, x interface{}) (err error) {
	if p.state != PromiseStatePending {
		return nil
	}
	vm := r.vm
	frame := vm.saveExecutionFrame()
	defer func() {
		// A direct native-to-JavaScript call can alter registers such as pc
		// before pushing its own context. Restore the entire caller context,
		// not only the stacks restored by the try frame.
		vm.restoreExecutionFrame(frame)
		if frame.callStackLen == 0 && !r.asyncTrackerActive && !r.leavingAbrupt {
			// No runWrapped/leave boundary owns this native cleanup. Forward
			// any retained jobs without executing them and clear VM scratch
			// state, including tails left by a callback's direct Value API.
			r.leaveAbrupt()
		}
	}()
	defer func() {
		if recovered := recover(); recovered != nil {
			if ex := asUncatchableException(recovered); ex != nil {
				err = ex
			} else {
				panic(recovered)
			}
		}
	}()
	// Host callbacks can enter the interpreter through direct Value/Object
	// APIs. A try frame restores their caller's context on both catchable
	// exceptions and terminal interruption before the recovery above runs.
	if ex := r.vm.try(func() { p.reject(r.ToValue(x)) }); ex != nil {
		return ex
	}
	return nil
}

// NewPromise creates and returns a Promise and resolving functions for it.
// The resolving functions return errors raised during argument conversion or
// resolver execution, including *Exception, *InterruptedError, and
// *StackOverflowError. Such errors should be propagated upwards. A promise
// rejection itself is reported through [PromiseRejectionTracker], not as a
// returned error. Later job execution or forwarding follows the runtime's exit
// policy: an exit-time callback's JavaScript exception can still propagate as a
// panic. Other Go panics are re-thrown after runtime cleanup; hook failures
// during abrupt cleanup do not replace the original failure.
//
// After [Runtime.Abort], reject can also reject a still-pending Promise whose
// resolve function already adopted a thenable. This permits native terminal
// cleanup when JavaScript can no longer complete that adoption. It never
// changes an already fulfilled or rejected result. Ordinary [Runtime.Interrupt]
// does not enable this terminal override.
//
// WARNING: The returned values are not goroutine-safe and must not be called in parallel with VM running.
// In order to make use of this method you need an event loop such as the one in goja_nodejs (https://github.com/joeycumines/goja_nodejs)
// where it can be used like this:
//
//	loop := NewEventLoop()
//	loop.Start()
//	defer loop.Stop()
//	loop.RunOnLoop(func(vm *goja.Runtime) {
//	    p, resolve, _ := vm.NewPromise()
//	    vm.Set("p", p)
//	    go func() {
//	        time.Sleep(500 * time.Millisecond)   // or perform any other blocking operation
//	        loop.RunOnLoop(func(*goja.Runtime) { // resolve() must be called on the loop, cannot call it here
//	            err := resolve(result)
//	            // Handle uncatchable errors (e.g. by stopping the loop, panicking or setting a flag)
//	        })
//	    }()
//	}
func (r *Runtime) NewPromise() (promise *Promise, resolve, reject func(reason interface{}) error) {
	p := r.newPromise(r.getPromisePrototype())
	resolveF, rejectF := p.createResolvingFunctions()
	return p, r.wrapPromiseReaction(p, resolveF, false), r.wrapPromiseReaction(p, rejectF, true)
}

// SetPromiseRejectionTracker registers a function that will be called in two scenarios: when a promise is rejected
// without any handlers (with operation argument set to PromiseRejectionReject), and when a handler is added to a
// rejected promise for the first time (with operation argument set to PromiseRejectionHandle).
//
// Setting a tracker replaces any existing one. Setting it to nil disables the functionality.
// Once a reaction is registered, its promise remains handled even if the tracker
// or enqueuer panics; reentry does not repeat the first-handle notification.
//
// See https://tc39.es/ecma262/#sec-host-promise-rejection-tracker for more details.
func (r *Runtime) SetPromiseRejectionTracker(tracker PromiseRejectionTracker) {
	r.promiseRejectionTracker = tracker
}

// SetAsyncContextTracker registers a handler that allows to track async execution contexts. See AsyncContextTracker
// documentation for more details.
//
// Changing or clearing the tracker affects only reactions scheduled after the change.
// Pending reactions retain the tracker that was active when they were scheduled.
//
// This method (as Runtime in general) is not goroutine-safe.
func (r *Runtime) SetAsyncContextTracker(tracker AsyncContextTracker) {
	r.asyncContextTracker = tracker
}

// SetPromiseJobEnqueuer registers a hook that receives promise jobs instead of
// queueing them internally. See PromiseJobEnqueuer for the contract. nil
// restores the default behaviour. Not goroutine-safe (except Interrupt).
func (r *Runtime) SetPromiseJobEnqueuer(enqueuer PromiseJobEnqueuer) {
	r.promiseJobEnqueuer = enqueuer
}
