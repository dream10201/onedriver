package logger

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// LogLevel represents the severity of a log message
type LogLevel int

// various log levels
const (
	FATAL LogLevel = iota
	ERROR
	WARN
	INFO
	TRACE
)

var currentLevel = INFO
var mutex = &sync.RWMutex{}

// StringToLevel converts a string to a LogLevel in a case-insensitive manner.
func StringToLevel(level string) LogLevel {
	level = strings.ToUpper(level)
	switch level {
	case "FATAL":
		return FATAL
	case "ERROR":
		return ERROR
	case "WARN":
		return WARN
	case "INFO":
		return INFO
	case "TRACE":
		return TRACE
	default:
		Errorf("Unrecognized log level %s, defaulting to TRACE.\n", level)
		return TRACE
	}
}

// SetLogLevel changes the current log level
func SetLogLevel(level LogLevel) {
	mutex.Lock()
	currentLevel = level
	mutex.Unlock()
}

func pad(text string, length int) string {
	strlen := len(text)
	if strlen < length {
		text += strings.Repeat(" ", length-strlen)
	}
	return text
}

// funcName gets the current function name from a pointer
func funcName(ptr uintptr) string {
	fname := runtime.FuncForPC(ptr).Name()
	lastDot := 0
	for i := 0; i < len(fname); i++ {
		if fname[i] == '.' {
			lastDot = i
		}
	}
	if lastDot == 0 {
		return filepath.Base(fname)
	}
	return fname[lastDot+1:] + "()"
}

// goroutineID fetches the current goroutine ID. Used solely for
// debugging which goroutine is doing what in the logs.
// Adapted from https://github.com/golang/net/blob/master/http2/gotrack.go
func goroutineID() uint64 {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	// parse out # in the format "goroutine # "
	buf = bytes.TrimPrefix(buf, []byte("goroutine "))
	buf = buf[:bytes.IndexByte(buf, ' ')]
	id, _ := strconv.ParseUint(string(buf), 10, 64)
	return id
}

// Caller obtains the calling function's file and location at a certain point
// in the stack.
func Caller(level int) string {
	// go runtime witchcraft
	ptr, file, line, ok := runtime.Caller(level)
	var functionName string
	if ok {
		functionName = funcName(ptr)
	} else {
		functionName = "(unknown source)"
	}

	return fmt.Sprintf("%d:%s:%d:%s",
		goroutineID(), filepath.Base(file), line, functionName)
}

// Log a function's output at a various level, ignoring messages below the
// currently configured level.
func logger(level LogLevel, format string, args ...interface{}) {
	mutex.RLock()
	defer mutex.RUnlock()
	if level > currentLevel {
		return
	}

	var prefix string
	switch level {
	case FATAL:
		prefix = "FATAL"
	case ERROR:
		prefix = "ERROR"
	case WARN:
		prefix = "WARN"
	case INFO:
		prefix = "INFO"
	case TRACE:
		prefix = "TRACE"
	}

	preformatted := fmt.Sprintf(format, args...)

	log.Printf("- %s - %s - %s",
		pad(prefix, 5), // log level
		Caller(3),      // goroutine + function being logged
		preformatted)   // actual log message
}

// Fatalf logs and kills the program. Uses printf formatting.
func Fatalf(format string, args ...interface{}) {
	logger(FATAL, format, args...)
	os.Exit(1)
}

// Fatal logs and kills the program
func Fatal(args ...interface{}) {
	logger(FATAL, "%s", fmt.Sprintln(args...))
	os.Exit(1)
}

// Errorf logs at the Error level, but allows formatting
func Errorf(format string, args ...interface{}) {
	logger(ERROR, format, args...)
}

// Error logs at the Error level
func Error(args ...interface{}) {
	logger(ERROR, "%s", fmt.Sprintln(args...))
}

// Warnf logs at the Warn level, but allows formatting
func Warnf(format string, args ...interface{}) {
	logger(WARN, format, args...)
}

// Warn logs at the Warn level
func Warn(args ...interface{}) {
	logger(WARN, "%s", fmt.Sprintln(args...))
}

// Infof logs at the Info level, but allows formatting
func Infof(format string, args ...interface{}) {
	logger(INFO, format, args...)
}

// Info logs at the Info level
func Info(args ...interface{}) {
	logger(INFO, "%s", fmt.Sprintln(args...))
}

// Tracef logs at the Warn level, but allows formatting
func Tracef(format string, args ...interface{}) {
	logger(TRACE, format, args...)
}

// Trace logs at the Trace level
func Trace(args ...interface{}) {
	logger(TRACE, "%s", fmt.Sprintln(args...))
}
