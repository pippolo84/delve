package proc

import (
	"bytes"
	"debug/dwarf"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"path/filepath"
	"strings"

	"github.com/go-delve/delve/pkg/astutil"
	"github.com/go-delve/delve/pkg/dwarf/reader"
)

const maxSkipAutogeneratedWrappers = 5 // maximum recursion depth for skipAutogeneratedWrappers

// ErrNoSourceForPC is returned when the given address
// does not correspond with a source file location.
type ErrNoSourceForPC struct {
	pc uint64
}

func (err *ErrNoSourceForPC) Error() string {
	return fmt.Sprintf("no source for PC %#x", err.pc)
}

// Next continues execution until the next source line.
func (dbp *Target) Next() (err error) {
	if _, err := dbp.Valid(); err != nil {
		return err
	}
	if dbp.Breakpoints().HasSteppingBreakpoints() {
		return fmt.Errorf("next while nexting")
	}

	if err = next(dbp, false, false); err != nil {
		dbp.ClearSteppingBreakpoints()
		return
	}

	return dbp.Continue()
}

// Continue continues execution of the debugged
// process. It will continue until it hits a breakpoint
// or is otherwise stopped.
func (dbp *Target) Continue() error {
	if _, err := dbp.Valid(); err != nil {
		return err
	}
	for _, thread := range dbp.ThreadList() {
		thread.Common().CallReturn = false
		thread.Common().returnValues = nil
	}
	dbp.Breakpoints().WatchOutOfScope = nil
	dbp.CheckAndClearManualStopRequest()
	defer func() {
		// Make sure we clear internal breakpoints if we simultaneously receive a
		// manual stop request and hit a breakpoint.
		if dbp.CheckAndClearManualStopRequest() {
			dbp.StopReason = StopManual
			if dbp.KeepSteppingBreakpoints&HaltKeepsSteppingBreakpoints == 0 {
				dbp.ClearSteppingBreakpoints()
			}
		}
	}()
	for {
		if dbp.CheckAndClearManualStopRequest() {
			dbp.StopReason = StopManual
			if dbp.KeepSteppingBreakpoints&HaltKeepsSteppingBreakpoints == 0 {
				dbp.ClearSteppingBreakpoints()
			}
			return nil
		}
		dbp.ClearCaches()
		trapthread, stopReason, contOnceErr := dbp.proc.ContinueOnce()
		dbp.StopReason = stopReason

		threads := dbp.ThreadList()
		for _, thread := range threads {
			if thread.Breakpoint().Breakpoint != nil {
				thread.Breakpoint().Breakpoint.checkCondition(dbp, thread, thread.Breakpoint())
			}
		}

		if contOnceErr != nil {
			// Attempt to refresh status of current thread/current goroutine, see
			// Issue #2078.
			// Errors are ignored because depending on why ContinueOnce failed this
			// might very well not work.
			if valid, _ := dbp.Valid(); valid {
				if trapthread != nil {
					_ = dbp.SwitchThread(trapthread.ThreadID())
				} else if curth := dbp.CurrentThread(); curth != nil {
					dbp.selectedGoroutine, _ = GetG(curth)
				}
			}
			if pe, ok := contOnceErr.(ErrProcessExited); ok {
				dbp.exitStatus = pe.Status
			}
			return contOnceErr
		}
		if dbp.StopReason == StopLaunched {
			dbp.ClearSteppingBreakpoints()
		}

		callInjectionDone, callErr := callInjectionProtocol(dbp, threads)
		// callErr check delayed until after pickCurrentThread, which must always
		// happen, otherwise the debugger could be left in an inconsistent
		// state.

		if err := pickCurrentThread(dbp, trapthread, threads); err != nil {
			return err
		}

		if callErr != nil {
			return callErr
		}

		curthread := dbp.CurrentThread()
		curbp := curthread.Breakpoint()

		switch {
		case curbp.Breakpoint == nil:
			// runtime.Breakpoint, manual stop or debugCallV1-related stop

			loc, err := curthread.Location()
			if err != nil || loc.Fn == nil {
				return conditionErrors(threads)
			}
			g, _ := GetG(curthread)
			arch := dbp.BinInfo().Arch

			switch {
			case loc.Fn.Name == "runtime.breakpoint":
				if recorded, _ := dbp.Recorded(); recorded {
					return conditionErrors(threads)
				}
				// In linux-arm64, PtraceSingleStep seems cannot step over BRK instruction
				// (linux-arm64 feature or kernel bug maybe).
				if !arch.BreakInstrMovesPC() {
					setPC(curthread, loc.PC+uint64(arch.BreakpointSize()))
				}
				// Single-step current thread until we exit runtime.breakpoint and
				// runtime.Breakpoint.
				// On go < 1.8 it was sufficient to single-step twice on go1.8 a change
				// to the compiler requires 4 steps.
				if err := stepInstructionOut(dbp, curthread, "runtime.breakpoint", "runtime.Breakpoint"); err != nil {
					return err
				}
				dbp.StopReason = StopHardcodedBreakpoint
				return conditionErrors(threads)
			case g == nil || dbp.fncallForG[g.ID] == nil:
				// a hardcoded breakpoint somewhere else in the code (probably cgo), or manual stop in cgo
				if !arch.BreakInstrMovesPC() {
					bpsize := arch.BreakpointSize()
					bp := make([]byte, bpsize)
					dbp.Memory().ReadMemory(bp, loc.PC)
					if bytes.Equal(bp, arch.BreakpointInstruction()) {
						setPC(curthread, loc.PC+uint64(bpsize))
					}
				}
				return conditionErrors(threads)
			}
		case curbp.Active && curbp.Stepping:
			if curbp.SteppingInto {
				// See description of proc.(*Process).next for the meaning of StepBreakpoints
				if err := conditionErrors(threads); err != nil {
					return err
				}
				if dbp.GetDirection() == Forward {
					text, err := disassembleCurrentInstruction(dbp, curthread, 0)
					if err != nil {
						return err
					}
					var fn *Function
					if loc, _ := curthread.Location(); loc != nil {
						fn = loc.Fn
					}
					// here we either set a breakpoint into the destination of the CALL
					// instruction or we determined that the called function is hidden,
					// either way we need to resume execution
					if err = setStepIntoBreakpoint(dbp, fn, text, sameGoroutineCondition(dbp.SelectedGoroutine())); err != nil {
						return err
					}
				} else {
					if err := dbp.ClearSteppingBreakpoints(); err != nil {
						return err
					}
					return dbp.StepInstruction()
				}
			} else {
				curthread.Common().returnValues = curbp.Breakpoint.returnInfo.Collect(dbp, curthread)
				if err := dbp.ClearSteppingBreakpoints(); err != nil {
					return err
				}
				dbp.StopReason = StopNextFinished
				return conditionErrors(threads)
			}
		case curbp.Active:
			onNextGoroutine, err := onNextGoroutine(dbp, curthread, dbp.Breakpoints())
			if err != nil {
				return err
			}
			if onNextGoroutine &&
				((!curbp.Tracepoint && !curbp.TraceReturn) || dbp.KeepSteppingBreakpoints&TracepointKeepsSteppingBreakpoints == 0) {
				err := dbp.ClearSteppingBreakpoints()
				if err != nil {
					return err
				}
			}
			if curbp.Name == UnrecoveredPanic {
				dbp.ClearSteppingBreakpoints()
			}
			dbp.StopReason = StopBreakpoint
			if curbp.Breakpoint.WatchType != 0 {
				dbp.StopReason = StopWatchpoint
			}
			if curbp.IsHitCondNoMoreSatisfiable() {
				if err := dbp.ClearBreakpoint(curbp.Addr); err != nil {
					return err
				}
			}
			return conditionErrors(threads)
		default:
			// not a manual stop, not on runtime.Breakpoint, not on a breakpoint, just repeat
		}
		if callInjectionDone {
			// a call injection was finished, don't let a breakpoint with a failed
			// condition or a step breakpoint shadow this.
			dbp.StopReason = StopCallReturned
			return conditionErrors(threads)
		}
	}
}

func conditionErrors(threads []Thread) error {
	var condErr error
	for _, th := range threads {
		if bp := th.Breakpoint(); bp.Breakpoint != nil && bp.CondError != nil {
			if condErr == nil {
				condErr = bp.CondError
			} else {
				return fmt.Errorf("multiple errors evaluating conditions")
			}
		}
	}
	return condErr
}

// pick a new dbp.currentThread, with the following priority:
// 	- a thread with onTriggeredInternalBreakpoint() == true
// 	- a thread with onTriggeredBreakpoint() == true (prioritizing trapthread)
// 	- trapthread
func pickCurrentThread(dbp *Target, trapthread Thread, threads []Thread) error {
	for _, th := range threads {
		if bp := th.Breakpoint(); bp.Active && bp.Stepping {
			return dbp.SwitchThread(th.ThreadID())
		}
	}
	if bp := trapthread.Breakpoint(); bp.Active {
		return dbp.SwitchThread(trapthread.ThreadID())
	}
	for _, th := range threads {
		if bp := th.Breakpoint(); bp.Active {
			return dbp.SwitchThread(th.ThreadID())
		}
	}
	return dbp.SwitchThread(trapthread.ThreadID())
}

func disassembleCurrentInstruction(p Process, thread Thread, off int64) ([]AsmInstruction, error) {
	regs, err := thread.Registers()
	if err != nil {
		return nil, err
	}
	pc := regs.PC() + uint64(off)
	return disassemble(p.Memory(), regs, p.Breakpoints(), p.BinInfo(), pc, pc+uint64(p.BinInfo().Arch.MaxInstructionLength()), true)
}

// stepInstructionOut repeatedly calls StepInstruction until the current
// function is neither fnname1 or fnname2.
// This function is used to step out of runtime.Breakpoint as well as
// runtime.debugCallV1.
func stepInstructionOut(dbp *Target, curthread Thread, fnname1, fnname2 string) error {
	defer dbp.ClearCaches()
	for {
		if err := curthread.StepInstruction(); err != nil {
			return err
		}
		loc, err := curthread.Location()
		var locFnName string
		if loc.Fn != nil {
			locFnName = loc.Fn.Name
			// Calls to runtime.Breakpoint are inlined in some versions of Go when
			// inlining is enabled. Here we attempt to resolve any inlining.
			dwarfTree, _ := loc.Fn.cu.image.getDwarfTree(loc.Fn.offset)
			if dwarfTree != nil {
				inlstack := reader.InlineStack(dwarfTree, loc.PC)
				if len(inlstack) > 0 {
					if locFnName2, ok := inlstack[0].Val(dwarf.AttrName).(string); ok {
						locFnName = locFnName2
					}
				}
			}
		}
		if err != nil || loc.Fn == nil || (locFnName != fnname1 && locFnName != fnname2) {
			g, _ := GetG(curthread)
			selg := dbp.SelectedGoroutine()
			if g != nil && selg != nil && g.ID == selg.ID {
				selg.CurrentLoc = *loc
			}
			return curthread.SetCurrentBreakpoint(true)
		}
	}
}

// Step will continue until another source line is reached.
// Will step into functions.
func (dbp *Target) Step() (err error) {
	if _, err := dbp.Valid(); err != nil {
		return err
	}
	if dbp.Breakpoints().HasSteppingBreakpoints() {
		return fmt.Errorf("next while nexting")
	}

	if err = next(dbp, true, false); err != nil {
		_ = dbp.ClearSteppingBreakpoints()
		return err
	}

	if bpstate := dbp.CurrentThread().Breakpoint(); bpstate.Breakpoint != nil && bpstate.Active && bpstate.SteppingInto && dbp.GetDirection() == Backward {
		dbp.ClearSteppingBreakpoints()
		return dbp.StepInstruction()
	}

	return dbp.Continue()
}

// sameGoroutineCondition returns an expression that evaluates to true when
// the current goroutine is g.
func sameGoroutineCondition(g *G) ast.Expr {
	if g == nil {
		return nil
	}
	return astutil.Eql(astutil.Sel(astutil.PkgVar("runtime", "curg"), "goid"), astutil.Int(int64(g.ID)))
}

func frameoffCondition(frame *Stackframe) ast.Expr {
	return astutil.Eql(astutil.PkgVar("runtime", "frameoff"), astutil.Int(frame.FrameOffset()))
}

// StepOut will continue until the current goroutine exits the
// function currently being executed or a deferred function is executed
func (dbp *Target) StepOut() error {
	backward := dbp.GetDirection() == Backward
	if _, err := dbp.Valid(); err != nil {
		return err
	}
	if dbp.Breakpoints().HasSteppingBreakpoints() {
		return fmt.Errorf("next while nexting")
	}

	selg := dbp.SelectedGoroutine()
	curthread := dbp.CurrentThread()

	topframe, retframe, err := topframe(selg, curthread)
	if err != nil {
		return err
	}

	success := false
	defer func() {
		if !success {
			dbp.ClearSteppingBreakpoints()
		}
	}()

	if topframe.Inlined {
		if err := next(dbp, false, true); err != nil {
			return err
		}

		success = true
		return dbp.Continue()
	}

	sameGCond := sameGoroutineCondition(selg)

	if backward {
		if err := stepOutReverse(dbp, topframe, retframe, sameGCond); err != nil {
			return err
		}

		success = true
		return dbp.Continue()
	}

	deferpc, err := setDeferBreakpoint(dbp, nil, topframe, sameGCond, false)
	if err != nil {
		return err
	}

	if topframe.Ret == 0 && deferpc == 0 {
		return errors.New("nothing to stepout to")
	}

	if topframe.Ret != 0 {
		topframe, retframe := skipAutogeneratedWrappersOut(selg, curthread, &topframe, &retframe)
		retFrameCond := astutil.And(sameGCond, frameoffCondition(retframe))
		bp, err := allowDuplicateBreakpoint(dbp.SetBreakpoint(retframe.Current.PC, NextBreakpoint, retFrameCond))
		if err != nil {
			return err
		}
		if bp != nil {
			configureReturnBreakpoint(dbp.BinInfo(), bp, topframe, retFrameCond)
		}
	}

	if bp := curthread.Breakpoint(); bp.Breakpoint == nil {
		curthread.SetCurrentBreakpoint(false)
	}

	success = true
	return dbp.Continue()
}

// StepInstruction will continue the current thread for exactly
// one instruction. This method affects only the thread
// associated with the selected goroutine. All other
// threads will remain stopped.
func (dbp *Target) StepInstruction() (err error) {
	thread := dbp.CurrentThread()
	g := dbp.SelectedGoroutine()
	if g != nil {
		if g.Thread == nil {
			// Step called on parked goroutine
			if _, err := dbp.SetBreakpoint(g.PC, NextBreakpoint,
				sameGoroutineCondition(dbp.SelectedGoroutine())); err != nil {
				return err
			}
			return dbp.Continue()
		}
		thread = g.Thread
	}
	dbp.ClearCaches()
	if ok, err := dbp.Valid(); !ok {
		return err
	}
	err = thread.StepInstruction()
	if err != nil {
		return err
	}
	thread.Breakpoint().Clear()
	err = thread.SetCurrentBreakpoint(true)
	if err != nil {
		return err
	}
	if tg, _ := GetG(thread); tg != nil {
		dbp.selectedGoroutine = tg
	}
	return nil
}

// Set breakpoints at every line, and the return address. Also look for
// a deferred function and set a breakpoint there too.
// If stepInto is true it will also set breakpoints inside all
// functions called on the current source line, for non-absolute CALLs
// a breakpoint of kind StepBreakpoint is set on the CALL instruction,
// Continue will take care of setting a breakpoint to the destination
// once the CALL is reached.
//
// Regardless of stepInto the following breakpoints will be set:
// - a breakpoint on the first deferred function with NextDeferBreakpoint
//   kind, the list of all the addresses to deferreturn calls in this function
//   and condition checking that we remain on the same goroutine
// - a breakpoint on each line of the function, with a condition checking
//   that we stay on the same stack frame and goroutine.
// - a breakpoint on the return address of the function, with a condition
//   checking that we move to the previous stack frame and stay on the same
//   goroutine.
//
// The breakpoint on the return address is *not* set if the current frame is
// an inlined call. For inlined calls topframe.Current.Fn is the function
// where the inlining happened and the second set of breakpoints will also
// cover the "return address".
//
// If inlinedStepOut is true this function implements the StepOut operation
// for an inlined function call. Everything works the same as normal except
// when removing instructions belonging to inlined calls we also remove all
// instructions belonging to the current inlined call.
func next(dbp *Target, stepInto, inlinedStepOut bool) error {
	backward := dbp.GetDirection() == Backward
	selg := dbp.SelectedGoroutine()
	curthread := dbp.CurrentThread()
	topframe, retframe, err := topframe(selg, curthread)
	if err != nil {
		return err
	}

	if topframe.Current.Fn == nil {
		return &ErrNoSourceForPC{topframe.Current.PC}
	}

	if backward && retframe.Current.Fn == nil {
		return &ErrNoSourceForPC{retframe.Current.PC}
	}

	// sanity check
	if inlinedStepOut && !topframe.Inlined {
		panic("next called with inlinedStepOut but topframe was not inlined")
	}

	success := false
	defer func() {
		if !success {
			dbp.ClearSteppingBreakpoints()
		}
	}()

	ext := filepath.Ext(topframe.Current.File)
	csource := ext != ".go" && ext != ".s"
	var regs Registers
	if selg != nil && selg.Thread != nil {
		regs, err = selg.Thread.Registers()
		if err != nil {
			return err
		}
	}

	sameGCond := sameGoroutineCondition(selg)

	var firstPCAfterPrologue uint64

	if backward {
		firstPCAfterPrologue, err = FirstPCAfterPrologue(dbp, topframe.Current.Fn, false)
		if err != nil {
			return err
		}
		if firstPCAfterPrologue == topframe.Current.PC {
			// We don't want to step into the prologue so we just execute a reverse step out instead
			if err := stepOutReverse(dbp, topframe, retframe, sameGCond); err != nil {
				return err
			}

			success = true
			return nil
		}

		topframe.Ret, err = findCallInstrForRet(dbp, dbp.Memory(), topframe.Ret, retframe.Current.Fn)
		if err != nil {
			return err
		}
	}

	text, err := disassemble(dbp.Memory(), regs, dbp.Breakpoints(), dbp.BinInfo(), topframe.Current.Fn.Entry, topframe.Current.Fn.End, false)
	if err != nil && stepInto {
		return err
	}

	var sameFrameCond ast.Expr
	if sameGCond != nil {
		sameFrameCond = astutil.And(sameGCond, frameoffCondition(&topframe))
	}

	if stepInto && !backward {
		err := setStepIntoBreakpoints(dbp, topframe.Current.Fn, text, topframe, sameGCond)
		if err != nil {
			return err
		}
	}

	if !backward {
		_, err = setDeferBreakpoint(dbp, text, topframe, sameGCond, stepInto)
		if err != nil {
			return err
		}
	}

	// Add breakpoints on all the lines in the current function
	pcs, err := topframe.Current.Fn.cu.lineInfo.AllPCsBetween(topframe.Current.Fn.Entry, topframe.Current.Fn.End-1, topframe.Current.File, topframe.Current.Line)
	if err != nil {
		return err
	}

	if backward {
		// Ensure that pcs contains firstPCAfterPrologue when reverse stepping.
		found := false
		for _, pc := range pcs {
			if pc == firstPCAfterPrologue {
				found = true
				break
			}
		}
		if !found {
			pcs = append(pcs, firstPCAfterPrologue)
		}
	}

	if !stepInto {
		// Removing any PC range belonging to an inlined call
		frame := topframe
		if inlinedStepOut {
			frame = retframe
		}
		pcs, err = removeInlinedCalls(pcs, frame)
		if err != nil {
			return err
		}
	}

	if !csource {
		var covered bool
		for i := range pcs {
			if topframe.Current.Fn.Entry <= pcs[i] && pcs[i] < topframe.Current.Fn.End {
				covered = true
				break
			}
		}

		if !covered {
			fn := dbp.BinInfo().PCToFunc(topframe.Ret)
			if selg != nil && fn != nil && fn.Name == "runtime.goexit" {
				return nil
			}
		}
	}

	for _, pc := range pcs {
		if _, err := allowDuplicateBreakpoint(dbp.SetBreakpoint(pc, NextBreakpoint, sameFrameCond)); err != nil {
			dbp.ClearSteppingBreakpoints()
			return err
		}
	}

	if stepInto && backward {
		err := setStepIntoBreakpointsReverse(dbp, text, topframe, sameGCond)
		if err != nil {
			return err
		}
	}

	if !topframe.Inlined {
		topframe, retframe := skipAutogeneratedWrappersOut(selg, curthread, &topframe, &retframe)
		retFrameCond := astutil.And(sameGCond, frameoffCondition(retframe))

		// Add a breakpoint on the return address for the current frame.
		// For inlined functions there is no need to do this, the set of PCs
		// returned by the AllPCsBetween call above already cover all instructions
		// of the containing function.
		bp, _ := dbp.SetBreakpoint(retframe.Current.PC, NextBreakpoint, retFrameCond)
		// Return address could be wrong, if we are unable to set a breakpoint
		// there it's ok.
		if bp != nil {
			configureReturnBreakpoint(dbp.BinInfo(), bp, topframe, retFrameCond)
		}
	}

	if bp := curthread.Breakpoint(); bp.Breakpoint == nil {
		curthread.SetCurrentBreakpoint(false)
	}
	success = true
	return nil
}

func setStepIntoBreakpoints(dbp *Target, curfn *Function, text []AsmInstruction, topframe Stackframe, sameGCond ast.Expr) error {
	for _, instr := range text {
		if instr.Loc.File != topframe.Current.File || instr.Loc.Line != topframe.Current.Line || !instr.IsCall() {
			continue
		}

		if instr.DestLoc != nil {
			if err := setStepIntoBreakpoint(dbp, curfn, []AsmInstruction{instr}, sameGCond); err != nil {
				return err
			}
		} else {
			// Non-absolute call instruction, set a StepBreakpoint here
			if _, err := allowDuplicateBreakpoint(dbp.SetBreakpoint(instr.Loc.PC, StepBreakpoint, sameGCond)); err != nil {
				return err
			}
		}
	}
	return nil
}

func setStepIntoBreakpointsReverse(dbp *Target, text []AsmInstruction, topframe Stackframe, sameGCond ast.Expr) error {
	bpmap := dbp.Breakpoints()
	// Set a breakpoint after every CALL instruction
	for i, instr := range text {
		if instr.Loc.File != topframe.Current.File || !instr.IsCall() || instr.DestLoc == nil || instr.DestLoc.Fn == nil {
			continue
		}

		if instr.DestLoc.Fn.privateRuntime() {
			continue
		}

		if nextIdx := i + 1; nextIdx < len(text) {
			_, ok := bpmap.M[text[nextIdx].Loc.PC]
			if !ok {
				if _, err := allowDuplicateBreakpoint(dbp.SetBreakpoint(text[nextIdx].Loc.PC, StepBreakpoint, sameGCond)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func FindDeferReturnCalls(text []AsmInstruction) []uint64 {
	const deferreturn = "runtime.deferreturn"
	deferreturns := []uint64{}

	// Find all runtime.deferreturn locations in the function
	// See documentation of Breakpoint.DeferCond for why this is necessary
	for _, instr := range text {
		if instr.IsCall() && instr.DestLoc != nil && instr.DestLoc.Fn != nil && instr.DestLoc.Fn.Name == deferreturn {
			deferreturns = append(deferreturns, instr.Loc.PC)
		}
	}
	return deferreturns
}

// Removes instructions belonging to inlined calls of topframe from pcs.
// If includeCurrentFn is true it will also remove all instructions
// belonging to the current function.
func removeInlinedCalls(pcs []uint64, topframe Stackframe) ([]uint64, error) {
	dwarfTree, err := topframe.Call.Fn.cu.image.getDwarfTree(topframe.Call.Fn.offset)
	if err != nil {
		return pcs, err
	}
	for _, e := range reader.InlineStack(dwarfTree, 0) {
		if e.Offset == topframe.Call.Fn.offset {
			continue
		}
		for _, rng := range e.Ranges {
			pcs = removePCsBetween(pcs, rng[0], rng[1])
		}
	}
	return pcs, nil
}

func removePCsBetween(pcs []uint64, start, end uint64) []uint64 {
	out := pcs[:0]
	for _, pc := range pcs {
		if pc < start || pc >= end {
			out = append(out, pc)
		}
	}
	return out
}

func setStepIntoBreakpoint(dbp *Target, curfn *Function, text []AsmInstruction, cond ast.Expr) error {
	if len(text) <= 0 {
		return nil
	}

	// If the current function is already a runtime function then
	// setStepIntoBreakpoint is allowed to step into unexported runtime
	// functions.
	stepIntoUnexportedRuntime := curfn != nil && strings.HasPrefix(curfn.Name, "runtime.")

	instr := text[0]

	if instr.DestLoc == nil {
		// Call destination couldn't be resolved because this was not the
		// current instruction, therefore the step-into breakpoint can not be set.
		return nil
	}

	fn := instr.DestLoc.Fn

	// Skip unexported runtime functions
	if !stepIntoUnexportedRuntime && fn != nil && fn.privateRuntime() {
		return nil
	}

	//TODO(aarzilli): if we want to let users hide functions
	// or entire packages from being stepped into with 'step'
	// those extra checks should be done here.

	pc := instr.DestLoc.PC

	// Skip InhibitStepInto functions for different arch.
	if dbp.BinInfo().Arch.inhibitStepInto(dbp.BinInfo(), pc) {
		return nil
	}

	fn, pc = skipAutogeneratedWrappersIn(dbp, fn, pc)

	// We want to skip the function prologue but we should only do it if the
	// destination address of the CALL instruction is the entry point of the
	// function.
	// Calls to runtime.duffzero and duffcopy inserted by the compiler can
	// sometimes point inside the body of those functions, well after the
	// prologue.
	if fn != nil && fn.Entry == pc {
		pc, _ = FirstPCAfterPrologue(dbp, fn, false)
	}

	// Set a breakpoint after the function's prologue
	if _, err := allowDuplicateBreakpoint(dbp.SetBreakpoint(pc, NextBreakpoint, cond)); err != nil {
		return err
	}

	return nil
}

func allowDuplicateBreakpoint(bp *Breakpoint, err error) (*Breakpoint, error) {
	if err != nil {
		//lint:ignore S1020 this is clearer
		if _, isexists := err.(BreakpointExistsError); isexists {
			return bp, nil
		}
	}
	return bp, err
}

func isAutogenerated(loc Location) bool {
	return loc.File == "<autogenerated>" && loc.Line == 1
}

func isAutogeneratedOrDeferReturn(loc Location) bool {
	return isAutogenerated(loc) || (loc.Fn != nil && loc.Fn.Name == "runtime.deferreturn")
}

// skipAutogeneratedWrappers skips autogenerated wrappers when setting a
// step-into breakpoint.
// See genwrapper in: $GOROOT/src/cmd/compile/internal/gc/subr.go
func skipAutogeneratedWrappersIn(p Process, startfn *Function, startpc uint64) (*Function, uint64) {
	if startfn == nil {
		return nil, startpc
	}
	fn := startfn
	for count := 0; count < maxSkipAutogeneratedWrappers; count++ {
		if !fn.cu.isgo {
			// can't exit Go
			return startfn, startpc
		}
		text, err := Disassemble(p.Memory(), nil, p.Breakpoints(), p.BinInfo(), fn.Entry, fn.End)
		if err != nil {
			break
		}
		if len(text) == 0 {
			break
		}
		if !isAutogenerated(text[0].Loc) {
			return fn, fn.Entry
		}
		tgtfns := []*Function{}
		// collect all functions called by the current destination function
		for _, instr := range text {
			switch {
			case instr.IsCall():
				if instr.DestLoc == nil || instr.DestLoc.Fn == nil {
					return startfn, startpc
				}
				// calls to non private runtime functions
				if !instr.DestLoc.Fn.privateRuntime() {
					tgtfns = append(tgtfns, instr.DestLoc.Fn)
				}
			case instr.IsJmp():
				// unconditional jumps to a different function that isn't a private runtime function
				if instr.DestLoc != nil && instr.DestLoc.Fn != fn && !instr.DestLoc.Fn.privateRuntime() {
					tgtfns = append(tgtfns, instr.DestLoc.Fn)
				}
			}
		}
		if len(tgtfns) != 1 {
			// too many or not enough function calls
			break
		}

		tgtfn := tgtfns[0]
		if tgtfn.BaseName() != fn.BaseName() {
			return startfn, startpc
		}
		fn = tgtfn
	}
	return startfn, startpc
}

// skipAutogeneratedWrappersOut skip autogenerated wrappers when setting a
// step out breakpoint.
// See genwrapper in: $GOROOT/src/cmd/compile/internal/gc/subr.go
// It also skips runtime.deferreturn frames (which are only ever on the stack on Go 1.18 or later)
func skipAutogeneratedWrappersOut(g *G, thread Thread, startTopframe, startRetframe *Stackframe) (topframe, retframe *Stackframe) {
	topframe, retframe = startTopframe, startRetframe
	if startTopframe.Ret == 0 {
		return
	}
	if !isAutogeneratedOrDeferReturn(startRetframe.Current) {
		return
	}
	retfn := thread.BinInfo().PCToFunc(startTopframe.Ret)
	if retfn == nil {
		return
	}
	if !retfn.cu.isgo {
		return
	}
	var err error
	var frames []Stackframe
	if g == nil {
		frames, err = ThreadStacktrace(thread, maxSkipAutogeneratedWrappers)
	} else {
		frames, err = g.Stacktrace(maxSkipAutogeneratedWrappers, 0)
	}
	if err != nil {
		return
	}
	for i := 1; i < len(frames); i++ {
		frame := frames[i]
		if frame.Current.Fn == nil {
			return
		}
		file, line := frame.Current.Fn.cu.lineInfo.PCToLine(frame.Current.Fn.Entry, frame.Current.Fn.Entry)
		if !isAutogeneratedOrDeferReturn(Location{File: file, Line: line, Fn: frame.Current.Fn}) {
			return &frames[i-1], &frames[i]
		}
	}
	return
}

// setDeferBreakpoint is a helper function used by next and StepOut to set a
// breakpoint on the first deferred function.
func setDeferBreakpoint(p *Target, text []AsmInstruction, topframe Stackframe, sameGCond ast.Expr, stepInto bool) (uint64, error) {
	// Set breakpoint on the most recently deferred function (if any)
	var deferpc uint64
	if topframe.TopmostDefer != nil && topframe.TopmostDefer.DwrapPC != 0 {
		_, _, deferfn := topframe.TopmostDefer.DeferredFunc(p)
		if deferfn != nil {
			var err error
			deferpc, err = FirstPCAfterPrologue(p, deferfn, false)
			if err != nil {
				return 0, err
			}
		}
	}
	if deferpc != 0 && deferpc != topframe.Current.PC {
		bp, err := allowDuplicateBreakpoint(p.SetBreakpoint(deferpc, NextDeferBreakpoint, sameGCond))
		if err != nil {
			return 0, err
		}
		if bp != nil && stepInto {
			// If DeferReturns is set then the breakpoint will also be triggered when
			// called from runtime.deferreturn. We only do this for the step command,
			// not for next or stepout.
			for _, breaklet := range bp.Breaklets {
				if breaklet.Kind == NextDeferBreakpoint {
					breaklet.DeferReturns = FindDeferReturnCalls(text)
					break
				}
			}
		}
	}

	return deferpc, nil
}

// findCallInstrForRet returns the PC address of the CALL instruction
// immediately preceding the instruction at ret.
func findCallInstrForRet(p Process, mem MemoryReadWriter, ret uint64, fn *Function) (uint64, error) {
	text, err := disassemble(mem, nil, p.Breakpoints(), p.BinInfo(), fn.Entry, fn.End, false)
	if err != nil {
		return 0, err
	}
	var prevInstr AsmInstruction
	for _, instr := range text {
		if instr.Loc.PC == ret {
			return prevInstr.Loc.PC, nil
		}
		prevInstr = instr
	}
	return 0, fmt.Errorf("could not find CALL instruction for address %#x in %s", ret, fn.Name)
}

// stepOutReverse sets a breakpoint on the CALL instruction that created the current frame, this is either:
// - the CALL instruction immediately preceding the return address of the
//   current frame
// - the return address of the current frame if the current frame was
//   created by a runtime.deferreturn run
// - the return address of the runtime.gopanic frame if the current frame
//   was created by a panic
// This function is used to implement reversed StepOut
func stepOutReverse(p *Target, topframe, retframe Stackframe, sameGCond ast.Expr) error {
	curthread := p.CurrentThread()
	selg := p.SelectedGoroutine()

	if selg != nil && selg.Thread != nil {
		curthread = selg.Thread
	}

	callerText, err := disassemble(p.Memory(), nil, p.Breakpoints(), p.BinInfo(), retframe.Current.Fn.Entry, retframe.Current.Fn.End, false)
	if err != nil {
		return err
	}
	deferReturns := FindDeferReturnCalls(callerText)

	var frames []Stackframe
	if selg == nil {
		frames, err = ThreadStacktrace(curthread, 3)
	} else {
		frames, err = selg.Stacktrace(3, 0)
	}
	if err != nil {
		return err
	}

	var callpc uint64

	if ok, panicFrame := isPanicCall(frames); ok {
		if len(frames) < panicFrame+2 || frames[panicFrame+1].Current.Fn == nil {
			if panicFrame < len(frames) {
				return &ErrNoSourceForPC{frames[panicFrame].Current.PC}
			} else {
				return &ErrNoSourceForPC{frames[0].Current.PC}
			}
		}
		callpc, err = findCallInstrForRet(p, p.Memory(), frames[panicFrame].Ret, frames[panicFrame+1].Current.Fn)
		if err != nil {
			return err
		}
	} else {
		callpc, err = findCallInstrForRet(p, p.Memory(), topframe.Ret, retframe.Current.Fn)
		if err != nil {
			return err
		}

		// check if the call instruction to this frame is a call to runtime.deferreturn
		if len(frames) > 0 {
			frames[0].Ret = callpc
		}
		if ok, pc := isDeferReturnCall(frames, deferReturns); ok && pc != 0 {
			callpc = pc
		}

	}

	_, err = allowDuplicateBreakpoint(p.SetBreakpoint(callpc, NextBreakpoint, sameGCond))

	return err
}

// onNextGoroutine returns true if this thread is on the goroutine requested by the current 'next' command
func onNextGoroutine(tgt *Target, thread Thread, breakpoints *BreakpointMap) (bool, error) {
	var breaklet *Breaklet
breakletSearch:
	for i := range breakpoints.M {
		for _, blet := range breakpoints.M[i].Breaklets {
			if blet.Kind&steppingMask != 0 && blet.Cond != nil {
				breaklet = blet
				break breakletSearch
			}
		}
	}
	if breaklet == nil {
		return false, nil
	}
	// Internal breakpoint conditions can take multiple different forms:
	// Step into breakpoints:
	//   runtime.curg.goid == X
	// Next or StepOut breakpoints:
	//   runtime.curg.goid == X && runtime.frameoff == Y
	// Breakpoints that can be hit either by stepping on a line in the same
	// function or by returning from the function:
	//   runtime.curg.goid == X && (runtime.frameoff == Y || runtime.frameoff == Z)
	// Here we are only interested in testing the runtime.curg.goid clause.
	w := onNextGoroutineWalker{tgt: tgt, thread: thread}
	ast.Walk(&w, breaklet.Cond)
	return w.ret, w.err
}

type onNextGoroutineWalker struct {
	tgt    *Target
	thread Thread
	ret    bool
	err    error
}

func (w *onNextGoroutineWalker) Visit(n ast.Node) ast.Visitor {
	if binx, isbin := n.(*ast.BinaryExpr); isbin && binx.Op == token.EQL && exprToString(binx.X) == "runtime.curg.goid" {
		w.ret, w.err = evalBreakpointCondition(w.tgt, w.thread, n.(ast.Expr))
		return nil
	}
	return w
}
