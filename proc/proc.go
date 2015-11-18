package proc

import (
	"debug/dwarf"
	"debug/gosym"
	"encoding/binary"
	"fmt"
	"go/constant"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	sys "golang.org/x/sys/unix"

	"github.com/derekparker/delve/dwarf/frame"
	"github.com/derekparker/delve/dwarf/line"
	"github.com/derekparker/delve/dwarf/reader"
)

// Process represents all of the information the debugger
// is holding onto regarding the process we are debugging.
type Process struct {
	Pid     int         // Process Pid
	Process *os.Process // Pointer to process struct for the actual process we are debugging

	// Breakpoint table, holds information on breakpoints.
	// Maps instruction address to Breakpoint struct.
	Breakpoints map[uint64]*Breakpoint

	// List of threads mapped as such: pid -> *Thread
	Threads map[int]*Thread

	// Active thread
	CurrentThread *Thread

	// Goroutine that will be used by default to set breakpoint, eval variables, etc...
	// Normally SelectedGoroutine is CurrentThread.GetG, it will not be only if SwitchGoroutine is called with a goroutine that isn't attached to a thread
	SelectedGoroutine *G

	// Maps package names to package paths, needed to lookup types inside DWARF info
	packageMap map[string]string

	allGCache               []*G
	dwarf                   *dwarf.Data
	goSymTable              *gosym.Table
	frameEntries            frame.FrameDescriptionEntries
	lineInfo                line.DebugLines
	firstStart              bool
	os                      *OSProcessDetails
	arch                    Arch
	breakpointIDCounter     int
	tempBreakpointIDCounter int
	halt                    bool
	exited                  bool
	ptraceChan              chan func()
	ptraceDoneChan          chan interface{}
}

func New(pid int) *Process {
	dbp := &Process{
		Pid:            pid,
		Threads:        make(map[int]*Thread),
		Breakpoints:    make(map[uint64]*Breakpoint),
		firstStart:     true,
		os:             new(OSProcessDetails),
		ptraceChan:     make(chan func()),
		ptraceDoneChan: make(chan interface{}),
	}
	go dbp.handlePtraceFuncs()
	return dbp
}

// ProcessExitedError indicates that the process has exited and contains both
// process id and exit status.
type ProcessExitedError struct {
	Pid    int
	Status int
}

func (pe ProcessExitedError) Error() string {
	return fmt.Sprintf("Process %d has exited with status %d", pe.Pid, pe.Status)
}

// Detach from the process being debugged, optionally killing it.
func (dbp *Process) Detach(kill bool) (err error) {
	if dbp.Running() {
		if err = dbp.Halt(); err != nil {
			return
		}
	}
	if !kill {
		// Clean up any breakpoints we've set.
		for _, bp := range dbp.Breakpoints {
			if bp != nil {
				_, err := dbp.ClearBreakpoint(bp.Addr)
				if err != nil {
					return err
				}
			}
		}
	}
	dbp.execPtraceFunc(func() {
		err = PtraceDetach(dbp.Pid, 0)
		if err != nil {
			return
		}
		if kill {
			err = sys.Kill(dbp.Pid, sys.SIGINT)
		}
	})
	return
}

// Returns whether or not Delve thinks the debugged
// process has exited.
func (dbp *Process) Exited() bool {
	return dbp.exited
}

// Returns whether or not Delve thinks the debugged
// process is currently executing.
func (dbp *Process) Running() bool {
	for _, th := range dbp.Threads {
		if th.running {
			return true
		}
	}
	return false
}

// Finds the executable and then uses it
// to parse the following information:
// * Dwarf .debug_frame section
// * Dwarf .debug_line section
// * Go symbol table.
func (dbp *Process) LoadInformation(path string) error {
	var wg sync.WaitGroup

	exe, err := dbp.findExecutable(path)
	if err != nil {
		return err
	}

	wg.Add(4)
	go dbp.loadProcessInformation(&wg)
	go dbp.parseDebugFrame(exe, &wg)
	go dbp.obtainGoSymbols(exe, &wg)
	go dbp.parseDebugLineInfo(exe, &wg)
	wg.Wait()

	return nil
}

func (dbp *Process) FindFileLocation(fileName string, lineno int) (uint64, error) {
	pc, _, err := dbp.goSymTable.LineToPC(fileName, lineno)
	if err != nil {
		return 0, err
	}
	return pc, nil
}

// Finds address of a function's line
// If firstLine == true is passed FindFunctionLocation will attempt to find the first line of the function
// If lineOffset is passed FindFunctionLocation will return the address of that line
// Pass lineOffset == 0 and firstLine == false if you want the address for the function's entry point
// Note that setting breakpoints at that address will cause surprising behavior:
// https://github.com/derekparker/delve/issues/170
func (dbp *Process) FindFunctionLocation(funcName string, firstLine bool, lineOffset int) (uint64, error) {
	origfn := dbp.goSymTable.LookupFunc(funcName)
	if origfn == nil {
		return 0, fmt.Errorf("Could not find function %s\n", funcName)
	}

	if firstLine {
		filename, lineno, _ := dbp.goSymTable.PCToLine(origfn.Entry)
		if filepath.Ext(filename) != ".go" {
			return origfn.Entry, nil
		}
		for {
			lineno++
			pc, fn, _ := dbp.goSymTable.LineToPC(filename, lineno)
			if fn != nil {
				if fn.Name != funcName {
					if strings.Contains(fn.Name, funcName) {
						continue
					}
					break
				}
				if fn.Name == funcName {
					return pc, nil
				}
			}
		}
		return origfn.Entry, nil
	} else if lineOffset > 0 {
		filename, lineno, _ := dbp.goSymTable.PCToLine(origfn.Entry)
		breakAddr, _, err := dbp.goSymTable.LineToPC(filename, lineno+lineOffset)
		return breakAddr, err
	}

	return origfn.Entry, nil
}

// Sends out a request that the debugged process halt
// execution. Sends SIGSTOP to all threads.
func (dbp *Process) RequestManualStop() error {
	dbp.halt = true
	return dbp.requestManualStop()
}

// Sets a breakpoint at addr, and stores it in the process wide
// break point table. Setting a break point must be thread specific due to
// ptrace actions needing the thread to be in a signal-delivery-stop.
func (dbp *Process) SetBreakpoint(addr uint64) (*Breakpoint, error) {
	return dbp.setBreakpoint(dbp.CurrentThread.Id, addr, false)
}

// Sets a temp breakpoint, for the 'next' command.
func (dbp *Process) SetTempBreakpoint(addr uint64) (*Breakpoint, error) {
	return dbp.setBreakpoint(dbp.CurrentThread.Id, addr, true)
}

// Clears a breakpoint.
func (dbp *Process) ClearBreakpoint(addr uint64) (*Breakpoint, error) {
	bp, ok := dbp.FindBreakpoint(addr)
	if !ok {
		return nil, NoBreakpointError{addr: addr}
	}

	if _, err := bp.Clear(dbp.CurrentThread); err != nil {
		return nil, err
	}

	delete(dbp.Breakpoints, addr)

	return bp, nil
}

// Returns the status of the current main thread context.
func (dbp *Process) Status() *sys.WaitStatus {
	return dbp.CurrentThread.Status
}

// Step over function calls.
func (dbp *Process) Next() (err error) {
	for i := range dbp.Breakpoints {
		if dbp.Breakpoints[i].Temp {
			return fmt.Errorf("next while nexting")
		}
	}

	// Get the goroutine for the current thread. We will
	// use it later in order to ensure we are on the same
	// goroutine.
	g, err := dbp.CurrentThread.GetG()
	if err != nil {
		return err
	}

	// Set breakpoints for any goroutine that is currently
	// blocked trying to read from a channel. This is so that
	// if control flow switches to that goroutine, we end up
	// somewhere useful instead of in runtime code.
	if _, err = dbp.setChanRecvBreakpoints(); err != nil {
		return
	}

	var goroutineExiting bool
	if err = dbp.CurrentThread.setNextBreakpoints(); err != nil {
		switch t := err.(type) {
		case ThreadBlockedError, NoReturnAddr: // Noop
		case GoroutineExitingError:
			goroutineExiting = t.goid == g.Id
		default:
			dbp.clearTempBreakpoints()
			return
		}
	}

	if !goroutineExiting {
		for i := range dbp.Breakpoints {
			if dbp.Breakpoints[i].Temp {
				dbp.Breakpoints[i].Cond = g.Id
			}
		}
	}

	return dbp.Continue()
}

func (dbp *Process) setChanRecvBreakpoints() (int, error) {
	var count int
	allg, err := dbp.GoroutinesInfo()
	if err != nil {
		return 0, err
	}

	for _, g := range allg {
		if g.ChanRecvBlocked() {
			ret, err := g.chanRecvReturnAddr(dbp)
			if err != nil {
				if _, ok := err.(NullAddrError); ok {
					continue
				}
				return 0, err
			}
			if _, err = dbp.SetTempBreakpoint(ret); err != nil {
				if _, ok := err.(BreakpointExistsError); ok {
					// Ignore duplicate breakpoints in case if multiple
					// goroutines wait on the same channel
					continue
				}
				return 0, err
			}
			count++
		}
	}
	return count, nil
}

// Resume process
func (dbp *Process) Continue() error {
	for {
		if err := dbp.continueOnce(); err != nil {
			return err
		}
		// if dbp.CurrentThread.CurrentBreakpoint is nil a manual stop was requested
		exitAnyway := (dbp.CurrentThread.CurrentBreakpoint == nil)
		if err := dbp.runBreakpointConditions(); err != nil {
			return err
		}
		if dbp.CurrentThread.onTriggeredBreakpoint() {
			if dbp.CurrentThread.onTriggeredTempBreakpoint() {
				if err := dbp.clearTempBreakpoints(); err != nil {
					return err
				}
			}
			return nil
		}
		if exitAnyway {
			return nil
		}
	}
}

func (dbp *Process) runBreakpointConditions() error {
	// first thread stopped on a breakpoint with true condition
	var trigth *Thread
	// first thread stopped on a temp breakpoint with true condition
	var tempth *Thread

	for _, th := range dbp.Threads {
		if th.CurrentBreakpoint == nil {
			continue
		}

		th.BreakpointConditionMet = th.CurrentBreakpoint.checkCondition(th)

		if th.onTriggeredBreakpoint() {
			if th.onTriggeredTempBreakpoint() {
				if tempth == nil {
					tempth = th
				}
			} else {
				if trigth == nil {
					trigth = th
				}
			}
		}
	}

	// If a temp breakpoint was encountered make its thread the CurrenThread
	// otherwise ensure that CurrentThread is on a triggered breakpoint if there is one
	cth := dbp.CurrentThread
	var err error
	if tempth != nil {
		if !cth.onTriggeredTempBreakpoint() {
			err = dbp.SwitchThread(tempth.Id)
		}
	} else if trigth != nil {
		if !cth.onTriggeredBreakpoint() {
			err = dbp.SwitchThread(trigth.Id)
		}
	}
	return err
}

// Resume process, does not evaluate breakpoint conditionals
func (dbp *Process) continueOnce() error {
	// all threads stopped over a breakpoint are made to step over it
	for _, thread := range dbp.Threads {
		if thread.CurrentBreakpoint != nil {
			if err := thread.Step(); err != nil {
				return err
			}
			thread.CurrentBreakpoint = nil
		}
	}
	// everything is resumed
	for _, thread := range dbp.Threads {
		if err := thread.resume(); err != nil {
			return dbp.exitGuard(err)
		}
	}
	return dbp.run(func() error {
		thread, err := dbp.trapWait(-1)
		if err != nil {
			return err
		}
		if err := dbp.Halt(); err != nil {
			return dbp.exitGuard(err)
		}
		dbp.SwitchThread(thread.Id)
		if err := dbp.setExtraBreakpoints(); err != nil {
			return err
		}
		loc, err := thread.Location()
		if err != nil {
			return err
		}
		// Check to see if we hit a runtime.breakpoint
		if loc.Fn != nil && loc.Fn.Name == "runtime.breakpoint" {
			// Step twice to get back to user code
			for i := 0; i < 2; i++ {
				if err = thread.Step(); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// Single step, will execute a single instruction.
func (dbp *Process) Step() (err error) {
	fn := func() error {
		for _, th := range dbp.Threads {
			if th.blocked() {
				continue
			}
			if err := th.Step(); err != nil {
				return err
			}
		}
		return nil
	}

	return dbp.run(fn)
}

// Change from current thread to the thread specified by `tid`.
func (dbp *Process) SwitchThread(tid int) error {
	if th, ok := dbp.Threads[tid]; ok {
		dbp.CurrentThread = th
		dbp.SelectedGoroutine, _ = dbp.CurrentThread.GetG()
		return nil
	}
	return fmt.Errorf("thread %d does not exist", tid)
}

// Change from current thread to the thread running the specified goroutine
func (dbp *Process) SwitchGoroutine(gid int) error {
	g, err := dbp.FindGoroutine(gid)
	if err != nil {
		return err
	}
	if g == nil {
		// user specified -1 and SelectedGoroutine is nil
		return nil
	}
	if g.thread != nil {
		return dbp.SwitchThread(g.thread.Id)
	}
	dbp.SelectedGoroutine = g
	return nil
}

// Returns an array of G structures representing the information
// Delve cares about from the internal runtime G structure.
func (dbp *Process) GoroutinesInfo() ([]*G, error) {
	if dbp.allGCache != nil {
		return dbp.allGCache, nil
	}

	var (
		threadg = map[int]*Thread{}
		allg    []*G
		rdr     = dbp.DwarfReader()
	)

	for i := range dbp.Threads {
		if dbp.Threads[i].blocked() {
			continue
		}
		g, _ := dbp.Threads[i].GetG()
		if g != nil {
			threadg[g.Id] = dbp.Threads[i]
		}
	}

	addr, err := rdr.AddrFor("runtime.allglen")
	if err != nil {
		return nil, err
	}
	allglenBytes, err := dbp.CurrentThread.readMemory(uintptr(addr), 8)
	if err != nil {
		return nil, err
	}
	allglen := binary.LittleEndian.Uint64(allglenBytes)

	rdr.Seek(0)
	allgentryaddr, err := rdr.AddrFor("runtime.allgs")
	if err != nil {
		// try old name (pre Go 1.6)
		allgentryaddr, err = rdr.AddrFor("runtime.allg")
		if err != nil {
			return nil, err
		}
	}
	faddr, err := dbp.CurrentThread.readMemory(uintptr(allgentryaddr), dbp.arch.PtrSize())
	allgptr := binary.LittleEndian.Uint64(faddr)

	for i := uint64(0); i < allglen; i++ {
		g, err := parseG(dbp.CurrentThread, allgptr+(i*uint64(dbp.arch.PtrSize())), true)
		if err != nil {
			return nil, err
		}
		if thread, allocated := threadg[g.Id]; allocated {
			loc, err := thread.Location()
			if err != nil {
				return nil, err
			}
			g.thread = thread
			// Prefer actual thread location information.
			g.CurrentLoc = *loc
		}
		if g.Status != Gdead {
			allg = append(allg, g)
		}
	}
	dbp.allGCache = allg
	return allg, nil
}

// Stop all threads.
func (dbp *Process) Halt() (err error) {
	for _, th := range dbp.Threads {
		if err := th.Halt(); err != nil {
			return err
		}
	}
	return nil
}

// Obtains register values from what Delve considers to be the current
// thread of the traced process.
func (dbp *Process) Registers() (Registers, error) {
	return dbp.CurrentThread.Registers()
}

// Returns the PC of the current thread.
func (dbp *Process) PC() (uint64, error) {
	return dbp.CurrentThread.PC()
}

// Returns the PC of the current thread.
func (dbp *Process) CurrentBreakpoint() *Breakpoint {
	return dbp.CurrentThread.CurrentBreakpoint
}

// Returns a reader for the dwarf data
func (dbp *Process) DwarfReader() *reader.Reader {
	return reader.New(dbp.dwarf)
}

// Returns list of source files that comprise the debugged binary.
func (dbp *Process) Sources() map[string]*gosym.Obj {
	return dbp.goSymTable.Files
}

// Returns list of functions present in the debugged program.
func (dbp *Process) Funcs() []gosym.Func {
	return dbp.goSymTable.Funcs
}

// Converts an instruction address to a file/line/function.
func (dbp *Process) PCToLine(pc uint64) (string, int, *gosym.Func) {
	return dbp.goSymTable.PCToLine(pc)
}

// Finds the breakpoint for the given ID.
func (dbp *Process) FindBreakpointByID(id int) (*Breakpoint, bool) {
	for _, bp := range dbp.Breakpoints {
		if bp.ID == id {
			return bp, true
		}
	}
	return nil, false
}

// Finds the breakpoint for the given pc.
func (dbp *Process) FindBreakpoint(pc uint64) (*Breakpoint, bool) {
	// Check to see if address is past the breakpoint, (i.e. breakpoint was hit).
	if bp, ok := dbp.Breakpoints[pc-uint64(dbp.arch.BreakpointSize())]; ok {
		return bp, true
	}
	// Directly use addr to lookup breakpoint.
	if bp, ok := dbp.Breakpoints[pc]; ok {
		return bp, true
	}
	return nil, false
}

// Returns a new Process struct.
func initializeDebugProcess(dbp *Process, path string, attach bool) (*Process, error) {
	if attach {
		var err error
		dbp.execPtraceFunc(func() { err = sys.PtraceAttach(dbp.Pid) })
		if err != nil {
			return nil, err
		}
		_, _, err = dbp.wait(dbp.Pid, 0)
		if err != nil {
			return nil, err
		}
	}

	proc, err := os.FindProcess(dbp.Pid)
	if err != nil {
		return nil, err
	}

	dbp.Process = proc
	err = dbp.LoadInformation(path)
	if err != nil {
		return nil, err
	}

	switch runtime.GOARCH {
	case "amd64":
		dbp.arch = AMD64Arch()
	}

	if err := dbp.updateThreadList(); err != nil {
		return nil, err
	}

	ver, isextld, err := dbp.getGoInformation()
	if err != nil {
		return nil, err
	}

	dbp.arch.SetGStructOffset(ver, isextld)
	// SelectedGoroutine can not be set correctly by the call to updateThreadList
	// because without calling SetGStructOffset we can not read the G struct of CurrentThread
	// but without calling updateThreadList we can not examine memory to determine
	// the offset of g struct inside TLS
	dbp.SelectedGoroutine, _ = dbp.CurrentThread.GetG()

	return dbp, nil
}

func (dbp *Process) clearTempBreakpoints() error {
	for _, bp := range dbp.Breakpoints {
		if !bp.Temp {
			continue
		}
		if _, err := dbp.ClearBreakpoint(bp.Addr); err != nil {
			return err
		}
	}
	for i := range dbp.Threads {
		if dbp.Threads[i].CurrentBreakpoint != nil && dbp.Threads[i].CurrentBreakpoint.Temp {
			dbp.Threads[i].CurrentBreakpoint = nil
		}
	}
	return nil
}

func (dbp *Process) handleBreakpointOnThread(id int) (*Thread, error) {
	thread, ok := dbp.Threads[id]
	if !ok {
		return nil, fmt.Errorf("could not find thread for %d", id)
	}
	// Check to see if we have hit a breakpoint.
	err := thread.SetCurrentBreakpoint()
	if err != nil {
		return nil, err
	}
	if (thread.CurrentBreakpoint != nil) || (dbp.halt) {
		return thread, nil
	}

	pc, err := thread.PC()
	if err != nil {
		return nil, err
	}
	fn := dbp.goSymTable.PCToFunc(pc)
	if fn != nil && fn.Name == "runtime.breakpoint" {
		for i := 0; i < 2; i++ {
			if err := thread.Step(); err != nil {
				return nil, err
			}
		}
		return thread, nil
	}
	return nil, NoBreakpointError{addr: pc}
}

func (dbp *Process) run(fn func() error) error {
	dbp.allGCache = nil
	if dbp.exited {
		return fmt.Errorf("process has already exited")
	}
	for _, th := range dbp.Threads {
		th.CurrentBreakpoint = nil
	}
	if err := fn(); err != nil {
		return err
	}
	return nil
}

func (dbp *Process) handlePtraceFuncs() {
	// We must ensure here that we are running on the same thread during
	// while invoking the ptrace(2) syscall. This is due to the fact that ptrace(2) expects
	// all commands after PTRACE_ATTACH to come from the same thread.
	runtime.LockOSThread()

	for fn := range dbp.ptraceChan {
		fn()
		dbp.ptraceDoneChan <- nil
	}
}

func (dbp *Process) execPtraceFunc(fn func()) {
	dbp.ptraceChan <- fn
	<-dbp.ptraceDoneChan
}

func (dbp *Process) getGoInformation() (ver GoVersion, isextld bool, err error) {
	vv, err := dbp.EvalPackageVariable("runtime.buildVersion")
	if err != nil {
		err = fmt.Errorf("Could not determine version number: %v\n", err)
		return
	}
	if vv.Unreadable != nil {
		err = fmt.Errorf("Unreadable version number: %v\n", vv.Unreadable)
		return
	}

	ver, ok := parseVersionString(constant.StringVal(vv.Value))
	if !ok {
		err = fmt.Errorf("Could not parse version number: %v\n", vv.Value)
		return
	}

	rdr := dbp.DwarfReader()
	rdr.Seek(0)
	for entry, err := rdr.NextCompileUnit(); entry != nil; entry, err = rdr.NextCompileUnit() {
		if err != nil {
			return ver, isextld, err
		}
		if prod, ok := entry.Val(dwarf.AttrProducer).(string); ok && (strings.HasPrefix(prod, "GNU AS")) {
			isextld = true
			break
		}
	}
	return
}

func (dbp *Process) FindGoroutine(gid int) (*G, error) {
	if gid == -1 {
		return dbp.SelectedGoroutine, nil
	}

	gs, err := dbp.GoroutinesInfo()
	if err != nil {
		return nil, err
	}
	for i := range gs {
		if gs[i].Id == gid {
			return gs[i], nil
		}
	}
	return nil, fmt.Errorf("Unknown goroutine %d", gid)
}

func (dbp *Process) ConvertEvalScope(gid, frame int) (*EvalScope, error) {
	g, err := dbp.FindGoroutine(gid)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return dbp.CurrentThread.Scope()
	}

	var out EvalScope

	if g.thread == nil {
		out.Thread = dbp.CurrentThread
	} else {
		out.Thread = g.thread
	}

	locs, err := dbp.GoroutineStacktrace(g, frame)
	if err != nil {
		return nil, err
	}

	if frame >= len(locs) {
		return nil, fmt.Errorf("Frame %d does not exist in goroutine %d", frame, gid)
	}

	out.PC, out.CFA = locs[frame].Current.PC, locs[frame].CFA

	return &out, nil
}

func (dbp *Process) postExit() {
	dbp.exited = true
	close(dbp.ptraceChan)
	close(dbp.ptraceDoneChan)
}
