package log

import (
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"sync"
	"time"
)

const (
	Ldate         = 1 << iota                                 // the date: 2009/0123
	Ltime                                                     // the time: 01:23:23
	Lmicroseconds                                             // microsecond resolution: 01:23:23.123123.  assumes Ltime.
	Llongfile                                                 // full file name and line number: /a/b/c/d.go:23
	Lshortfile                                                // final file name element and line number: d.go:23. overrides Llongfile
	Lmodule                                                   // module name
	Llevel                                                    // level: 0(Debug), 1(Info), 2(Warn), 3(Error), 4(Panic), 5(Fatal)
	LstdFlags     = Ldate | Ltime | Lmicroseconds             // initial values for the standard logger
	Ldefault      = Lmodule | Llevel | Lshortfile | LstdFlags // [prefix][time][level][module][shortfile|longfile]
)

const (
	Lnop = iota
	Ldebug
	Linfo
	Lwarn
	Lerror
	Lpanic
	Lfatal
)

const BUFFER_SIZE = 1000

var levels = []string{
	"",
	"[DEBUG]",
	"[INFO]",
	"[WARN]",
	"[ERROR]",
	"[PANIC]",
	"[FATAL]",
}

var level_flags = []string{
	"",
	"debug",
	"info",
	"warn",
	"error",
	"panic",
	"fatal",
}

// A Logger represents an active logging object that generates lines of
// output to an io.WriterCloser.  Each logging operation makes a single call to
// the Writer's Write method.  A Logger can be used simultaneously from
// multiple goroutines; it guarantees to serialize access to the Writer.
type Logger struct {
	mu                  sync.Mutex // ensures atomic writes; protects the following fields
	prefix              string     // prefix to write at beginning of each line
	flag                int        // properties
	Level               int
	out                 io.WriteCloser // destination for output
	buf                 chan []byte    // for accumulating text to write
	isClosed            chan bool      // for accumulating text channel
	levelStats          [6]int64
	enableCallFuncDepth bool
	callFuncDepth       int
	//hooks
	hooks Hooks
	//for rotate
	rotate       bool
	rotateLogger *RotateLogger
}

// New creates a new Logger.   The out variable sets the
// destination to which log data will be written.
// The prefix appears at the beginning of each generated log line.
// The flag argument defines the logging properties.
func New(out io.WriteCloser, prefix string, flag int) *Logger {
	logger := &Logger{out: out, prefix: prefix, buf: make(chan []byte, BUFFER_SIZE), isClosed: make(chan bool), Level: 1, flag: flag}
	go RealWrite(logger)
	return logger
}

func NewRotate(dir, prefix, suffix string, size int64) (*Logger, error) {
	kbSize := size * 1024 * 1024
	rl, err := NewRotateLogger(dir, prefix, suffix, kbSize)
	if err != nil {
		return nil, err
	}
	return &Logger{out: rl.logf, prefix: prefix, Level: 1, flag: Ldefault, rotate: true, rotateLogger: rl}, nil
}

func (l *Logger) SetHooks(hooks Hooks) {
	l.hooks = hooks
}

func (l *Logger) enableLogDepth(flag bool, depth int) {
	l.enableCallFuncDepth = flag
	l.callFuncDepth = depth
	if l.rotate {
		l.rotateLogger.Logger.enableLogDepth(flag, depth)
	}
}

func (l *Logger) formatHeader2(t time.Time, lvl int, reqId string) string {
	prefix := l.prefix
	var (
		date   string
		clock  string
		reqid  string
		level  string
		source string
	)
	if l.flag&(Ldate|Ltime|Lmicroseconds) != 0 {
		if l.flag&Ldate != 0 {
			year, month, day := t.Date()
			date = fmt.Sprintf("[%d-%02d-%02d ", year, month, day)
		}
		if l.flag&(Ltime|Lmicroseconds) != 0 {
			hour, min, sec := t.Clock()
			t_clock := fmt.Sprintf("%02d:%02d:%02d", hour, min, sec)
			var t_ms string
			if l.flag&Lmicroseconds != 0 {
				t_ms = fmt.Sprintf(".%06d", t.Nanosecond()/1e3)
			}
			clock = fmt.Sprintf("%s%s] ", t_clock, t_ms)
		}
	}
	if reqId != "" {
		reqid = "[" + reqId + "]"
	}
	if l.flag&Llevel != 0 {
		level = levels[lvl]
	}
	if l.enableCallFuncDepth {
		pc, file, lineno, ok := runtime.Caller(l.callFuncDepth)
		if ok {
			_, filename := path.Split(file)
			source = fmt.Sprintf("%s:%s:%d ", runtime.FuncForPC(pc).Name(), filename, lineno)
		}
	}
	header := prefix + date + clock + reqid + level + source
	return header
}

func (l *Logger) RotateMode() bool {
	return l.rotate
}

func (l *Logger) Close() {
	if l.rotate {
		l.rotateLogger.Close()
	}
}

// Output writes the output for a logging event.  The string s contains
// the text to print after the prefix specified by the flags of the
// Logger.  A newline is appended if the last character of s is not
// already a newline.  Calldepth is used to recover the PC and is
// provided for generality, although at the moment on all pre-defined
// paths it will be 2.
func (l *Logger) Output(reqId string, lvl int, calldepth int, s string) error {
	if l.rotate {
		return l.rotateLogger.Output(reqId, lvl, calldepth, s)
	}
	if lvl < l.Level {
		return nil
	}
	now := time.Now() // get this early.
	l.levelStats[lvl]++
	hd := l.formatHeader2(now, lvl, reqId)
	content := hd + s
	if len(s) > 0 && s[len(s)-1] != '\n' {
		content = content + "\n"
	}
	go l.hooks.Fire(level_flags[lvl], []byte(content))
	l.buf <- []byte(content)
	return nil
}

func RealWrite(l *Logger) {
	for {
		select {
		case buf := <-l.buf:
			l.mu.Lock()
			l.out.Write(buf)
			l.mu.Unlock()
		case <-l.isClosed:
			for more := true; more; {
				select {
				case buf := <-l.buf:
					l.mu.Lock()
					l.out.Write(buf)
					l.mu.Unlock()
				default:
					more = false
				}
			}
			l.out.Close()
			return
		}
	}
}

// ==============================================
// Printf calls l.Output to print to the logger.
// Arguments are handled in the manner of fmt.Printf.
func (l *Logger) Printf(format string, v ...interface{}) {
	l.Output("", Linfo, 2, fmt.Sprintf(format, v...))
}

// Print calls l.Output to print to the logger.
// Arguments are handled in the manner of fmt.Print.
func (l *Logger) Print(v ...interface{}) { l.Output("", Linfo, 2, fmt.Sprint(v...)) }

// Println calls l.Output to print to the logger.
// Arguments are handled in the manner of fmt.Println.
func (l *Logger) Println(v ...interface{}) { l.Output("", Linfo, 2, fmt.Sprintln(v...)) }

func (l *Logger) Debugf(format string, v ...interface{}) {
	if Ldebug < l.Level {
		return
	}
	l.Output("", Ldebug, 2, fmt.Sprintf(format, v...))
}

func (l *Logger) Debug(v ...interface{}) {
	if Ldebug < l.Level {
		return
	}
	l.Output("", Ldebug, 2, fmt.Sprintln(v...))
}

func (l *Logger) Infof(format string, v ...interface{}) {
	if Linfo < l.Level {
		return
	}
	l.Output("", Linfo, 2, fmt.Sprintf(format, v...))
}

func (l *Logger) Info(v ...interface{}) {
	if Linfo < l.Level {
		return
	}
	l.Output("", Linfo, 2, fmt.Sprintln(v...))
}

func (l *Logger) Warnf(format string, v ...interface{}) {
	l.Output("", Lwarn, 2, fmt.Sprintf(format, v...))
}

func (l *Logger) Warn(v ...interface{}) { l.Output("", Lwarn, 2, fmt.Sprintln(v...)) }

func (l *Logger) Errorf(format string, v ...interface{}) {
	l.Output("", Lerror, 2, fmt.Sprintf(format, v...))
}

func (l *Logger) Error(v ...interface{}) { l.Output("", Lerror, 2, fmt.Sprintln(v...)) }

func (l *Logger) Fatal(v ...interface{}) {
	l.Output("", Lfatal, 2, fmt.Sprint(v...))
	os.Exit(1)
}

// Fatalf is equivalent to l.Printf() followed by a call to os.Exit(1).
func (l *Logger) Fatalf(format string, v ...interface{}) {
	l.Output("", Lfatal, 2, fmt.Sprintf(format, v...))
	os.Exit(1)
}

// Fatalln is equivalent to l.Println() followed by a call to os.Exit(1).
func (l *Logger) Fatalln(v ...interface{}) {
	l.Output("", Lfatal, 2, fmt.Sprintln(v...))
	os.Exit(1)
}

// Panic is equivalent to l.Print() followed by a call to panic().
func (l *Logger) Panic(v ...interface{}) {
	s := fmt.Sprint(v...)
	l.Output("", Lpanic, 2, s)
	panic(s)
}

// Panicf is equivalent to l.Printf() followed by a call to panic().
func (l *Logger) Panicf(format string, v ...interface{}) {
	s := fmt.Sprintf(format, v...)
	l.Output("", Lpanic, 2, s)
	panic(s)
}

// Panicln is equivalent to l.Println() followed by a call to panic().
func (l *Logger) Panicln(v ...interface{}) {
	s := fmt.Sprintln(v...)
	l.Output("", Lpanic, 2, s)
	panic(s)
}

func (l *Logger) Stack(v ...interface{}) {
	s := fmt.Sprint(v...)
	s += "\n"
	buf := make([]byte, 1024*1024)
	n := runtime.Stack(buf, true)
	s += string(buf[:n])
	s += "\n"
	l.Output("", Lerror, 2, s)
}

func (l *Logger) Stat() (stats []int64) {
	l.mu.Lock()
	v := l.levelStats
	l.mu.Unlock()
	return v[:]
}

// Flags returns the output flags for the logger.
func (l *Logger) Flags() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.flag
}

// SetFlags sets the output flags for the logger.
func (l *Logger) SetFlags(flag int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flag = flag
}

// Prefix returns the output prefix for the logger.
func (l *Logger) Prefix() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.prefix
}

// SetPrefix sets the output prefix for the logger.
func (l *Logger) SetPrefix(prefix string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prefix = prefix
}

// SetOutputLevel sets the output level for the logger.
func (l *Logger) SetOutputLevel(lvl int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Level = lvl
	if l.rotate {
		l.rotateLogger.SetOutputLevel(lvl)
	}
}

func (l *Logger) SetOutputLevelString(lv string) {
	var level int
	switch lv {
	case "debug":
		fallthrough
	case "DEBUG":
		level = Ldebug
	case "info":
		fallthrough
	case "INFO":
		level = Linfo
	case "warn":
		fallthrough
	case "WARN":
		level = Lwarn
	case "error":
		fallthrough
	case "ERROR":
		level = Lerror
	default:
		level = Linfo
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Level = level
	if l.rotate {
		l.rotateLogger.SetOutputLevel(level)
	}
}

// SetDailyRotate sets the daily strategy for log slice
func (l *Logger) SetDailyRotate(daily bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rotate {
		l.rotateLogger.SetDailyRotate(daily)
	}
}

func (l *Logger) SetBackup(backup int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rotate {
		l.rotateLogger.SetBackup(backup)
	}
}
