package wasmparser

import (
	"fmt"
	"strings"
)

func (v *Validator) validateCanonicalSection(p *ComponentCanonicalSectionPayload) error {
	cs := v.current()
	if cs == nil {
		return fmt.Errorf("canonical section outside component")
	}
	for cf, err := range p.Items() {
		if err != nil {
			return err
		}
		if err := v.addCanonical(cs, cf); err != nil {
			return err
		}
	}
	return nil
}

func (v *Validator) addCanonical(cs *ComponentState, cf CanonicalFunction) error {
	if cs.kind != ComponentKindComponent {
		return fmt.Errorf("canonical functions cannot be defined in type declarations")
	}
	switch c := cf.(type) {
	case CanonLift:
		return v.canonLift(cs, c)
	case CanonLower:
		return v.canonLower(cs, c)
	case CanonResourceNew:
		return v.canonResourceNew(cs, c)
	case CanonResourceDrop:
		return v.canonResourceDrop(cs, c)
	case CanonResourceRep:
		return v.canonResourceRep(cs, c)
	case CanonTaskReturn:
		return v.canonTaskReturn(cs, c)
	case CanonContextGet:
		return v.pushBuiltin(cs, CoreFuncTypeDesc{Results: []CoreValType{contextCoreType(c.Type)}}, c.Index == 0, "context.get")
	case CanonContextSet:
		return v.pushBuiltin(cs, CoreFuncTypeDesc{Params: []CoreValType{contextCoreType(c.Type)}}, c.Index == 0, "context.set")
	case CanonSubtaskCancel:
		return v.pushBuiltin(cs, CoreFuncTypeDesc{Params: i32s(1), Results: i32s(1)}, true, "")
	case CanonSubtaskDrop, CanonWaitableSetDrop:
		return v.pushBuiltin(cs, CoreFuncTypeDesc{Params: i32s(1)}, true, "")
	case CanonWaitableSetNew:
		return v.pushBuiltin(cs, CoreFuncTypeDesc{Results: i32s(1)}, true, "")
	case CanonWaitableJoin:
		return v.pushBuiltin(cs, CoreFuncTypeDesc{Params: i32s(2)}, true, "")
	default:
		return fmt.Errorf("unknown canonical function %T", cf)
	}
}

func (v *Validator) canonLift(cs *ComponentState, c CanonLift) error {
	// Validate core function index
	coreFuncID, err := cs.getCoreFunc(c.CoreFuncIndex)
	if err != nil {
		return fmt.Errorf("core %s", err)
	}
	// Validate type index
	ty, err := cs.getType(c.TypeIndex)
	if err != nil {
		return err
	}
	if ty.Kind != AnyTypeFunc {
		return fmt.Errorf("type index %d is not a function type", c.TypeIndex)
	}

	funcType := v.arena.FuncTypes[ty.Index]

	// Validate options, checking memory/realloc requirements for lift
	opts, err := v.checkCanonOptions(cs, c.Options, true)
	if err != nil {
		return err
	}

	if opts.async != funcType.Async && !opts.async {
		return fmt.Errorf("async function type requires the `async` canonical option")
	}
	if opts.hasCallback {
		if !opts.async {
			return fmt.Errorf("canonical option `callback` requires `async`")
		}
		ct := v.arena.CoreFuncTypes[opts.callbackFuncID]
		if !coreValTypesEqual(ct.Params, i32s(3)) || !coreValTypesEqual(ct.Results, i32s(1)) {
			return fmt.Errorf("canonical option `callback` uses a core function with an incorrect signature")
		}
	}

	// Check memory/realloc requirements based on the function type
	needsMemory, needsRealloc := v.liftRequirements(&funcType)
	if needsMemory && !opts.hasMemory {
		return fmt.Errorf("canonical option `memory` is required")
	}
	if needsRealloc && !opts.hasRealloc {
		return fmt.Errorf("canonical option `realloc` is required")
	}

	actual := v.arena.CoreFuncTypes[coreFuncID]

	// Validate realloc signature: (i32 i32 i32 i32) -> i32
	if opts.hasRealloc {
		rt := v.arena.CoreFuncTypes[opts.reallocFuncID]
		if !coreValTypesEqual(rt.Params, reallocParamTypes) || !coreValTypesEqual(rt.Results, reallocResultTypes) {
			return fmt.Errorf("canonical option `realloc` uses a core function with an incorrect signature")
		}
	}

	// Validate post-return signature: params == core func results, no results
	if opts.hasPostReturn {
		pt := v.arena.CoreFuncTypes[opts.postReturnFuncID]
		if !coreValTypesEqual(pt.Params, actual.Results) || len(pt.Results) != 0 {
			return fmt.Errorf("canonical option `post-return` uses a core function with an incorrect signature")
		}
	}

	// Verify the core function's signature matches the canonical lowering
	// of the component function type being lifted to.
	expected := v.flattenFuncType(&funcType, true, opts)
	if !coreValTypesEqual(expected.Params, actual.Params) {
		return fmt.Errorf("lowered parameter types %s do not match parameter types %s",
			formatCoreValTypes(expected.Params), formatCoreValTypes(actual.Params))
	}
	if !coreValTypesEqual(expected.Results, actual.Results) {
		return fmt.Errorf("lowered result types %s do not match result types %s",
			formatCoreValTypes(expected.Results), formatCoreValTypes(actual.Results))
	}

	// Lift produces a component function
	cs.funcs = append(cs.funcs, ComponentFuncTypeID(ty.Index))
	return nil
}

func (v *Validator) canonLower(cs *ComponentState, c CanonLower) error {
	// Validate function index
	funcID, err := cs.getFunc(c.FuncIndex)
	if err != nil {
		return err
	}

	funcType := v.arena.FuncTypes[funcID]

	// Validate options
	opts, err := v.checkCanonOptions(cs, c.Options, false)
	if err != nil {
		return err
	}

	// Check memory/realloc requirements based on the function type
	needsMemory, needsRealloc := v.lowerRequirements(&funcType)
	if needsMemory && !opts.hasMemory {
		return fmt.Errorf("canonical option `memory` is required")
	}
	if needsRealloc && !opts.hasRealloc {
		return fmt.Errorf("canonical option `realloc` is required")
	}

	// Validate realloc signature: (i32 i32 i32 i32) -> i32
	if opts.hasRealloc {
		rt := v.arena.CoreFuncTypes[opts.reallocFuncID]
		if !coreValTypesEqual(rt.Params, reallocParamTypes) || !coreValTypesEqual(rt.Results, reallocResultTypes) {
			return fmt.Errorf("canonical option `realloc` uses a core function with an incorrect signature")
		}
	}

	// Lower produces a core function — compute the lowered signature
	lowered := v.flattenFuncType(&funcType, false, opts)
	loweredType := v.arena.pushCoreFuncType(lowered)
	cs.coreFuncs = append(cs.coreFuncs, loweredType)
	return nil
}

// resourceIsLocal reports whether rid (or the resource it ultimately aliases)
// is locally defined in this component. Resource exports are minted with
// fresh IDs that alias back to the defining declaration; without resolving
// the alias, canon resource.new/rep on an exported alias would falsely
// report "not a local resource".
func (v *Validator) resourceIsLocal(cs *ComponentState, rid ResourceID) bool {
	if cs.localResources[rid] {
		return true
	}
	if defining := v.arena.resolveResourceAlias(rid); defining != rid {
		return cs.localResources[defining]
	}
	return false
}

func (v *Validator) canonResourceNew(cs *ComponentState, c CanonResourceNew) error {
	ty, err := cs.getType(c.TypeIndex)
	if err != nil {
		return err
	}
	if ty.Kind != AnyTypeResource {
		return fmt.Errorf("type index %d is not a resource type", c.TypeIndex)
	}
	if !v.resourceIsLocal(cs, ty.ResID) {
		return fmt.Errorf("not a local resource")
	}
	// resource.new: [rep] -> [i32]
	loweredType := v.arena.pushCoreFuncType(CoreFuncTypeDesc{
		Params:  []CoreValType{CoreValTypeI32},
		Results: []CoreValType{CoreValTypeI32},
	})
	cs.coreFuncs = append(cs.coreFuncs, loweredType)
	return nil
}

func (v *Validator) canonResourceDrop(cs *ComponentState, c CanonResourceDrop) error {
	ty, err := cs.getType(c.TypeIndex)
	if err != nil {
		return err
	}
	if ty.Kind != AnyTypeResource {
		return fmt.Errorf("type index %d is not a resource type", c.TypeIndex)
	}
	// resource.drop: [i32] -> []
	loweredType := v.arena.pushCoreFuncType(CoreFuncTypeDesc{
		Params:  []CoreValType{CoreValTypeI32},
		Results: nil,
	})
	cs.coreFuncs = append(cs.coreFuncs, loweredType)
	return nil
}

func (v *Validator) canonResourceRep(cs *ComponentState, c CanonResourceRep) error {
	ty, err := cs.getType(c.TypeIndex)
	if err != nil {
		return err
	}
	if ty.Kind != AnyTypeResource {
		return fmt.Errorf("type index %d is not a resource type", c.TypeIndex)
	}
	if !v.resourceIsLocal(cs, ty.ResID) {
		return fmt.Errorf("not a local resource")
	}
	// resource.rep: [i32] -> [rep]
	loweredType := v.arena.pushCoreFuncType(CoreFuncTypeDesc{
		Params:  []CoreValType{CoreValTypeI32},
		Results: []CoreValType{CoreValTypeI32},
	})
	cs.coreFuncs = append(cs.coreFuncs, loweredType)
	return nil
}

func (v *Validator) canonTaskReturn(cs *ComponentState, c CanonTaskReturn) error {
	results := make([]FuncParamDesc, len(c.Results))
	for i, r := range c.Results {
		ivt, err := internalValTypeFromParsed(r.Type, cs)
		if err != nil {
			return err
		}
		results[i] = FuncParamDesc{Type: ivt}
	}
	if _, err := v.checkCanonOptions(cs, c.Options, false); err != nil {
		return err
	}
	// task.return takes the result values as flat params, going indirect
	// past maxFlatParams like a sync lift's params.
	ft := FuncTypeDesc{Params: results}
	var params []CoreValType
	if v.flatParamCount(&ft) <= maxFlatParams {
		for _, r := range results {
			params = append(params, v.flattenToCoreTypes(r.Type)...)
		}
	} else {
		params = i32s(1)
	}
	return v.pushBuiltin(cs, CoreFuncTypeDesc{Params: params}, true, "")
}

// pushBuiltin appends a canon built-in's core function type to the core
// function index space. ok=false rejects the built-in with an error naming it.
func (v *Validator) pushBuiltin(cs *ComponentState, ft CoreFuncTypeDesc, ok bool, name string) error {
	if !ok {
		return fmt.Errorf("unsupported immediate for `%s`", name)
	}
	cs.coreFuncs = append(cs.coreFuncs, v.arena.pushCoreFuncType(ft))
	return nil
}

func i32s(n int) []CoreValType {
	out := make([]CoreValType, n)
	for i := range out {
		out[i] = CoreValTypeI32
	}
	return out
}

func contextCoreType(t ValType) CoreValType {
	if t == ValTypeI64 {
		return CoreValTypeI64
	}
	return CoreValTypeI32
}

// maxFlatParams is the maximum number of core wasm params before going indirect.
const maxFlatParams = 16

// maxFlatAsyncParams is the maximum number of core wasm params of an
// async-lowered function before going indirect.
const maxFlatAsyncParams = 4

// maxFlatResults is the maximum number of core wasm results before going indirect.
const maxFlatResults = 1

// canonOptionsResolved holds the validated, deduplicated canonical options
// extracted from a canon-lift or canon-lower's options list.
type canonOptionsResolved struct {
	hasMemory        bool
	hasRealloc       bool
	reallocFuncID    CoreFuncTypeID
	hasPostReturn    bool
	postReturnFuncID CoreFuncTypeID
	async            bool
	hasCallback      bool
	callbackFuncID   CoreFuncTypeID
}

// reallocParamTypes / reallocResultTypes describe the required signature of a
// canonical-ABI realloc function: (i32 i32 i32 i32) -> i32.
var (
	reallocParamTypes  = []CoreValType{CoreValTypeI32, CoreValTypeI32, CoreValTypeI32, CoreValTypeI32}
	reallocResultTypes = []CoreValType{CoreValTypeI32}
)

// coreValTypesEqual returns true when two flat type lists are element-wise equal.
func coreValTypesEqual(a, b []CoreValType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// formatCoreValTypes renders a flat type list using the wasm-tools spec-test
// format, e.g. `[]` or `[I32]` or `[I32, I64]`.
func formatCoreValTypes(types []CoreValType) string {
	parts := make([]string, len(types))
	for i, t := range types {
		switch t {
		case CoreValTypeI32:
			parts[i] = "I32"
		case CoreValTypeI64:
			parts[i] = "I64"
		case CoreValTypeF32:
			parts[i] = "F32"
		case CoreValTypeF64:
			parts[i] = "F64"
		case CoreValTypeV128:
			parts[i] = "V128"
		case CoreValTypeFuncRef:
			parts[i] = "FuncRef"
		case CoreValTypeExternRef:
			parts[i] = "ExternRef"
		default:
			parts[i] = fmt.Sprintf("0x%02x", uint8(t))
		}
	}
	return "`[" + strings.Join(parts, ", ") + "]`"
}

// valTypeContainsPtr returns true if the given value type contains pointers
// (strings, lists) that require linear memory for the canonical ABI.
func (v *Validator) valTypeContainsPtr(ivt ValTypeDesc) bool {
	if ivt.IsPrimitive {
		return ivt.Primitive == PrimString
	}
	return v.walkValType(ivt, func(dt *DefinedTypeDesc) bool {
		return dt.Kind == DefinedKindList
	})
}

// flattenValType returns the number of core wasm values needed to represent this type.
func (v *Validator) flattenValType(ivt ValTypeDesc) int {
	if ivt.IsPrimitive {
		return flattenPrimitiveCount(ivt.Primitive)
	}
	dt := v.arena.DefinedTypes[ivt.TypeID]
	return v.flattenDefinedType(&dt)
}

// flattenPrimitiveCount returns the number of core wasm values a
// primitive component-model type flattens to.
func flattenPrimitiveCount(p PrimitiveValType) int {
	if p == PrimString {
		return 2
	}
	return 1
}

func (v *Validator) flattenDefinedType(dt *DefinedTypeDesc) int {
	switch dt.Kind {
	case DefinedKindPrimitive:
		return flattenPrimitiveCount(dt.Primitive)
	case DefinedKindList:
		return 2 // ptr + len
	case DefinedKindRecord:
		n := 0
		if dt.Record != nil {
			for _, f := range dt.Record.Fields {
				n += v.flattenValType(f.Type)
			}
		}
		return n
	case DefinedKindTuple:
		n := 0
		for _, t := range dt.Tuple {
			n += v.flattenValType(t)
		}
		return n
	case DefinedKindFlags:
		// Each group of 32 flags needs one i32
		nflags := len(dt.Flags)
		return (nflags + 31) / 32
	case DefinedKindVariant:
		// discriminant + max of all case payloads
		maxPayload := 0
		if dt.Variant != nil {
			for _, c := range dt.Variant.Cases {
				if c.Type.Valid {
					n := v.flattenValType(c.Type.Value)
					if n > maxPayload {
						maxPayload = n
					}
				}
			}
		}
		return 1 + maxPayload
	case DefinedKindEnum:
		return 1 // discriminant
	case DefinedKindOption:
		return 1 + v.flattenValType(dt.Option)
	case DefinedKindResult:
		maxPayload := 0
		if dt.ResultOk.Valid {
			n := v.flattenValType(dt.ResultOk.Value)
			if n > maxPayload {
				maxPayload = n
			}
		}
		if dt.ResultErr.Valid {
			n := v.flattenValType(dt.ResultErr.Value)
			if n > maxPayload {
				maxPayload = n
			}
		}
		return 1 + maxPayload
	case DefinedKindOwn, DefinedKindBorrow:
		return 1 // i32 handle
	default:
		return 1
	}
}

// funcTypeAnyContainsPtr returns whether any param or result contains a pointer type.
func (v *Validator) funcTypeParamsContainPtr(ft *FuncTypeDesc) bool {
	for _, p := range ft.Params {
		if v.valTypeContainsPtr(p.Type) {
			return true
		}
	}
	return false
}

func (v *Validator) funcTypeResultsContainPtr(ft *FuncTypeDesc) bool {
	for _, r := range ft.Results {
		if v.valTypeContainsPtr(r.Type) {
			return true
		}
	}
	return false
}

// flatParamCount returns the total number of flat core wasm params for a func type.
func (v *Validator) flatParamCount(ft *FuncTypeDesc) int {
	n := 0
	for _, p := range ft.Params {
		n += v.flattenValType(p.Type)
	}
	return n
}

// flatResultCount returns the total number of flat core wasm results for a func type.
func (v *Validator) flatResultCount(ft *FuncTypeDesc) int {
	n := 0
	for _, r := range ft.Results {
		n += v.flattenValType(r.Type)
	}
	return n
}

// liftRequirements returns whether a canon lift needs memory and realloc.
func (v *Validator) liftRequirements(ft *FuncTypeDesc) (needsMemory, needsRealloc bool) {
	paramsFlat := v.flatParamCount(ft)
	resultsFlat := v.flatResultCount(ft)
	paramsPtr := v.funcTypeParamsContainPtr(ft)
	resultsPtr := v.funcTypeResultsContainPtr(ft)

	// Memory is needed if any type contains pointers or if flattening exceeds limits
	needsMemory = paramsPtr || resultsPtr || paramsFlat > maxFlatParams || resultsFlat > maxFlatResults

	// Realloc is needed for lift if params contain pointers or params go indirect
	// (the component needs to allocate space for incoming args)
	needsRealloc = paramsPtr || paramsFlat > maxFlatParams

	return
}

// lowerRequirements returns whether a canon lower needs memory and realloc.
func (v *Validator) lowerRequirements(ft *FuncTypeDesc) (needsMemory, needsRealloc bool) {
	paramsFlat := v.flatParamCount(ft)
	resultsFlat := v.flatResultCount(ft)
	paramsPtr := v.funcTypeParamsContainPtr(ft)
	resultsPtr := v.funcTypeResultsContainPtr(ft)

	// Memory is needed if any type contains pointers or if flattening exceeds limits
	needsMemory = paramsPtr || resultsPtr || paramsFlat > maxFlatParams || resultsFlat > maxFlatResults

	// Realloc is needed for lower iff results contain a list or string. The
	// lower trampoline must write returned dynamic data into the core
	// caller's memory, which requires invoking the canon-options realloc.
	// Param ptrs (the caller already holds them in core memory and passes
	// ptr+len through), and bare flat overflow on either side (handled by a
	// caller-supplied return pointer or arg pointer), do not by themselves
	// require realloc.
	needsRealloc = resultsPtr

	return
}

// flattenFuncType computes the canonical ABI core signature of a component
// function type. The forLift flag selects the lift vs lower convention for
// indirect results: lift returns the result-buffer pointer as the core result,
// while lower passes a caller-allocated pointer in as an extra param.
func (v *Validator) flattenFuncType(ft *FuncTypeDesc, forLift bool, opts canonOptionsResolved) CoreFuncTypeDesc {
	var params []CoreValType
	var results []CoreValType

	paramsFlat := v.flatParamCount(ft)
	resultsFlat := v.flatResultCount(ft)

	if opts.async {
		// Async lift returns a packed callback code (or nothing without a
		// callback); async lower always returns a packed subtask status and
		// writes results through a trailing pointer.
		maxParams := maxFlatParams
		if !forLift {
			maxParams = maxFlatAsyncParams
		}
		if paramsFlat <= maxParams {
			for _, p := range ft.Params {
				params = append(params, v.flattenToCoreTypes(p.Type)...)
			}
		} else {
			params = []CoreValType{CoreValTypeI32}
		}
		if forLift {
			if opts.hasCallback {
				results = i32s(1)
			}
		} else {
			if resultsFlat > 0 {
				params = append(params, CoreValTypeI32)
			}
			results = i32s(1)
		}
		return CoreFuncTypeDesc{Params: params, Results: results}
	}

	if paramsFlat <= maxFlatParams {
		for _, p := range ft.Params {
			params = append(params, v.flattenToCoreTypes(p.Type)...)
		}
	} else {
		params = []CoreValType{CoreValTypeI32}
	}

	if resultsFlat <= maxFlatResults {
		for _, r := range ft.Results {
			results = append(results, v.flattenToCoreTypes(r.Type)...)
		}
	} else if forLift {
		results = []CoreValType{CoreValTypeI32}
	} else {
		params = append(params, CoreValTypeI32)
	}

	return CoreFuncTypeDesc{Params: params, Results: results}
}

// flattenToCoreTypes returns the core wasm types for a component value type.
func (v *Validator) flattenToCoreTypes(ivt ValTypeDesc) []CoreValType {
	if ivt.IsPrimitive {
		return flattenPrimitiveToCoreTypes(ivt.Primitive)
	}
	dt := v.arena.DefinedTypes[ivt.TypeID]
	return v.flattenDefinedToCoreTypes(&dt)
}

// flattenPrimitiveToCoreTypes returns the core wasm types for a primitive
// component-model value type. Used by both the direct-primitive path
// (ValTypeDesc.IsPrimitive) and the named-alias path
// (DefinedKindPrimitive on a DefinedTypeDesc).
func flattenPrimitiveToCoreTypes(p PrimitiveValType) []CoreValType {
	switch p {
	case PrimString:
		return []CoreValType{CoreValTypeI32, CoreValTypeI32}
	case PrimS64, PrimU64:
		return []CoreValType{CoreValTypeI64}
	case PrimF32:
		return []CoreValType{CoreValTypeF32}
	case PrimF64:
		return []CoreValType{CoreValTypeF64}
	default:
		return []CoreValType{CoreValTypeI32}
	}
}

func (v *Validator) flattenDefinedToCoreTypes(dt *DefinedTypeDesc) []CoreValType {
	switch dt.Kind {
	case DefinedKindPrimitive:
		return flattenPrimitiveToCoreTypes(dt.Primitive)
	case DefinedKindList:
		return []CoreValType{CoreValTypeI32, CoreValTypeI32}
	case DefinedKindRecord:
		var types []CoreValType
		if dt.Record != nil {
			for _, f := range dt.Record.Fields {
				types = append(types, v.flattenToCoreTypes(f.Type)...)
			}
		}
		return types
	case DefinedKindTuple:
		var types []CoreValType
		for _, t := range dt.Tuple {
			types = append(types, v.flattenToCoreTypes(t)...)
		}
		return types
	case DefinedKindVariant:
		var payload []CoreValType
		if dt.Variant != nil {
			for _, c := range dt.Variant.Cases {
				if c.Type.Valid {
					payload = joinCoreFlatLists(payload, v.flattenToCoreTypes(c.Type.Value))
				}
			}
		}
		return append([]CoreValType{CoreValTypeI32}, payload...)
	case DefinedKindEnum:
		return []CoreValType{CoreValTypeI32}
	case DefinedKindOption:
		payload := v.flattenToCoreTypes(dt.Option)
		return append([]CoreValType{CoreValTypeI32}, payload...)
	case DefinedKindResult:
		var payload []CoreValType
		if dt.ResultOk.Valid {
			payload = joinCoreFlatLists(payload, v.flattenToCoreTypes(dt.ResultOk.Value))
		}
		if dt.ResultErr.Valid {
			payload = joinCoreFlatLists(payload, v.flattenToCoreTypes(dt.ResultErr.Value))
		}
		return append([]CoreValType{CoreValTypeI32}, payload...)
	case DefinedKindFlags:
		n := (len(dt.Flags) + 31) / 32
		if n == 0 {
			n = 1
		}
		types := make([]CoreValType, n)
		for i := range types {
			types[i] = CoreValTypeI32
		}
		return types
	case DefinedKindOwn, DefinedKindBorrow:
		return []CoreValType{CoreValTypeI32}
	default:
		return []CoreValType{CoreValTypeI32}
	}
}

// joinCoreFlatLists performs element-wise canonical ABI join of two flat lists,
// extending to the length of the longer input. Used for variant/result payloads.
func joinCoreFlatLists(a, b []CoreValType) []CoreValType {
	if len(b) > len(a) {
		a, b = b, a
	}
	result := make([]CoreValType, len(a))
	copy(result, a)
	for i := range b {
		result[i] = joinCoreValType(result[i], b[i])
	}
	return result
}

// joinCoreValType returns the canonical ABI "join" of two core value types.
// i32 join f32 → i32; all other mixed combinations → i64.
func joinCoreValType(a, b CoreValType) CoreValType {
	if a == b {
		return a
	}
	if (a == CoreValTypeI32 && b == CoreValTypeF32) || (a == CoreValTypeF32 && b == CoreValTypeI32) {
		return CoreValTypeI32
	}
	return CoreValTypeI64
}

func (v *Validator) checkCanonOptions(cs *ComponentState, opts []CanonicalOption, isLift bool) (canonOptionsResolved, error) {
	var out canonOptionsResolved
	encodingName := ""

	for _, opt := range opts {
		switch o := opt.(type) {
		case CanonOptUTF8, CanonOptUTF16, CanonOptLatin1UTF16:
			name := canonEncodingName(opt)
			if encodingName != "" {
				return out, fmt.Errorf("canonical encoding option `%s` conflicts with option `%s`",
					encodingName, name)
			}
			encodingName = name
		case CanonOptMemory:
			if out.hasMemory {
				return out, fmt.Errorf("`memory` is specified more than once")
			}
			out.hasMemory = true
			_, err := cs.getCoreMemory(o.Index)
			if err != nil {
				return out, err
			}
		case CanonOptRealloc:
			if out.hasRealloc {
				return out, fmt.Errorf("canonical option `realloc` is specified more than once")
			}
			out.hasRealloc = true
			id, err := cs.getCoreFunc(o.Index)
			if err != nil {
				return out, err
			}
			out.reallocFuncID = id
		case CanonOptPostReturn:
			if !isLift {
				return out, fmt.Errorf("canonical option `post-return` cannot be specified for lowerings")
			}
			if out.hasPostReturn {
				return out, fmt.Errorf("canonical option `post-return` is specified more than once")
			}
			out.hasPostReturn = true
			id, err := cs.getCoreFunc(o.Index)
			if err != nil {
				return out, err
			}
			out.postReturnFuncID = id
		case CanonOptAsync:
			if out.async {
				return out, fmt.Errorf("canonical option `async` is specified more than once")
			}
			out.async = true
		case CanonOptCallback:
			if !isLift {
				return out, fmt.Errorf("canonical option `callback` cannot be specified for lowerings")
			}
			if out.hasCallback {
				return out, fmt.Errorf("canonical option `callback` is specified more than once")
			}
			out.hasCallback = true
			id, err := cs.getCoreFunc(o.Index)
			if err != nil {
				return out, err
			}
			out.callbackFuncID = id
		}
	}
	return out, nil
}

// canonEncodingName returns the spec-test display name for a string-encoding
// canonical option (utf8, utf16, latin1-utf16).
func canonEncodingName(opt CanonicalOption) string {
	switch opt.(type) {
	case CanonOptUTF8:
		return "utf8"
	case CanonOptUTF16:
		return "utf16"
	case CanonOptLatin1UTF16:
		return "latin1-utf16"
	default:
		return ""
	}
}

// ---------- Instance Section ----------
