/*
*
Go Watch: missing watch mode for the "go" command. Invoked exactly like the
"go" command, but also watches Go files and reruns on changes.
*/
package main

import (
	l "log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/mitranim/gg"
)

var (
	log = l.New(os.Stderr, `[gow] `, 0)
	cwd = gg.Cwd()
)

func main() {
	var main Main
	defer main.Exit()
	defer main.Deinit()
	main.Init()
	main.Run()
}

type Main struct {
	Opt         Opt
	Cmd         Cmd
	Stdio       Stdio
	Watcher     Watcher
	TermState   TermState
	ChanSignals gg.Chan[os.Signal]
	ChanRestart gg.Chan[struct{}]
	ChanKill    gg.Chan[syscall.Signal]
	lastRestart time.Time
}

func (self *Main) Init() {
	self.Opt.Init(os.Args[1:])

	self.ChanRestart.Init()
	self.ChanKill.Init()

	self.Cmd.Init(self)
	self.SigInit()
	self.WatchInit()
	self.TermState.Init(self)
	self.Stdio.Init(self)
	self.lastRestart = time.Now()
}

/*
We MUST call this before exiting because:

	* We modify global OS state: terminal, subprocs.
	* OS will NOT auto-cleanup after us.

Otherwise:

	* Terminal is left in unusable state.
	* Subprocs become orphan daemons.

We MUST call this manually before using `syscall.Kill` or `syscall.Exit` on the
current process. Syscalls terminate the process bypassing Go `defer`.
*/
func (self *Main) Deinit() {
	self.Stdio.Deinit()
	self.TermState.Deinit()
	self.WatchDeinit()
	self.SigDeinit()
	self.Cmd.Deinit()
}

func (self *Main) Run() {
	go self.Stdio.Run()
	go self.SigRun()
	go self.WatchRun()
	self.CmdRun()
}

/*
We override Go's default signal handling to ensure cleanup before exit.
Cleanup is necessary to restore the previous terminal state and kill any
descendant processes.

The set of signals registered here MUST match the set of signals explicitly
handled by this program; see below.
*/
func (self *Main) SigInit() {
	self.ChanSignals.InitCap(1)
	signal.Notify(self.ChanSignals, KILL_SIGS_OS...)
}

func (self *Main) SigDeinit() {
	if self.ChanSignals != nil {
		signal.Stop(self.ChanSignals)
	}
}

func (self *Main) SigRun() {
	for val := range self.ChanSignals {
		// Should work on all Unix systems. At the time of writing,
		// we're not prepared to support other systems.
		sig := val.(syscall.Signal)

		if gg.Has(KILL_SIGS, sig) {
			if self.Opt.Verb {
				log.Println(`received kill signal:`, sig)
			}
			self.Kill(sig)
			continue
		}

		if self.Opt.Verb {
			log.Println(`received unknown signal:`, sig)
		}
	}
}

func (self *Main) WatchInit() {
	wat := new(WatchNotify)
	wat.Init(self)
	self.Watcher = wat
}

func (self *Main) WatchDeinit() {
	if self.Watcher != nil {
		self.Watcher.Deinit()
		self.Watcher = nil
	}
}

func (self *Main) WatchRun() {
	if self.Watcher != nil {
		self.Watcher.Run()
	}
}

func (self *Main) CmdRun() {
	if !self.Opt.Postpone {
		self.Cmd.Restart()
	}

	for {
		select {
		case <-self.ChanRestart:
			self.lastRestart = time.Now()
			self.Opt.TermInter()
			self.Cmd.Restart()

		case sig := <-self.ChanKill:
			self.Terminate(sig)
			return
		}
	}
}

func (self *Main) CmdWait(cmd *exec.Cmd) {
	self.Opt.LogSubErr(cmd.Wait())
	self.Opt.TermSuf()
}

// Must be deferred.
func (self *Main) Exit() {
	err := gg.AnyErrTraced(recover())
	if err != nil {
		self.Opt.LogErr(err)
		os.Exit(1)
	}
	os.Exit(0)
}

func (self *Main) OnFsEvent(event FsEvent) {
	if !self.ShouldRestart(event) {
		return
	}
	if self.Opt.Verb {
		log.Println(`restarting on FS event:`, event)
	}
	self.Restart()
}

func (self *Main) ShouldRestart(event FsEvent) bool {
	return event != nil &&
		!(self.Opt.Lazy && self.Cmd.IsRunning()) &&
		self.Opt.AllowPath(event.Path()) &&
		self.Opt.Debounce.Allow(self.lastRestart)
}

func (self *Main) Restart() { self.ChanRestart.SendZeroOpt() }

func (self *Main) Kill(val syscall.Signal) { self.ChanKill.SendOpt(val) }

func (self *Main) Terminate(sig syscall.Signal) {
	/**
	This should terminate any descendant processes, using their default behavior
	for the given signal. If any misbehaving processes do not terminate on a
	kill signal, this is out of our hands for now. We could use SIGKILL to
	ensure termination, but it's unclear if we should.
	*/
	self.Cmd.Broadcast(sig)

	/**
	This should restore previous terminal state and un-register our custom signal
	handling.
	*/
	self.Deinit()

	/**
	Re-send the signal after un-registering our signal handling. If our process is
	still running by the time the signal is received, the signal will be handled
	by the Go runtime, using the default behavior. Most of the time, this signal
	should not be received because after calling this method, we also return
	from the main function.
	*/
	gg.Nop1(syscall.Kill(os.Getpid(), sig))
}
