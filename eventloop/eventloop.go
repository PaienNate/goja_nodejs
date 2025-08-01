// Package eventloop provides a high-performance JavaScript event loop implementation
// with optimized logging to minimize performance impact.
//
// Logging Performance Optimizations:
// - Debug logs are disabled by default and controlled by enableDebugLog flag
// - Stack traces are only included in debug mode to reduce log size
// - Nil checks prevent unnecessary log calls when logger is not configured
// - Error logs are kept concise while maintaining essential information
// - Panic statistics are only logged when panics occur or in debug mode
//
// Usage:
//
//	loop := NewEventLoop(
//	  WithLogger(myLogger),
//	  WithDebugLog(false), // Keep false in production for best performance
//	)
//
// Fork from goga-nodejs
package eventloop

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/console"
	"github.com/dop251/goja_nodejs/require"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/panjf2000/ants/v2"
)

type job struct {
	cancel func() bool
	fn     func()
	idx    int

	cancelled bool
}

type Timer struct {
	job
	timer *time.Timer
}

type Interval struct {
	job
	ticker   *time.Ticker
	stopChan chan struct{}
	stopped  int32 // atomic flag to prevent double close
}

type Immediate struct {
	job
}

type EventLoop struct {
	vm       *goja.Runtime
	jobChan  chan func()
	jobs     []*job
	jobCount int32
	canRun   int32

	auxJobsLock sync.Mutex
	wakeupChan  chan struct{}

	auxJobsSpare, auxJobs []func()

	stopLock   sync.Mutex
	stopCond   *sync.Cond
	running    bool
	terminated bool

	enableConsole bool
	registry      *require.Registry

	// ants goroutine pool
	pool     *ants.Pool
	poolSize int

	// Enhanced panic handling and lifecycle management
	ctx        context.Context
	cancel     context.CancelFunc
	panicCount int64       // atomic counter for panic statistics
	helper     *log.Helper // Kratos log helper for convenient logging

	// Performance optimization flags
	enableDebugLog bool // Control debug level logging for performance
}

func NewEventLoop(opts ...Option) *EventLoop {
	vm := goja.New()

	// Create context with cancel for lifecycle management
	ctx, cancel := context.WithCancel(context.Background())

	loop := &EventLoop{
		vm:             vm,
		jobChan:        make(chan func()),
		wakeupChan:     make(chan struct{}, 1),
		enableConsole:  true,
		poolSize:       100, // default pool size
		ctx:            ctx,
		cancel:         cancel,
		helper:         log.NewHelper(log.With(log.GetLogger(), "component", "EventLoop")), // default Kratos logger
		enableDebugLog: false,                                                              // Default to false for better performance - reduces log overhead
	}
	loop.stopCond = sync.NewCond(&loop.stopLock)

	// Apply options first
	for _, opt := range opts {
		opt(loop)
	}

	// Initialize ants pool with optimized panic handler for better performance
	pool, err := ants.NewPool(loop.poolSize,
		ants.WithPanicHandler(func(p interface{}) {
			// Optimized panic handling - reduced logging overhead for better performance
			atomic.AddInt64(&loop.panicCount, 1)
			loop.helper.Errorf("Panic in ants pool: %v", p)
			// Stack trace only in debug mode to reduce performance impact
			if loop.enableDebugLog {
				loop.helper.Debugf("Stack trace for ants pool panic:\n%s", debug.Stack())
			}
		}),
		ants.WithDisablePurge(true), // Disable background cleanup goroutines for performance
	)
	if err != nil {
		// Fallback: if pool creation fails, set pool to nil
		// The code will handle this gracefully by falling back to direct goroutines
		loop.helper.Errorf("Failed to create ants pool: %v, falling back to direct goroutines", err)
		loop.pool = nil
	} else {
		loop.pool = pool
	}

	if loop.registry == nil {
		loop.registry = new(require.Registry)
	}
	loop.registry.Enable(vm)
	if loop.enableConsole {
		console.Enable(vm)
	}
	vm.Set("setTimeout", loop.setTimeout)
	vm.Set("setInterval", loop.setInterval)
	vm.Set("setImmediate", loop.setImmediate)
	vm.Set("clearTimeout", loop.clearTimeout)
	vm.Set("clearInterval", loop.clearInterval)
	vm.Set("clearImmediate", loop.clearImmediate)

	return loop
}

type Option func(*EventLoop)

// EnableConsole controls whether the "console" module is loaded into
// the runtime used by the loop.  By default, loops are created with
// the "console" module loaded, pass EnableConsole(false) to
// NewEventLoop to disable this behavior.
func EnableConsole(enableConsole bool) Option {
	return func(loop *EventLoop) {
		loop.enableConsole = enableConsole
	}
}

func WithRegistry(registry *require.Registry) Option {
	return func(loop *EventLoop) {
		loop.registry = registry
	}
}

// WithPoolSize sets the goroutine pool size for ants
func WithPoolSize(size int) Option {
	return func(loop *EventLoop) {
		if size > 0 {
			loop.poolSize = size
		}
	}
}

// WithLoggerHelper sets a custom Kratos logger for the event loop
func WithLoggerHelper(logger *log.Helper) Option {
	return func(loop *EventLoop) {
		loop.helper = logger
	}
}

// WithDebugLog enables or disables debug level logging for performance optimization.
// When disabled (default), only error and warning logs are emitted, significantly
// reducing logging overhead in production environments.
// Enable this only for debugging purposes as it may impact performance.
func WithDebugLog(enable bool) Option {
	return func(loop *EventLoop) {
		loop.enableDebugLog = enable
	}
}

// GetPanicCount returns the total number of panics that have occurred
func (loop *EventLoop) GetPanicCount() int64 {
	return atomic.LoadInt64(&loop.panicCount)
}

// reinitializeContext creates new context for restart after termination
func (loop *EventLoop) reinitializeContext() {
	loop.cancel()
	loop.ctx, loop.cancel = context.WithCancel(context.Background())
	loop.terminated = false
	// Only log context reinitialization in debug mode to reduce performance impact
	if loop.helper != nil && loop.enableDebugLog {
		loop.helper.Debugf("Event loop context reinitialized for restart")
	}
}

// safeExecute wraps function execution with panic recovery
func (loop *EventLoop) safeExecute(name string, fn func()) {
	if fn == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			atomic.AddInt64(&loop.panicCount, 1)
			// Only include stack trace for critical errors to reduce log size
			if loop.helper != nil {
				loop.helper.Errorf("Panic in %s: %v", name, r)
				// Stack trace only in debug mode
				if loop.enableDebugLog {
					loop.helper.Debugf("Stack trace for panic in %s:\n%s", name, debug.Stack())
				}
			}
		}
	}()
	fn()
}

func (loop *EventLoop) schedule(call goja.FunctionCall, repeating bool) goja.Value {
	if fn, ok := goja.AssertFunction(call.Argument(0)); ok {
		delay := call.Argument(1).ToInteger()
		var args []goja.Value
		if len(call.Arguments) > 2 {
			args = append(args, call.Arguments[2:]...)
		}
		f := func() {
			_, err := fn(nil, args...)
			if err != nil {
				loop.helper.Errorf("Error in scheduled function: %v", err)
				return
			}
		}
		loop.jobCount++
		var job *job
		var ret goja.Value
		if repeating {
			interval := loop.newInterval(f)
			interval.start(loop, time.Duration(delay)*time.Millisecond)
			job = &interval.job
			ret = loop.vm.ToValue(interval)
		} else {
			timeout := loop.newTimeout(f)
			timeout.start(loop, time.Duration(delay)*time.Millisecond)
			job = &timeout.job
			ret = loop.vm.ToValue(timeout)
		}
		job.idx = len(loop.jobs)
		loop.jobs = append(loop.jobs, job)
		return ret
	}
	return nil
}

func (loop *EventLoop) setTimeout(call goja.FunctionCall) goja.Value {
	return loop.schedule(call, false)
}

func (loop *EventLoop) setInterval(call goja.FunctionCall) goja.Value {
	return loop.schedule(call, true)
}

func (loop *EventLoop) setImmediate(call goja.FunctionCall) goja.Value {
	if fn, ok := goja.AssertFunction(call.Argument(0)); ok {
		var args []goja.Value
		if len(call.Arguments) > 1 {
			args = append(args, call.Arguments[1:]...)
		}
		f := func() {
			_, err := fn(nil, args...)
			if err != nil {
				loop.helper.Error(fmt.Sprintf("JS immediate callback error: %v", err))
				return
			}
		}
		loop.jobCount++
		return loop.vm.ToValue(loop.addImmediate(f))
	}
	return nil
}

// SetTimeout schedules to run the specified function in the context
// of the loop as soon as possible after the specified timeout period.
// SetTimeout returns a Timer which can be passed to ClearTimeout.
// The instance of goja.Runtime that is passed to the function and any Values derived
// from it must not be used outside the function. SetTimeout is
// safe to call inside or outside the loop.
// If the loop is terminated (see Terminate()) returns nil.
func (loop *EventLoop) SetTimeout(fn func(*goja.Runtime), timeout time.Duration) *Timer {
	t := loop.newTimeout(func() { fn(loop.vm) })
	if loop.addAuxJob(func() {
		t.start(loop, timeout)
		loop.jobCount++
		t.idx = len(loop.jobs)
		loop.jobs = append(loop.jobs, &t.job)
	}) {
		return t
	}
	return nil
}

// ClearTimeout cancels a Timer returned by SetTimeout if it has not run yet.
// ClearTimeout is safe to call inside or outside the loop.
func (loop *EventLoop) ClearTimeout(t *Timer) {
	loop.addAuxJob(func() {
		loop.clearTimeout(t)
	})
}

// SetInterval schedules to repeatedly run the specified function in
// the context of the loop as soon as possible after every specified
// timeout period.  SetInterval returns an Interval which can be
// passed to ClearInterval. The instance of goja.Runtime that is passed to the
// function and any Values derived from it must not be used outside
// the function. SetInterval is safe to call inside or outside the
// loop.
// If the loop is terminated (see Terminate()) returns nil.
func (loop *EventLoop) SetInterval(fn func(*goja.Runtime), timeout time.Duration) *Interval {
	i := loop.newInterval(func() { fn(loop.vm) })
	if loop.addAuxJob(func() {
		i.start(loop, timeout)
		loop.jobCount++
		i.idx = len(loop.jobs)
		loop.jobs = append(loop.jobs, &i.job)
	}) {
		return i
	}
	return nil
}

// ClearInterval cancels an Interval returned by SetInterval.
// ClearInterval is safe to call inside or outside the loop.
func (loop *EventLoop) ClearInterval(i *Interval) {
	loop.addAuxJob(func() {
		loop.clearInterval(i)
	})
}

func (loop *EventLoop) setRunning() {
	loop.stopLock.Lock()
	defer loop.stopLock.Unlock()
	if loop.running {
		panic("Loop is already started")
	}

	// Reinitialize context if terminated
	if loop.terminated {
		loop.reinitializeContext()
	}

	atomic.StoreInt32(&loop.canRun, 1)
	loop.running = true
	// Use debug level for routine operations to reduce log noise
	if loop.helper != nil && loop.enableDebugLog {
		loop.helper.Debugf("Event loop started")
	}
}

// Run calls the specified function, starts the event loop and waits until there are no more delayed jobs to run
// after which it stops the loop and returns.
// The instance of goja.Runtime that is passed to the function and any Values derived from it must not be used
// outside the function.
// Do NOT use this function while the loop is already running. Use RunOnLoop() instead.
// If the loop is already started it will panic.
func (loop *EventLoop) Run(fn func(*goja.Runtime)) {
	loop.setRunning()
	fn(loop.vm)
	loop.run(false)
}

// Start the event loop in the background. The loop continues to run until Stop() is called.
// If the loop is already started it will panic.
func (loop *EventLoop) Start() {
	loop.setRunning()
	go loop.run(true)
}

// StartInForeground starts the event loop in the current goroutine. The loop continues to run until Stop() is called.
// If the loop is already started it will panic.
// Use this instead of Start if you want to recover from panics that may occur while calling native Go functions from
// within setInterval and setTimeout callbacks.
func (loop *EventLoop) StartInForeground() {
	loop.setRunning()
	loop.run(true)
}

// Stop the loop that was started with Start(). After this function returns there will be no more jobs executed
// by the loop. It is possible to call Start() or Run() again after this to resume the execution.
// Note, it does not cancel active timeouts (use Terminate() instead if you want this).
// It is not allowed to run Start() (or Run()) and Stop() or Terminate() concurrently.
// Calling Stop() on a non-running loop has no effect.
// It is not allowed to call Stop() from the loop, because it is synchronous and cannot complete until the loop
// is not running any jobs. Use StopNoWait() instead.
// return number of jobs remaining
func (loop *EventLoop) Stop() int {
	loop.stopLock.Lock()
	for loop.running {
		atomic.StoreInt32(&loop.canRun, 0)
		loop.wakeup()
		loop.stopCond.Wait()
	}
	loop.stopLock.Unlock()
	return int(loop.jobCount)
}

// StopNoWait tells the loop to stop and returns immediately. Can be used inside the loop. Calling it on a
// non-running loop has no effect.
func (loop *EventLoop) StopNoWait() {
	loop.stopLock.Lock()
	if loop.running {
		atomic.StoreInt32(&loop.canRun, 0)
		loop.wakeup()
	}
	loop.stopLock.Unlock()
}

// Terminate stops the loop and clears all active timeouts and intervals. After it returns there are no
// active timers or goroutines associated with the loop. Any attempt to submit a task (by using RunOnLoop(),
// SetTimeout() or SetInterval()) will not succeed.
// After being terminated the loop can be restarted again by using Start() or Run().
// This method must not be called concurrently with Stop*(), Start(), or Run().
func (loop *EventLoop) Terminate() {
	if loop.helper != nil && loop.enableDebugLog {
		loop.helper.Debugf("Terminating event loop...")
	}

	// Cancel context to signal all goroutines to stop
	loop.cancel()

	loop.Stop()

	loop.auxJobsLock.Lock()
	loop.terminated = true
	loop.auxJobsLock.Unlock()

	loop.runAux()

	for i := 0; i < len(loop.jobs); i++ {
		job := loop.jobs[i]
		if !job.cancelled {
			job.cancelled = true
			if job.cancel() {
				loop.removeJob(job)
				i--
			}
		}
	}

	for len(loop.jobs) > 0 {
		loop.safeExecute("terminate-cleanup", func() {
			(<-loop.jobChan)()
		})
	}

	// Wait for any background goroutines to complete
	// Note: With unified goroutine management, most tasks are handled by ants pool
	// or short-lived goroutines that don't require explicit waiting

	// Clean up ants pool
	if loop.pool != nil {
		loop.pool.Release()
		loop.pool = nil
	}

	// Only log termination statistics if there were panics or in debug mode
	panicCount := loop.GetPanicCount()
	if loop.helper != nil {
		if panicCount > 0 {
			loop.helper.Warnf("Event loop terminated with %d panics handled", panicCount)
		} else if loop.enableDebugLog {
			loop.helper.Debugf("Event loop terminated successfully")
		}
	}
}

// RunOnLoop schedules to run the specified function in the context of the loop as soon as possible.
// The order of the runs is preserved (i.e. the functions will be called in the same order as calls to RunOnLoop())
// The instance of goja.Runtime that is passed to the function and any Values derived from it must not be used
// outside the function. It is safe to call inside or outside the loop.
// Returns true on success or false if the loop is terminated (see Terminate()).
func (loop *EventLoop) RunOnLoop(fn func(*goja.Runtime)) bool {
	return loop.addAuxJob(func() { fn(loop.vm) })
}

func (loop *EventLoop) runAux() {
	loop.auxJobsLock.Lock()
	jobs := loop.auxJobs
	loop.auxJobs = loop.auxJobsSpare
	loop.auxJobsLock.Unlock()
	for i, job := range jobs {
		loop.safeExecute(fmt.Sprintf("auxJob-%d", i), job)
		jobs[i] = nil
	}
	loop.auxJobsSpare = jobs[:0]
}

func (loop *EventLoop) run(inBackground bool) {
	// Ensure proper cleanup of running state even if panic occurs
	// This prevents deadlock in Stop() and Terminate() methods
	defer func() {
		if r := recover(); r != nil {
			atomic.AddInt64(&loop.panicCount, 1)
			if loop.helper != nil {
				loop.helper.Errorf("Panic in event loop run: %v", r)
				if loop.enableDebugLog {
					loop.helper.Debugf("Stack trace for event loop panic:\n%s", debug.Stack())
				}
			}
		}
		loop.stopLock.Lock()
		loop.running = false
		loop.stopLock.Unlock()
		loop.stopCond.Broadcast()
	}()

	loop.runAux()
	if inBackground {
		loop.jobCount++
	}
LOOP:
	for loop.jobCount > 0 {
		select {
		case jobber := <-loop.jobChan:
			loop.safeExecute("eventloop-job", jobber)
		case <-loop.wakeupChan:
			loop.runAux()
			if atomic.LoadInt32(&loop.canRun) == 0 {
				break LOOP
			}
		case <-loop.ctx.Done():
			// Context cancelled, graceful shutdown
			if loop.helper != nil && loop.enableDebugLog {
				loop.helper.Debugf("Event loop context cancelled, shutting down gracefully")
			}
			break LOOP
		}
	}
	if inBackground {
		loop.jobCount--
	}
}

func (loop *EventLoop) wakeup() {
	select {
	case loop.wakeupChan <- struct{}{}:
	default:
	}
}

// submitTask submits a task to the ants pool for unified concurrency control
func (loop *EventLoop) submitTask(task func()) {
	executeTask := func() {
		loop.safeExecute("task", task)
	}

	if loop.pool != nil {
		// Use ants pool with enhanced error handling
		err := loop.pool.Submit(executeTask)
		if err != nil {
			// Pool is full or closed, use direct goroutine with context cancellation support
			go executeTask()
		}
	} else {
		// Pool not available, use direct goroutine
		go executeTask()
	}
}

func (loop *EventLoop) addAuxJob(fn func()) bool {
	loop.auxJobsLock.Lock()
	if loop.terminated {
		loop.auxJobsLock.Unlock()
		return false
	}
	loop.auxJobs = append(loop.auxJobs, fn)
	loop.auxJobsLock.Unlock()
	loop.wakeup()
	return true
}

func (loop *EventLoop) newTimeout(f func()) *Timer {
	t := &Timer{
		job: job{fn: f},
	}
	t.cancel = t.doCancel

	return t
}

func (t *Timer) start(loop *EventLoop, timeout time.Duration) {
	t.timer = time.AfterFunc(timeout, func() {
		loop.jobChan <- func() {
			loop.doTimeout(t)
		}
	})
}

func (loop *EventLoop) newInterval(f func()) *Interval {
	i := &Interval{
		job:      job{fn: f},
		stopChan: make(chan struct{}),
	}
	i.cancel = i.doCancel

	return i
}

func (i *Interval) start(loop *EventLoop, timeout time.Duration) {
	// https://nodejs.org/api/timers.html#timers_setinterval_callback_delay_args
	if timeout <= 0 {
		timeout = time.Millisecond
	}
	i.ticker = time.NewTicker(timeout)
	loop.submitTask(func() {
		i.run(loop)
	})
}

func (loop *EventLoop) addImmediate(f func()) *Immediate {
	i := &Immediate{
		job: job{fn: f},
	}
	loop.addAuxJob(func() {
		loop.doImmediate(i)
	})
	return i
}

func (loop *EventLoop) doTimeout(t *Timer) {
	loop.removeJob(&t.job)
	if !t.cancelled {
		t.cancelled = true
		loop.jobCount--
		loop.safeExecute("timeout", t.fn)
	}
}

func (loop *EventLoop) doInterval(i *Interval) {
	if !i.cancelled {
		loop.safeExecute("interval", i.fn)
	}
}

func (loop *EventLoop) doImmediate(i *Immediate) {
	if !i.cancelled {
		i.cancelled = true
		loop.jobCount--
		loop.safeExecute("immediate", i.fn)
	}
}

func (loop *EventLoop) clearTimeout(t *Timer) {
	if t != nil && !t.cancelled {
		t.cancelled = true
		loop.jobCount--
		if t.doCancel() {
			loop.removeJob(&t.job)
		}
	}
}

func (loop *EventLoop) clearInterval(i *Interval) {
	if i != nil && !i.cancelled {
		i.cancelled = true
		loop.jobCount--
		i.doCancel()
		loop.removeJob(&i.job)
	}
}

func (loop *EventLoop) removeJob(job *job) {
	idx := job.idx
	if idx < 0 {
		return
	}
	if idx < len(loop.jobs)-1 {
		loop.jobs[idx] = loop.jobs[len(loop.jobs)-1]
		loop.jobs[idx].idx = idx
	}
	loop.jobs[len(loop.jobs)-1] = nil
	loop.jobs = loop.jobs[:len(loop.jobs)-1]
	job.idx = -1
}

func (loop *EventLoop) clearImmediate(i *Immediate) {
	if i != nil && !i.cancelled {
		i.cancelled = true
		loop.jobCount--
	}
}

func (i *Interval) doCancel() bool {
	if atomic.CompareAndSwapInt32(&i.stopped, 0, 1) {
		close(i.stopChan)
	}
	return false
}

func (t *Timer) doCancel() bool {
	return t.timer.Stop()
}

func (i *Interval) run(loop *EventLoop) {
L:
	for {
		select {
		case <-i.stopChan:
			i.ticker.Stop()
			break L
		case <-i.ticker.C:
			select {
			case loop.jobChan <- func() {
				loop.doInterval(i)
			}:
			case <-i.stopChan:
				i.ticker.Stop()
				break L
			}
		}
	}
	// Try to send cleanup job, but don't block if event loop is terminated
	select {
	case loop.jobChan <- func() {
		loop.removeJob(&i.job)
	}:
	default:
		// Event loop is likely terminated, cleanup will be handled by Terminate()
	}
}
