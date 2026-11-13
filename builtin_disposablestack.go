package goja

// disposableResource is a DisposableResource Record. For the resources added by DisposableStack.prototype.adopt()
// and DisposableStack.prototype.defer() the value is undefined and the method is a closure calling onDispose.
type disposableResource struct {
	value  Value
	method func(FunctionCall) Value
}

// disposeCapability is a DisposeCapability Record.
type disposeCapability struct {
	resources []disposableResource
}

// createDisposableResource implements CreateDisposableResource(V, sync-dispose).
// Returns false if v is null or undefined in which case no resource should be added.
func (r *Runtime) createDisposableResource(v Value) (res disposableResource, ok bool) {
	if v == nil || v == _undefined || v == _null {
		return
	}
	obj, isObj := v.(*Object)
	if !isObj {
		panic(r.NewTypeError("Disposable resource must be an object, null or undefined"))
	}
	method := toMethod(obj.self.getSym(SymDispose, obj))
	if method == nil {
		panic(r.NewTypeError("Object is not disposable: [Symbol.dispose] is not a function"))
	}
	return disposableResource{
		value:  v,
		method: method,
	}, true
}

// addDisposableResource implements AddDisposableResource(disposeCapability, V, sync-dispose).
func (r *Runtime) addDisposableResource(dc *disposeCapability, v Value) {
	if res, ok := r.createDisposableResource(v); ok {
		dc.resources = append(dc.resources, res)
	}
}

// addDisposeCallback adds a resource that has no value, only the method (as created by adopt() and defer()).
func (dc *disposeCapability) addDisposeCallback(method func(FunctionCall) Value) {
	dc.resources = append(dc.resources, disposableResource{
		value:  _undefined,
		method: method,
	})
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
			if completion != nil {
				completion = &Exception{
					val: r.newSuppressedError(ex.val, completion.val),
				}
			} else {
				completion = ex
			}
		}
	}
	return completion
}

type disposableStackObject struct {
	baseObject
	disposed   bool
	capability disposeCapability
}

func (r *Runtime) newDisposableStackObject(proto *Object) *disposableStackObject {
	o := &Object{runtime: r}

	ds := &disposableStackObject{}
	ds.class = classObject
	ds.val = o
	ds.extensible = true
	o.self = ds
	ds.prototype = proto
	ds.init()
	return ds
}

func (r *Runtime) thisDisposableStack(call FunctionCall, method string) *disposableStackObject {
	if obj, ok := call.This.(*Object); ok {
		if ds, ok := obj.self.(*disposableStackObject); ok {
			return ds
		}
	}
	panic(r.NewTypeError("Method DisposableStack.prototype.%s called on incompatible receiver", method))
}

func (r *Runtime) throwIfDisposed(ds *disposableStackObject) {
	if ds.disposed {
		panic(r.newError(r.getReferenceError(), "DisposableStack has already been disposed"))
	}
}

func (r *Runtime) disposableStackProto_adopt(call FunctionCall) Value {
	ds := r.thisDisposableStack(call, "adopt")
	r.throwIfDisposed(ds)
	onDispose, ok := assertCallable(call.Argument(1))
	if !ok {
		panic(r.NewTypeError("onDispose is not a function"))
	}
	value := call.Argument(0)
	ds.capability.addDisposeCallback(func(FunctionCall) Value {
		return onDispose(FunctionCall{This: _undefined, Arguments: []Value{value}})
	})
	return value
}

func (r *Runtime) disposableStackProto_defer(call FunctionCall) Value {
	ds := r.thisDisposableStack(call, "defer")
	r.throwIfDisposed(ds)
	onDispose, ok := assertCallable(call.Argument(0))
	if !ok {
		panic(r.NewTypeError("onDispose is not a function"))
	}
	ds.capability.addDisposeCallback(func(FunctionCall) Value {
		return onDispose(FunctionCall{This: _undefined})
	})
	return _undefined
}

func (r *Runtime) disposableStackProto_dispose(call FunctionCall) Value {
	ds := r.thisDisposableStack(call, "dispose")
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
	ds := r.thisDisposableStack(call, "disposed")
	return r.toBoolean(ds.disposed)
}

func (r *Runtime) disposableStackProto_move(call FunctionCall) Value {
	ds := r.thisDisposableStack(call, "move")
	r.throwIfDisposed(ds)
	newDs := r.newDisposableStackObject(r.getDisposableStackPrototype())
	newDs.capability = ds.capability
	ds.capability = disposeCapability{}
	ds.disposed = true
	return newDs.val
}

func (r *Runtime) disposableStackProto_use(call FunctionCall) Value {
	ds := r.thisDisposableStack(call, "use")
	r.throwIfDisposed(ds)
	value := call.Argument(0)
	r.addDisposableResource(&ds.capability, value)
	return value
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
	return r.newDisposableStackObject(proto).val
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

func (r *Runtime) createDisposableStack(val *Object) objectImpl {
	o := r.newNativeConstructOnly(val, r.builtin_newDisposableStack, r.getDisposableStackPrototype(), "DisposableStack", 0)

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

func (r *Runtime) getDisposableStack() *Object {
	ret := r.global.DisposableStack
	if ret == nil {
		ret = &Object{runtime: r}
		r.global.DisposableStack = ret
		ret.self = r.createDisposableStack(ret)
	}
	return ret
}
