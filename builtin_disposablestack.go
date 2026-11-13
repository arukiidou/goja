package goja

// disposableResource is a DisposableResource Record. For the resources added by DisposableStack.prototype.adopt()
// and DisposableStack.prototype.defer() the value is undefined and the method is a closure calling onDispose.
//
// async is set if the hint is async-dispose. In this case method can be nil (if the value is null or undefined).
type disposableResource struct {
	value  Value
	method func(FunctionCall) Value
	async  bool
}

// disposeCapability is a DisposeCapability Record.
type disposeCapability struct {
	resources []disposableResource

	// state of an asynchronous DisposeResources() in progress
	needsAwait, hasAwaited bool
}

// createDisposableResource implements the part of AddDisposableResource(disposeCapability, V, hint) that creates
// the resource, where hint is async-dispose if async is true, sync-dispose otherwise.
// Returns false if no resource should be added (which is when v is null or undefined and the hint is sync-dispose).
func (r *Runtime) createDisposableResource(v Value, async bool) (res disposableResource, ok bool) {
	if v == nil || v == _undefined || v == _null {
		if async {
			return disposableResource{
				value: _undefined,
				async: true,
			}, true
		}
		return
	}
	obj, isObj := v.(*Object)
	if !isObj {
		panic(r.NewTypeError("Disposable resource must be an object, null or undefined"))
	}
	var method func(FunctionCall) Value
	if async {
		method = toMethod(obj.self.getSym(SymAsyncDispose, obj))
	}
	if method == nil {
		method = toMethod(obj.self.getSym(SymDispose, obj))
		if method == nil {
			if async {
				panic(r.NewTypeError("Object is not async disposable: neither [Symbol.asyncDispose] nor [Symbol.dispose] is a function"))
			}
			panic(r.NewTypeError("Object is not disposable: [Symbol.dispose] is not a function"))
		}
		if async {
			// The result of the synchronous method is ignored, undefined is awaited instead.
			syncMethod := method
			method = func(call FunctionCall) Value {
				syncMethod(call)
				return _undefined
			}
		}
	}
	return disposableResource{
		value:  v,
		method: method,
		async:  async,
	}, true
}

// addDisposableResource implements AddDisposableResource(disposeCapability, V, hint), where hint is async-dispose
// if async is true, sync-dispose otherwise.
func (r *Runtime) addDisposableResource(dc *disposeCapability, v Value, async bool) {
	if res, ok := r.createDisposableResource(v, async); ok {
		dc.resources = append(dc.resources, res)
	}
}

// addDisposeCallback adds a resource that has no value, only the method (as created by adopt() and defer()).
func (dc *disposeCapability) addDisposeCallback(method func(FunctionCall) Value, async bool) {
	dc.resources = append(dc.resources, disposableResource{
		value:  _undefined,
		method: method,
		async:  async,
	})
}

// addDisposeError records an exception thrown while disposing of the resources. If there is already an exception
// in *completion the new one is combined with it using SuppressedError.
func (r *Runtime) addDisposeError(completion **Exception, ex *Exception) {
	if *completion != nil {
		ex = &Exception{
			val: r.newSuppressedError(ex.val, (*completion).val),
		}
	}
	*completion = ex
}

// disposeResources implements DisposeResources(disposeCapability, completion) for synchronous resources.
// If the completion is a throw completion, completion must be set to the corresponding exception, otherwise it must be nil.
// Returns the resulting throw completion or nil if it's a normal completion.
func (r *Runtime) disposeResources(dc *disposeCapability, completion *Exception) *Exception {
	resources := dc.resources
	dc.resources = nil
	for i := len(resources) - 1; i >= 0; i-- {
		res := &resources[i]
		ex := r.vm.try(func() {
			res.method(FunctionCall{This: res.value})
		})
		if ex != nil {
			r.addDisposeError(&completion, ex)
		}
	}
	return completion
}

// disposeStep implements DisposeResources(disposeCapability, completion) for resources that may have to be awaited.
// It disposes of the resources, in the reverse order, until it finds one that requires an Await(). In this case it
// returns the value to be awaited and true. The caller must await it (ignoring the result unless it is a rejection,
// in which case it must be recorded using addDisposeError()) and call disposeStep() again. When all the resources
// have been disposed of it returns false. Exceptions are accumulated in *completion.
func (r *Runtime) disposeStep(dc *disposeCapability, completion **Exception) (Value, bool) {
	for {
		l := len(dc.resources)
		if l == 0 {
			if dc.needsAwait && !dc.hasAwaited {
				dc.needsAwait = false
				return _undefined, true
			}
			return nil, false
		}
		res := dc.resources[l-1]
		if !res.async && dc.needsAwait && !dc.hasAwaited {
			dc.needsAwait = false
			return _undefined, true
		}
		dc.resources[l-1] = disposableResource{}
		dc.resources = dc.resources[:l-1]
		if res.method == nil {
			// null or undefined with the async-dispose hint
			dc.needsAwait = true
			continue
		}
		var result Value
		ex := r.vm.try(func() {
			result = res.method(FunctionCall{This: res.value})
		})
		if ex != nil {
			r.addDisposeError(completion, ex)
			continue
		}
		if res.async {
			dc.hasAwaited = true
			return nilSafe(result), true
		}
	}
}

// disposeResourcesAsync runs DisposeResources() for a stack that has the async-dispose hint (i.e.
// AsyncDisposableStack.prototype.disposeAsync()). It returns a promise that is settled once all the resources
// have been disposed of. Like an async function, it runs synchronously until the first Await().
func (r *Runtime) disposeResourcesAsync(dc *disposeCapability) *Object {
	pcap := r.newPromiseCapability(r.getPromise())
	var completion *Exception
	var step func()
	onFulfilled := r.newNativeFunc(func(FunctionCall) Value {
		step()
		return _undefined
	}, "", 1)
	onRejected := r.newNativeFunc(func(call FunctionCall) Value {
		r.addDisposeError(&completion, &Exception{val: call.Argument(0)})
		step()
		return _undefined
	}, "", 1)
	step = func() {
		for {
			v, ok := r.disposeStep(dc, &completion)
			if !ok {
				break
			}
			var p *Object
			if ex := r.vm.try(func() {
				p = r.promiseResolve(r.getPromise(), v)
			}); ex != nil {
				r.addDisposeError(&completion, ex)
				continue
			}
			r.performPromiseThen(p.self.(*Promise), onFulfilled, onRejected, nil)
			return
		}
		if completion != nil {
			pcap.reject(completion.val)
		} else {
			pcap.resolve(_undefined)
		}
	}
	step()
	return pcap.promise
}

// disposableStackObject is both DisposableStack and AsyncDisposableStack (async is true for the latter).
type disposableStackObject struct {
	baseObject
	async      bool
	disposed   bool
	capability disposeCapability
}

func (r *Runtime) newDisposableStackObject(proto *Object, async bool) *disposableStackObject {
	o := &Object{runtime: r}

	ds := &disposableStackObject{}
	ds.class = classObject
	ds.val = o
	ds.extensible = true
	ds.async = async
	o.self = ds
	ds.prototype = proto
	ds.init()
	return ds
}

func disposableStackName(async bool) string {
	if async {
		return "AsyncDisposableStack"
	}
	return "DisposableStack"
}

func (r *Runtime) thisDisposableStackOrNil(v Value, async bool) *disposableStackObject {
	if obj, ok := v.(*Object); ok {
		if ds, ok := obj.self.(*disposableStackObject); ok && ds.async == async {
			return ds
		}
	}
	return nil
}

func (r *Runtime) thisDisposableStack(call FunctionCall, method string, async bool) *disposableStackObject {
	if ds := r.thisDisposableStackOrNil(call.This, async); ds != nil {
		return ds
	}
	panic(r.NewTypeError("Method %s.prototype.%s called on incompatible receiver", disposableStackName(async), method))
}

func (r *Runtime) throwIfDisposed(ds *disposableStackObject) {
	if ds.disposed {
		panic(r.newError(r.getReferenceError(), disposableStackName(ds.async)+" has already been disposed"))
	}
}

func (r *Runtime) disposableStackAdopt(call FunctionCall, async bool) Value {
	ds := r.thisDisposableStack(call, "adopt", async)
	r.throwIfDisposed(ds)
	onDispose, ok := assertCallable(call.Argument(1))
	if !ok {
		panic(r.NewTypeError("onDispose is not a function"))
	}
	value := call.Argument(0)
	ds.capability.addDisposeCallback(func(FunctionCall) Value {
		return onDispose(FunctionCall{This: _undefined, Arguments: []Value{value}})
	}, async)
	return value
}

func (r *Runtime) disposableStackDefer(call FunctionCall, async bool) Value {
	ds := r.thisDisposableStack(call, "defer", async)
	r.throwIfDisposed(ds)
	onDispose, ok := assertCallable(call.Argument(0))
	if !ok {
		panic(r.NewTypeError("onDispose is not a function"))
	}
	ds.capability.addDisposeCallback(func(FunctionCall) Value {
		return onDispose(FunctionCall{This: _undefined})
	}, async)
	return _undefined
}

func (r *Runtime) disposableStackDisposed(call FunctionCall, async bool) Value {
	ds := r.thisDisposableStack(call, "disposed", async)
	return r.toBoolean(ds.disposed)
}

func (r *Runtime) disposableStackMove(call FunctionCall, async bool) Value {
	ds := r.thisDisposableStack(call, "move", async)
	r.throwIfDisposed(ds)
	var proto *Object
	if async {
		proto = r.getAsyncDisposableStackPrototype()
	} else {
		proto = r.getDisposableStackPrototype()
	}
	newDs := r.newDisposableStackObject(proto, async)
	newDs.capability = ds.capability
	ds.capability = disposeCapability{}
	ds.disposed = true
	return newDs.val
}

func (r *Runtime) disposableStackUse(call FunctionCall, async bool) Value {
	ds := r.thisDisposableStack(call, "use", async)
	r.throwIfDisposed(ds)
	value := call.Argument(0)
	r.addDisposableResource(&ds.capability, value, async)
	return value
}

func (r *Runtime) disposableStackProto_adopt(call FunctionCall) Value {
	return r.disposableStackAdopt(call, false)
}

func (r *Runtime) disposableStackProto_defer(call FunctionCall) Value {
	return r.disposableStackDefer(call, false)
}

func (r *Runtime) disposableStackProto_dispose(call FunctionCall) Value {
	ds := r.thisDisposableStack(call, "dispose", false)
	if ds.disposed {
		return _undefined
	}
	ds.disposed = true
	if ex := r.disposeResources(&ds.capability, nil); ex != nil {
		panic(ex)
	}
	return _undefined
}

func (r *Runtime) disposableStackProto_disposed(call FunctionCall) Value {
	return r.disposableStackDisposed(call, false)
}

func (r *Runtime) disposableStackProto_move(call FunctionCall) Value {
	return r.disposableStackMove(call, false)
}

func (r *Runtime) disposableStackProto_use(call FunctionCall) Value {
	return r.disposableStackUse(call, false)
}

func (r *Runtime) asyncDisposableStackProto_adopt(call FunctionCall) Value {
	return r.disposableStackAdopt(call, true)
}

func (r *Runtime) asyncDisposableStackProto_defer(call FunctionCall) Value {
	return r.disposableStackDefer(call, true)
}

func (r *Runtime) asyncDisposableStackProto_disposeAsync(call FunctionCall) Value {
	ds := r.thisDisposableStackOrNil(call.This, true)
	if ds == nil {
		// errors are reported by rejecting the returned promise
		pcap := r.newPromiseCapability(r.getPromise())
		pcap.reject(r.NewTypeError("Method AsyncDisposableStack.prototype.disposeAsync called on incompatible receiver"))
		return pcap.promise
	}
	if ds.disposed {
		pcap := r.newPromiseCapability(r.getPromise())
		pcap.resolve(_undefined)
		return pcap.promise
	}
	ds.disposed = true
	return r.disposeResourcesAsync(&ds.capability)
}

func (r *Runtime) asyncDisposableStackProto_disposed(call FunctionCall) Value {
	return r.disposableStackDisposed(call, true)
}

func (r *Runtime) asyncDisposableStackProto_move(call FunctionCall) Value {
	return r.disposableStackMove(call, true)
}

func (r *Runtime) asyncDisposableStackProto_use(call FunctionCall) Value {
	return r.disposableStackUse(call, true)
}

func (r *Runtime) iteratorProto_dispose(call FunctionCall) Value {
	if ret := toMethod(r.getVStr(call.This, "return")); ret != nil {
		ret(FunctionCall{This: call.This})
	}
	return _undefined
}

func (r *Runtime) builtin_newDisposableStack(args []Value, newTarget *Object) *Object {
	if newTarget == nil {
		panic(r.needNew("DisposableStack"))
	}
	proto := r.getPrototypeFromCtor(newTarget, r.global.DisposableStack, r.global.DisposableStackPrototype)
	return r.newDisposableStackObject(proto, false).val
}

func (r *Runtime) builtin_newAsyncDisposableStack(args []Value, newTarget *Object) *Object {
	if newTarget == nil {
		panic(r.needNew("AsyncDisposableStack"))
	}
	proto := r.getPrototypeFromCtor(newTarget, r.global.AsyncDisposableStack, r.global.AsyncDisposableStackPrototype)
	return r.newDisposableStackObject(proto, true).val
}

func (r *Runtime) createDisposableStackProto(val *Object) objectImpl {
	o := newBaseObjectObj(val, r.global.ObjectPrototype, classObject)

	o._putProp("constructor", r.getDisposableStack(), true, false, true)
	o._putProp("adopt", r.newNativeFunc(r.disposableStackProto_adopt, "adopt", 2), true, false, true)
	o._putProp("defer", r.newNativeFunc(r.disposableStackProto_defer, "defer", 1), true, false, true)
	dispose := r.newNativeFunc(r.disposableStackProto_dispose, "dispose", 0)
	o._putProp("dispose", dispose, true, false, true)
	o.setOwnStr("disposed", &valueProperty{
		configurable: true,
		getterFunc:   r.newNativeFunc(r.disposableStackProto_disposed, "get disposed", 0),
		accessor:     true,
	}, false)
	o._putProp("move", r.newNativeFunc(r.disposableStackProto_move, "move", 0), true, false, true)
	o._putProp("use", r.newNativeFunc(r.disposableStackProto_use, "use", 1), true, false, true)

	o._putSym(SymDispose, valueProp(dispose, true, false, true))
	o._putSym(SymToStringTag, valueProp(asciiString("DisposableStack"), false, false, true))

	return o
}

func (r *Runtime) createAsyncDisposableStackProto(val *Object) objectImpl {
	o := newBaseObjectObj(val, r.global.ObjectPrototype, classObject)

	o._putProp("constructor", r.getAsyncDisposableStack(), true, false, true)
	o._putProp("adopt", r.newNativeFunc(r.asyncDisposableStackProto_adopt, "adopt", 2), true, false, true)
	o._putProp("defer", r.newNativeFunc(r.asyncDisposableStackProto_defer, "defer", 1), true, false, true)
	disposeAsync := r.newNativeFunc(r.asyncDisposableStackProto_disposeAsync, "disposeAsync", 0)
	o._putProp("disposeAsync", disposeAsync, true, false, true)
	o.setOwnStr("disposed", &valueProperty{
		configurable: true,
		getterFunc:   r.newNativeFunc(r.asyncDisposableStackProto_disposed, "get disposed", 0),
		accessor:     true,
	}, false)
	o._putProp("move", r.newNativeFunc(r.asyncDisposableStackProto_move, "move", 0), true, false, true)
	o._putProp("use", r.newNativeFunc(r.asyncDisposableStackProto_use, "use", 1), true, false, true)

	o._putSym(SymAsyncDispose, valueProp(disposeAsync, true, false, true))
	o._putSym(SymToStringTag, valueProp(asciiString("AsyncDisposableStack"), false, false, true))

	return o
}

func (r *Runtime) createDisposableStack(val *Object) objectImpl {
	o := r.newNativeConstructOnly(val, r.builtin_newDisposableStack, r.getDisposableStackPrototype(), "DisposableStack", 0)

	return o
}

func (r *Runtime) createAsyncDisposableStack(val *Object) objectImpl {
	o := r.newNativeConstructOnly(val, r.builtin_newAsyncDisposableStack, r.getAsyncDisposableStackPrototype(), "AsyncDisposableStack", 0)

	return o
}

func (r *Runtime) getDisposableStackPrototype() *Object {
	ret := r.global.DisposableStackPrototype
	if ret == nil {
		ret = &Object{runtime: r}
		r.global.DisposableStackPrototype = ret
		ret.self = r.createDisposableStackProto(ret)
	}
	return ret
}

func (r *Runtime) getAsyncDisposableStackPrototype() *Object {
	ret := r.global.AsyncDisposableStackPrototype
	if ret == nil {
		ret = &Object{runtime: r}
		r.global.AsyncDisposableStackPrototype = ret
		ret.self = r.createAsyncDisposableStackProto(ret)
	}
	return ret
}

func (r *Runtime) getDisposableStack() *Object {
	ret := r.global.DisposableStack
	if ret == nil {
		ret = &Object{runtime: r}
		r.global.DisposableStack = ret
		ret.self = r.createDisposableStack(ret)
	}
	return ret
}

func (r *Runtime) getAsyncDisposableStack() *Object {
	ret := r.global.AsyncDisposableStack
	if ret == nil {
		ret = &Object{runtime: r}
		r.global.AsyncDisposableStack = ret
		ret.self = r.createAsyncDisposableStack(ret)
	}
	return ret
}
