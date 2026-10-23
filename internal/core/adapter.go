package core

import (
	"context"
	"fmt"

	"github.com/partite-ai/wacogo/internal/canon"
)

// execLowerWithGoAdapter creates a cross-component adapter via canon.Host.BuildAdapter.
// The adapter bridges a call from the caller's core ABI into the callee's core ABI
// using a pre-compiled transfer plan. Caller options come from the lower step;
// the callee side comes from fn.Callee().
func (s *instantiationState) execLowerWithGoAdapter(ctx context.Context, step planLower, fn *ExportedFunc) error {
	caller, err := s.callerSide(step.options, "lower")
	if err != nil {
		return err
	}

	if fn.goFn != nil {
		baseName := fmt.Sprintf("wacogo_golower_%s_%d", sanitizeName(fn.Name), instanceCounter.Add(1))
		adapter, err := s.inst.engine.canonHost.BuildHostImport(ctx, baseName, s.inst.async, caller,
			step.options.async,
			FuncTypeParamsAsCanon(fn.funcType),
			FuncTypeResultsAsCanon(fn.funcType),
			fn.goFn)
		if err != nil {
			return fmt.Errorf("wacogo: lower %q: %w", fn.Name, err)
		}
		s.addAdapter(adapter)
		return nil
	}
	if step.options.async {
		return fmt.Errorf("wacogo: lower %q: async lowering is only supported for Go functions", fn.Name)
	}

	callee := fn.binding.Callee()

	var baseName string
	if fn.Name != "" {
		baseName = fmt.Sprintf("wacogo_lower_%s", sanitizeName(fn.Name))
	} else {
		baseName = fmt.Sprintf("wacogo_lower_%d", instanceCounter.Add(1))
	}
	adapter, err := s.inst.engine.canonHost.BuildAdapter(ctx, baseName,
		caller, callee,
		FuncTypeParamsAsCanon(fn.funcType),
		FuncTypeResultsAsCanon(fn.funcType))
	if err != nil {
		return fmt.Errorf("wacogo: lower adapter: %w", err)
	}

	s.inst.owned = append(s.inst.owned, adapter)
	s.coreFuncs = append(s.coreFuncs, coreFunc{
		instance: adapter.Module,
		name:     adapter.Name,
	})
	return nil
}

// callerSide resolves a lift/lower/task.return's memory, realloc and
// string encoding options against this instantiation.
func (s *instantiationState) callerSide(opts canonicalOptions, op string) (canon.CallSide, error) {
	side := canon.CallSide{
		Instance:       canonInstanceView{i: s.inst},
		StringEncoding: opts.stringEncoding,
	}
	if opts.hasMemory {
		if int(opts.memory) >= len(s.coreMemories) {
			return side, fmt.Errorf("wacogo: %s: memory option index %d out of range", op, opts.memory)
		}
		cm := s.coreMemories[opts.memory]
		side.Memory = cm.instance.ExportedMemory(cm.name)
		if side.Memory == nil {
			return side, fmt.Errorf("wacogo: %s: memory %q not exported by module", op, cm.name)
		}
	}
	if opts.hasRealloc {
		if int(opts.realloc) >= len(s.coreFuncs) {
			return side, fmt.Errorf("wacogo: %s: realloc option index %d out of range", op, opts.realloc)
		}
		rf := s.coreFuncs[opts.realloc]
		side.Realloc = rf.instance.ExportedFunction(rf.name)
		if side.Realloc == nil {
			return side, fmt.Errorf("wacogo: %s: realloc %q not exported by module", op, rf.name)
		}
		side.ReallocModName = rf.instance.Name()
		side.ReallocFnExport = rf.name
	}
	return side, nil
}

// addAdapter records adapter as owned and appends its function to the
// core function index space.
func (s *instantiationState) addAdapter(adapter *canon.CallAdapter) {
	s.inst.owned = append(s.inst.owned, adapter)
	s.coreFuncs = append(s.coreFuncs, coreFunc{instance: adapter.Module, name: adapter.Name})
}

func (step *planTaskReturn) execute(ctx context.Context, s *instantiationState) error {
	side, err := s.callerSide(step.options, "task.return")
	if err != nil {
		return err
	}
	rc := &resolverCtx{inst: s.inst, imports: s.imports, state: s}
	results := make([]canon.Type, len(step.results))
	for i, r := range step.results {
		t := r.resolve(rc)
		if t == nil {
			return fmt.Errorf("wacogo: task.return: unresolved result type")
		}
		results[i] = canonType{t: t}
	}
	adapter, err := s.inst.engine.canonHost.BuildTaskReturn(ctx,
		fmt.Sprintf("wacogo_taskreturn_%d", instanceCounter.Add(1)), s.inst.async, side, results)
	if err != nil {
		return fmt.Errorf("wacogo: task.return: %w", err)
	}
	s.addAdapter(adapter)
	return nil
}

func (step *planAsyncBuiltin) execute(ctx context.Context, s *instantiationState) error {
	h := s.inst.engine.canonHost
	build := map[asyncBuiltin]func(context.Context, string, *canon.AsyncState) (*canon.CallAdapter, error){
		builtinContextGet:      h.BuildContextGet,
		builtinContextSet:      h.BuildContextSet,
		builtinSubtaskCancel:   h.BuildSubtaskCancel,
		builtinSubtaskDrop:     h.BuildSubtaskDrop,
		builtinWaitableSetNew:  h.BuildWaitableSetNew,
		builtinWaitableSetDrop: h.BuildWaitableSetDrop,
		builtinWaitableJoin:    h.BuildWaitableJoin,
	}[step.kind]
	adapter, err := build(ctx, fmt.Sprintf("wacogo_async_%d", instanceCounter.Add(1)), s.inst.async)
	if err != nil {
		return fmt.Errorf("wacogo: async built-in: %w", err)
	}
	s.addAdapter(adapter)
	return nil
}

// sanitizeName replaces characters unsuitable for wasm module names with '_'.
func sanitizeName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			out = append(out, c)
			continue
		}
		out = append(out, '_')
	}
	return string(out)
}

// execResourceNew creates a core function for canon resource.new.
// Signature: (rep: i32) -> (handle: i32)
func (step *planResourceNew) execute(ctx context.Context, s *instantiationState) error {
	rtype := lookupResourceType(s, step.typeID)
	baseName := fmt.Sprintf("wacogo_resnew_%d", instanceCounter.Add(1))
	adapter, err := s.inst.engine.canonHost.BuildResourceNew(ctx, baseName,
		canonInstanceView{i: s.inst}, resourceType{rt: rtype})
	if err != nil {
		return fmt.Errorf("wacogo: resource.new: %w", err)
	}
	s.inst.owned = append(s.inst.owned, adapter)
	s.coreFuncs = append(s.coreFuncs, coreFunc{
		instance: adapter.Module,
		name:     adapter.Name,
	})
	return nil
}

// execResourceDrop creates a core function for canon resource.drop.
// Signature: (handle: i32) -> ()
//
// The destructor is no longer threaded through the BuildResourceDrop
// signature — canon's ResourceTable.Drop reaches it via
// ResourceType.Destructor() which forwards to *TypeResource.dtor.
// step.dtorFuncIndex still influences load-time core-func wiring (the
// dtor api.Function is registered as a regular core function for
// alias resolution etc.), but canon no longer consumes it directly
// from this code path.
func (step *planResourceDrop) execute(ctx context.Context, s *instantiationState) error {
	rtype := lookupResourceType(s, step.typeID)

	baseName := fmt.Sprintf("wacogo_resdrop_%d", instanceCounter.Add(1))
	adapter, err := s.inst.engine.canonHost.BuildResourceDrop(ctx, baseName,
		canonInstanceView{i: s.inst}, resourceType{rt: rtype})
	if err != nil {
		return fmt.Errorf("wacogo: resource.drop: %w", err)
	}
	s.inst.owned = append(s.inst.owned, adapter)
	s.coreFuncs = append(s.coreFuncs, coreFunc{
		instance: adapter.Module,
		name:     adapter.Name,
	})
	return nil
}

// execResourceRep creates a core function for canon resource.rep.
// Signature: (handle: i32) -> (rep: i32)
func (step *planResourceRep) execute(ctx context.Context, s *instantiationState) error {
	rtype := lookupResourceType(s, step.typeID)
	baseName := fmt.Sprintf("wacogo_resrep_%d", instanceCounter.Add(1))
	adapter, err := s.inst.engine.canonHost.BuildResourceRep(ctx, baseName,
		canonInstanceView{i: s.inst}, resourceType{rt: rtype})
	if err != nil {
		return fmt.Errorf("wacogo: resource.rep: %w", err)
	}
	s.inst.owned = append(s.inst.owned, adapter)
	s.coreFuncs = append(s.coreFuncs, coreFunc{
		instance: adapter.Module,
		name:     adapter.Name,
	})
	return nil
}
