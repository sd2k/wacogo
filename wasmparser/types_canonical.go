package wasmparser

// CanonicalFunction is the union of canonical function definitions.
// Variants: CanonLift, CanonLower, CanonResourceNew, CanonResourceDrop, CanonResourceRep,
// and the async built-ins CanonTaskReturn, CanonContextGet, CanonContextSet,
// CanonSubtaskCancel, CanonSubtaskDrop, CanonWaitableSetNew, CanonWaitableSetDrop,
// CanonWaitableJoin.
type CanonicalFunction interface {
	canonicalFunction()
}

// CanonLift lifts a core function to a component function.
type CanonLift struct {
	CoreFuncIndex uint32
	TypeIndex     uint32
	Options       []CanonicalOption
}

func (CanonLift) canonicalFunction() {}

// CanonLower lowers a component function to a core function.
type CanonLower struct {
	FuncIndex uint32
	Options   []CanonicalOption
}

func (CanonLower) canonicalFunction() {}

// CanonResourceNew creates a new resource handle.
type CanonResourceNew struct{ TypeIndex uint32 }

func (CanonResourceNew) canonicalFunction() {}

// CanonResourceDrop drops a resource handle.
type CanonResourceDrop struct{ TypeIndex uint32 }

func (CanonResourceDrop) canonicalFunction() {}

// CanonResourceRep gets the representation of a resource handle.
type CanonResourceRep struct{ TypeIndex uint32 }

func (CanonResourceRep) canonicalFunction() {}

// CanonTaskReturn returns a result from an async-lifted export.
type CanonTaskReturn struct {
	Results []FuncParam
	Options []CanonicalOption
}

func (CanonTaskReturn) canonicalFunction() {}

// CanonContextGet reads a slot of the current task's context storage.
type CanonContextGet struct {
	Type  ValType
	Index uint32
}

func (CanonContextGet) canonicalFunction() {}

// CanonContextSet writes a slot of the current task's context storage.
type CanonContextSet struct {
	Type  ValType
	Index uint32
}

func (CanonContextSet) canonicalFunction() {}

// CanonSubtaskCancel requests cancellation of a subtask.
type CanonSubtaskCancel struct{ Async bool }

func (CanonSubtaskCancel) canonicalFunction() {}

// CanonSubtaskDrop drops a resolved subtask.
type CanonSubtaskDrop struct{}

func (CanonSubtaskDrop) canonicalFunction() {}

// CanonWaitableSetNew creates an empty waitable set.
type CanonWaitableSetNew struct{}

func (CanonWaitableSetNew) canonicalFunction() {}

// CanonWaitableSetDrop drops an empty waitable set.
type CanonWaitableSetDrop struct{}

func (CanonWaitableSetDrop) canonicalFunction() {}

// CanonWaitableJoin moves a waitable into a waitable set, or out of any set.
type CanonWaitableJoin struct{}

func (CanonWaitableJoin) canonicalFunction() {}

// CanonicalOption is the union of canonical options.
// Variants: CanonOptUTF8, CanonOptUTF16, CanonOptLatin1UTF16, CanonOptMemory, CanonOptRealloc, CanonOptPostReturn,
// CanonOptAsync, CanonOptCallback.
type CanonicalOption interface {
	canonicalOption()
}

// CanonOptUTF8 selects UTF-8 string encoding.
type CanonOptUTF8 struct{}

func (CanonOptUTF8) canonicalOption() {}

// CanonOptUTF16 selects UTF-16 string encoding.
type CanonOptUTF16 struct{}

func (CanonOptUTF16) canonicalOption() {}

// CanonOptLatin1UTF16 selects Latin-1+UTF-16 string encoding.
type CanonOptLatin1UTF16 struct{}

func (CanonOptLatin1UTF16) canonicalOption() {}

// CanonOptMemory specifies the memory to use for lifting/lowering.
type CanonOptMemory struct{ Index uint32 }

func (CanonOptMemory) canonicalOption() {}

// CanonOptRealloc specifies the realloc function for lifting/lowering.
type CanonOptRealloc struct{ Index uint32 }

func (CanonOptRealloc) canonicalOption() {}

// CanonOptPostReturn specifies a post-return function.
type CanonOptPostReturn struct{ Index uint32 }

func (CanonOptPostReturn) canonicalOption() {}

// CanonOptAsync selects the async ABI.
type CanonOptAsync struct{}

func (CanonOptAsync) canonicalOption() {}

// CanonOptCallback specifies the event-loop callback of an async lift.
type CanonOptCallback struct{ Index uint32 }

func (CanonOptCallback) canonicalOption() {}

// readCanonicalOptions reads a vector of canonical options.
func readCanonicalOptions(r *BinaryReader) ([]CanonicalOption, error) {
	count, err := r.ReadU32()
	if err != nil {
		return nil, err
	}
	if count > MaxCanonicalOptions {
		return nil, errfAt(r.Offset(), "canonical option count %d exceeds maximum %d", count, MaxCanonicalOptions)
	}
	opts := make([]CanonicalOption, 0, count)
	for range count {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		switch b {
		case 0x00:
			opts = append(opts, CanonOptUTF8{})
		case 0x01:
			opts = append(opts, CanonOptUTF16{})
		case 0x02:
			opts = append(opts, CanonOptLatin1UTF16{})
		case 0x03:
			idx, err := r.ReadU32()
			if err != nil {
				return nil, err
			}
			opts = append(opts, CanonOptMemory{Index: idx})
		case 0x04:
			idx, err := r.ReadU32()
			if err != nil {
				return nil, err
			}
			opts = append(opts, CanonOptRealloc{Index: idx})
		case 0x05:
			idx, err := r.ReadU32()
			if err != nil {
				return nil, err
			}
			opts = append(opts, CanonOptPostReturn{Index: idx})
		case 0x06:
			opts = append(opts, CanonOptAsync{})
		case 0x07:
			idx, err := r.ReadU32()
			if err != nil {
				return nil, err
			}
			opts = append(opts, CanonOptCallback{Index: idx})
		default:
			return nil, errfAt(r.Offset()-1, "unknown canonical option: 0x%02x", b)
		}
	}
	return opts, nil
}

// readCanonicalFunction reads a CanonicalFunction from the binary reader.
func readCanonicalFunction(r *BinaryReader) (CanonicalFunction, error) {
	b, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	switch b {
	case 0x00:
		// Lift: 0x00 core_func_idx, options, type_idx
		flagByte, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if flagByte != 0x00 {
			return nil, errfAt(r.Offset()-1, "expected 0x00 core func flag in canon lift, got 0x%02x", flagByte)
		}
		coreFuncIdx, err := r.ReadU32()
		if err != nil {
			return nil, err
		}
		opts, err := readCanonicalOptions(r)
		if err != nil {
			return nil, err
		}
		typeIdx, err := r.ReadU32()
		if err != nil {
			return nil, err
		}
		return CanonLift{CoreFuncIndex: coreFuncIdx, TypeIndex: typeIdx, Options: opts}, nil
	case 0x01:
		// Lower: 0x00 func_idx, options
		flagByte, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if flagByte != 0x00 {
			return nil, errfAt(r.Offset()-1, "expected 0x00 sub-type byte in canon lower, got 0x%02x", flagByte)
		}
		funcIdx, err := r.ReadU32()
		if err != nil {
			return nil, err
		}
		opts, err := readCanonicalOptions(r)
		if err != nil {
			return nil, err
		}
		return CanonLower{FuncIndex: funcIdx, Options: opts}, nil
	case 0x02:
		// resource.new: type_idx
		typeIdx, err := r.ReadU32()
		if err != nil {
			return nil, err
		}
		return CanonResourceNew{TypeIndex: typeIdx}, nil
	case 0x03:
		// resource.drop: type_idx
		typeIdx, err := r.ReadU32()
		if err != nil {
			return nil, err
		}
		return CanonResourceDrop{TypeIndex: typeIdx}, nil
	case 0x04:
		// resource.rep: type_idx
		typeIdx, err := r.ReadU32()
		if err != nil {
			return nil, err
		}
		return CanonResourceRep{TypeIndex: typeIdx}, nil
	case 0x09:
		// task.return: resultlist, options
		results, err := readResultList(r)
		if err != nil {
			return nil, err
		}
		opts, err := readCanonicalOptions(r)
		if err != nil {
			return nil, err
		}
		return CanonTaskReturn{Results: results, Options: opts}, nil
	case 0x0a, 0x0b:
		// context.get / context.set: core valtype, slot index
		tb, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		var vt ValType
		switch tb {
		case 0x7f:
			vt = ValTypeI32
		case 0x7e:
			vt = ValTypeI64
		default:
			return nil, errfAt(r.Offset()-1, "invalid context value type: 0x%02x", tb)
		}
		idx, err := r.ReadU32()
		if err != nil {
			return nil, err
		}
		if b == 0x0a {
			return CanonContextGet{Type: vt, Index: idx}, nil
		}
		return CanonContextSet{Type: vt, Index: idx}, nil
	case 0x06:
		// subtask.cancel: async?
		async, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if async > 0x01 {
			return nil, errfAt(r.Offset()-1, "invalid async flag in subtask.cancel: 0x%02x", async)
		}
		return CanonSubtaskCancel{Async: async == 0x01}, nil
	case 0x0d:
		return CanonSubtaskDrop{}, nil
	case 0x1f:
		return CanonWaitableSetNew{}, nil
	case 0x22:
		return CanonWaitableSetDrop{}, nil
	case 0x23:
		return CanonWaitableJoin{}, nil
	default:
		return nil, errfAt(r.Offset()-1, "unknown canonical function kind: 0x%02x", b)
	}
}
