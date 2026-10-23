package canon

import (
	"context"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// Async ABI constants (CanonicalABI.md).
const (
	maxFlatAsyncParams = 4

	callbackExit  = 0
	callbackYield = 1
	callbackWait  = 2

	eventNone    = 0
	eventSubtask = 1

	subtaskStarted                 = 1
	subtaskReturned                = 2
	subtaskCancelledBeforeReturned = 4
)

// HostFunc implements a component function import in Go. For an
// async-lowered import it runs on its own goroutine, and ctx is cancelled
// when the guest cancels the call.
type HostFunc func(ctx context.Context, args []Val) ([]Val, error)

// AsyncState is the async-ABI state of one component instance: its table
// of subtasks and waitable sets, and the task currently running in it.
//
// ponytail: subtasks and waitable sets live in their own table rather than
// sharing the resource table as the spec has it; guests only see opaque
// indices, so this is invisible unless a guest mixes the index spaces.
type AsyncState struct {
	handles []any // *subtask or *waitableSet; index 0 is never used
	free    []uint32
	task    *asyncTask
	done    chan *subtask
}

// NewAsyncState returns empty async state for a component instance.
func NewAsyncState() *AsyncState {
	// ponytail: fixed-size completion buffer; a host goroutine blocks on
	// send once 1024 completions are undelivered.
	return &AsyncState{handles: []any{nil}, done: make(chan *subtask, 1024)}
}

// asyncTask is one call of an async-lifted export.
// ponytail: one task per instance at a time (the instance's reentrance
// gate already serialises exports); concurrent tasks need a task per
// event-loop turn instead of this field.
type asyncTask struct {
	context  uint64
	results  []Val
	returned bool
}

type waitableSet struct {
	members []*subtask
}

// subtask is an async-lowered call into a HostFunc.
type subtask struct {
	idx       uint32
	state     uint32
	set       *waitableSet
	cancel    context.CancelFunc
	results   []Val
	err       error
	resolved  bool // completion applied on the event-loop goroutine
	pending   bool // resolved and not yet delivered to the guest
	delivered bool
	lower     func(ctx context.Context, results []Val) // writes results into guest memory
}

func (st *AsyncState) add(h any) uint32 {
	if n := len(st.free); n > 0 {
		i := st.free[n-1]
		st.free = st.free[:n-1]
		st.handles[i] = h
		return i
	}
	st.handles = append(st.handles, h)
	return uint32(len(st.handles) - 1)
}

func (st *AsyncState) remove(i uint32) {
	st.handles[i] = nil
	st.free = append(st.free, i)
}

func (st *AsyncState) get(i uint32) any {
	if i == 0 || int(i) >= len(st.handles) || st.handles[i] == nil {
		trapf("unknown handle index %d", i)
	}
	return st.handles[i]
}

func (st *AsyncState) getSubtask(i uint32) *subtask {
	s, ok := st.get(i).(*subtask)
	if !ok {
		trapf("handle index %d is not a subtask", i)
	}
	return s
}

func (st *AsyncState) getSet(i uint32) *waitableSet {
	s, ok := st.get(i).(*waitableSet)
	if !ok {
		trapf("handle index %d is not a waitable set", i)
	}
	return s
}

func (st *AsyncState) currentTask(op string) *asyncTask {
	if st.task == nil {
		trapf("%s: no current async task", op)
	}
	return st.task
}

// apply records a completion on the event-loop goroutine, lowering the
// subtask's results into guest memory.
func (st *AsyncState) apply(ctx context.Context, s *subtask) {
	s.resolved = true
	s.pending = true
	switch {
	case s.err == nil:
		s.lower(ctx, s.results)
		s.state = subtaskReturned
	case errors.Is(s.err, context.Canceled):
		s.state = subtaskCancelledBeforeReturned
	default:
		trapf("async host import failed: %v", s.err)
	}
}

// waitOne blocks until a host goroutine completes, then applies it.
func (st *AsyncState) waitOne(ctx context.Context) error {
	select {
	case s := <-st.done:
		st.apply(ctx, s)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitSet blocks until a member of set has an event, and delivers it.
func (st *AsyncState) waitSet(ctx context.Context, set *waitableSet) (code, p1, p2 uint32, err error) {
	for {
		for _, s := range set.members {
			if s.pending {
				s.pending = false
				s.delivered = true
				return eventSubtask, s.idx, s.state, nil
			}
		}
		if err := st.waitOne(ctx); err != nil {
			return 0, 0, 0, err
		}
	}
}

func (s *subtask) join(set *waitableSet) {
	if s.set != nil {
		for i, m := range s.set.members {
			if m == s {
				s.set.members = append(s.set.members[:i], s.set.members[i+1:]...)
				break
			}
		}
	}
	s.set = set
	if set != nil {
		set.members = append(set.members, s)
	}
}

// BuildContextGet implements context.get i32 0.
func (h *Host) BuildContextGet(ctx context.Context, baseName string, st *AsyncState) (*CallAdapter, error) {
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		stack[0] = st.currentTask("context.get").context
	})
	return h.buildResourceStub(ctx, baseName, "context.get", fn, nil, []api.ValueType{api.ValueTypeI32})
}

// BuildContextSet implements context.set i32 0.
func (h *Host) BuildContextSet(ctx context.Context, baseName string, st *AsyncState) (*CallAdapter, error) {
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		st.currentTask("context.set").context = uint64(uint32(stack[0]))
	})
	return h.buildResourceStub(ctx, baseName, "context.set", fn, []api.ValueType{api.ValueTypeI32}, nil)
}

// BuildWaitableSetNew implements waitable-set.new.
func (h *Host) BuildWaitableSetNew(ctx context.Context, baseName string, st *AsyncState) (*CallAdapter, error) {
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		stack[0] = uint64(st.add(&waitableSet{}))
	})
	return h.buildResourceStub(ctx, baseName, "waitable-set.new", fn, nil, []api.ValueType{api.ValueTypeI32})
}

// BuildWaitableSetDrop implements waitable-set.drop.
func (h *Host) BuildWaitableSetDrop(ctx context.Context, baseName string, st *AsyncState) (*CallAdapter, error) {
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		i := uint32(stack[0])
		if len(st.getSet(i).members) > 0 {
			trapf("waitable-set.drop: set %d is not empty", i)
		}
		st.remove(i)
	})
	return h.buildResourceStub(ctx, baseName, "waitable-set.drop", fn, []api.ValueType{api.ValueTypeI32}, nil)
}

// BuildWaitableJoin implements waitable.join.
func (h *Host) BuildWaitableJoin(ctx context.Context, baseName string, st *AsyncState) (*CallAdapter, error) {
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		s := st.getSubtask(uint32(stack[0]))
		var set *waitableSet
		if si := uint32(stack[1]); si != 0 {
			set = st.getSet(si)
		}
		s.join(set)
	})
	return h.buildResourceStub(ctx, baseName, "waitable.join", fn,
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, nil)
}

// BuildSubtaskDrop implements subtask.drop.
func (h *Host) BuildSubtaskDrop(ctx context.Context, baseName string, st *AsyncState) (*CallAdapter, error) {
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		i := uint32(stack[0])
		s := st.getSubtask(i)
		if !s.delivered {
			trapf("subtask.drop: subtask %d has not resolved", i)
		}
		s.join(nil)
		st.remove(i)
	})
	return h.buildResourceStub(ctx, baseName, "subtask.drop", fn, []api.ValueType{api.ValueTypeI32}, nil)
}

// BuildSubtaskCancel implements the synchronous subtask.cancel: it cancels
// the host goroutine and blocks until it finishes.
func (h *Host) BuildSubtaskCancel(ctx context.Context, baseName string, st *AsyncState) (*CallAdapter, error) {
	fn := api.GoModuleFunc(func(callCtx context.Context, _ api.Module, stack []uint64) {
		s := st.getSubtask(uint32(stack[0]))
		if s.delivered {
			trapf("subtask.cancel: subtask already resolved")
		}
		if s.set != nil {
			trapf("subtask.cancel: subtask is in a waitable set")
		}
		s.cancel()
		for !s.resolved {
			if err := st.waitOne(callCtx); err != nil {
				trapf("subtask.cancel: %v", err)
			}
		}
		s.pending = false
		s.delivered = true
		stack[0] = uint64(s.state)
	})
	return h.buildResourceStub(ctx, baseName, "subtask.cancel", fn,
		[]api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32})
}

// BuildTaskReturn implements task.return for results lifted through side.
func (h *Host) BuildTaskReturn(ctx context.Context, baseName string, st *AsyncState, side CallSide, results []Type) (*CallAdapter, error) {
	steps, flat, paramTypes := liftSteps(results, maxFlatParams)
	gcc := &gocallContext{callee: sideOf(side)}
	fn := api.GoModuleFunc(func(callCtx context.Context, _ api.Module, stack []uint64) {
		task := st.currentTask("task.return")
		if task.returned {
			trapf("task.return called twice")
		}
		gcc.registers = stack
		var base uint32
		if !flat {
			base = uint32(stack[0])
		}
		vals := make([]Val, len(steps))
		for i, step := range steps {
			v, err := step(callCtx, gcc, base)
			if err != nil {
				trapf("task.return: %v", err)
			}
			vals[i] = v
		}
		task.results = vals
		task.returned = true
	})
	return h.buildResourceStub(ctx, baseName, "task.return", fn, paramTypes, nil)
}

// BuildHostImport builds the core function a guest calls for a component
// function implemented by fn. With async, the call runs fn on a new
// goroutine and returns a subtask; otherwise it runs fn inline.
func (h *Host) BuildHostImport(
	ctx context.Context,
	baseName string,
	st *AsyncState,
	caller CallSide,
	async bool,
	params, results []Type,
	fn HostFunc,
) (*CallAdapter, error) {
	maxParams := uint32(maxFlatParams)
	if async {
		maxParams = maxFlatAsyncParams
	}
	pSteps, pFlat, paramTypes := liftSteps(params, maxParams)
	nParamSlots := len(paramTypes)

	// Results: sync and at most one flat value comes back in registers;
	// everything else is written through a trailing return pointer.
	var rFlatSteps, rMemSteps []gocallLowerStep
	var resultTypes []api.ValueType
	rFlat := !async && pickFlat(results, maxFlatResults)
	if rFlat {
		fv := &valToFlatVisitor{}
		for _, t := range results {
			t.Accept(fv)
			for _, ct := range flattenTypeOf(t) {
				resultTypes = append(resultTypes, coreValueTypeToAPI(ct))
			}
		}
		rFlatSteps = fv.out
	} else if len(results) > 0 {
		mv := &valToMemVisitor{}
		for _, t := range results {
			t.Accept(mv)
		}
		rMemSteps = mv.out
		paramTypes = append(paramTypes, api.ValueTypeI32)
	}
	if async {
		resultTypes = []api.ValueType{api.ValueTypeI32}
	}

	side := sideOf(caller)
	lift := func(callCtx context.Context, stack []uint64) []Val {
		gcc := &gocallContext{callee: side, registers: stack}
		var base uint32
		if !pFlat {
			base = uint32(stack[0])
		}
		args := make([]Val, len(pSteps))
		for i, step := range pSteps {
			v, err := step(callCtx, gcc, base)
			if err != nil {
				trapf("%s: lift params: %v", baseName, err)
			}
			args[i] = v
		}
		return args
	}
	lowerMem := func(callCtx context.Context, retptr uint32, vals []Val) {
		prev := side.Instance.SuspendLeave()
		defer side.Instance.RestoreLeave(prev)
		gcc := &gocallContext{callee: side}
		for i, step := range rMemSteps {
			if err := step(callCtx, gcc, vals[i], retptr); err != nil {
				trapf("%s: lower results: %v", baseName, err)
			}
		}
	}

	var core api.GoModuleFunc
	if async {
		core = func(callCtx context.Context, _ api.Module, stack []uint64) {
			args := lift(callCtx, stack)
			var retptr uint32
			if len(rMemSteps) > 0 {
				retptr = uint32(stack[nParamSlots])
			}
			goCtx, cancel := context.WithCancel(callCtx)
			s := &subtask{state: subtaskStarted, cancel: cancel}
			s.lower = func(c context.Context, vals []Val) { lowerMem(c, retptr, vals) }
			s.idx = st.add(s)
			go func() {
				s.results, s.err = fn(goCtx, args)
				st.done <- s
			}()
			stack[0] = uint64(subtaskStarted | s.idx<<4)
		}
	} else {
		core = func(callCtx context.Context, _ api.Module, stack []uint64) {
			args := lift(callCtx, stack)
			var retptr uint32
			if len(rMemSteps) > 0 {
				retptr = uint32(stack[nParamSlots])
			}
			vals, err := fn(callCtx, args)
			if err != nil {
				trapf("%s: %v", baseName, err)
			}
			if rFlat {
				gcc := &gocallContext{callee: side, registers: stack}
				for i, step := range rFlatSteps {
					if err := step(callCtx, gcc, vals[i], 0); err != nil {
						trapf("%s: lower results: %v", baseName, err)
					}
				}
			} else {
				lowerMem(callCtx, retptr, vals)
			}
		}
	}
	return h.buildResourceStub(ctx, baseName, "host import", core, paramTypes, resultTypes)
}

// CallAsync runs an async-lifted export: it lowers args, calls the core
// function, then drives the callback event loop until the task exits,
// returning the values the guest passed to task.return.
func (cb *CallBinding) CallAsync(ctx context.Context, args []Val, st *AsyncState, callback api.Function) (results []Val, err error) {
	defer func() {
		if r := recover(); r != nil {
			if t, ok := r.(*Trap); ok {
				err = fmt.Errorf("%s", t.Error())
				return
			}
			panic(r)
		}
	}()
	if callback == nil {
		return nil, fmt.Errorf("async lift without callback is not supported")
	}
	if len(args) != len(cb.plan.paramSteps) {
		return nil, fmt.Errorf("expected %d args, got %d", len(cb.plan.paramSteps), len(args))
	}
	gcc := &cb.gcc
	if err := gcc.callee.Instance.Enter(ctx); err != nil {
		return nil, err
	}
	defer gcc.callee.Instance.Exit(ctx)

	task := &asyncTask{}
	st.task = task
	defer func() { st.task = nil }()

	stack := make([]uint64, max(cb.plan.nParamRegs, 3))
	gcc.registers = stack
	var paramBase uint32
	if err := func() error {
		prev := gcc.callee.Instance.SuspendLeave()
		defer gcc.callee.Instance.RestoreLeave(prev)
		if cb.plan.paramMemSize > 0 {
			ptr, rerr := callRealloc(ctx, gcc.callee.Realloc, gcc.callee.Memory, 0, 0, cb.plan.paramMaxAlign, cb.plan.paramMemSize)
			if rerr != nil {
				return rerr
			}
			paramBase = ptr
			stack[0] = uint64(ptr)
		}
		for i, step := range cb.plan.paramSteps {
			if serr := step(ctx, gcc, args[i], paramBase); serr != nil {
				return serr
			}
		}
		return nil
	}(); err != nil {
		return nil, err
	}

	if err := cb.core.CallWithStack(ctx, stack[:max(cb.plan.nParamRegs, 1)]); err != nil {
		gcc.callee.Instance.Poison(err)
		return nil, err
	}
	for {
		packed := uint32(stack[0])
		var code, p1, p2 uint32
		switch packed & 0xf {
		case callbackExit:
			if !task.returned {
				return nil, fmt.Errorf("async export exited without calling task.return")
			}
			return task.results, nil
		case callbackYield:
			code = eventNone
		case callbackWait:
			code, p1, p2, err = st.waitSet(ctx, st.getSet(packed>>4))
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("invalid callback code %d", packed&0xf)
		}
		stack[0], stack[1], stack[2] = uint64(code), uint64(p1), uint64(p2)
		if err := callback.CallWithStack(ctx, stack[:3]); err != nil {
			gcc.callee.Instance.Poison(err)
			return nil, err
		}
	}
}

// liftSteps compiles steps lifting types from registers when they flatten
// to at most maxFlat values, else from memory at the pointer in register
// 0. It also returns the core param types the values arrive in.
func liftSteps(types []Type, maxFlat uint32) (steps []gocallLiftStep, flat bool, coreTypes []api.ValueType) {
	if pickFlat(types, maxFlat) {
		fv := &flatToValVisitor{}
		for _, t := range types {
			t.Accept(fv)
			for _, ct := range flattenTypeOf(t) {
				coreTypes = append(coreTypes, coreValueTypeToAPI(ct))
			}
		}
		return fv.out, true, coreTypes
	}
	mv := &memToValVisitor{}
	for _, t := range types {
		t.Accept(mv)
	}
	return mv.out, false, []api.ValueType{api.ValueTypeI32}
}

func sideOf(c CallSide) *transferSide {
	return &transferSide{
		Instance:       c.Instance,
		Memory:         c.Memory,
		Realloc:        c.realloc(),
		StringEncoding: c.StringEncoding,
		ResourceTable:  c.Instance.ResourceTable(),
	}
}
