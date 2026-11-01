// Package logger is the global printf-style logger of the gateway.
//
// It keeps the API of the team logger: InitStdoutLogger in main,
// InitTestLogger in tests, Debug, Info and Warning with a format string and
// Error with an error value. Every line is "<LEVEL>: <message>"; the stdout
// logger prefixes it with the UTC date and time.
package logger

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
)

// Level is the minimal severity of the messages that are written.
type Level int

// Levels in the order of severity.
const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarning
	LevelError
)

// ErrUnknownLevel is returned by ParseLevel for a name that is not a level.
var ErrUnknownLevel = errors.New("unknown log level")

// unknownLevelFormat is the text of the ParseLevel error: ErrUnknownLevel and the given name.
const unknownLevelFormat = "%w %q: allowed values are DEBUG, INFO, WARNING, ERROR"

var levelNames = map[Level]string{
	LevelDebug:   "DEBUG",
	LevelInfo:    "INFO",
	LevelWarning: "WARNING",
	LevelError:   "ERROR",
}

var (
	mu       sync.RWMutex
	output   = log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds|log.LUTC)
	minLevel = LevelInfo
)

// String returns the name of the level as it is written in the log.
func (l Level) String() string {
	if name, ok := levelNames[l]; ok {
		return name
	}
	return fmt.Sprintf("LEVEL(%d)", int(l))
}

// ParseLevel converts a level name (DEBUG, INFO, WARNING, ERROR; any case) to a Level.
func ParseLevel(name string) (Level, error) {
	normalized := strings.ToUpper(strings.TrimSpace(name))
	for level, levelName := range levelNames {
		if levelName == normalized {
			return level, nil
		}
	}
	return LevelInfo, fmt.Errorf(unknownLevelFormat, ErrUnknownLevel, name)
}

// InitStdoutLogger writes the log to stdout with the UTC time and sets the INFO level.
func InitStdoutLogger() {
	mu.Lock()
	defer mu.Unlock()

	output = log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds|log.LUTC)
	minLevel = LevelInfo
}

// InitTestLogger writes the log to memory without the time and sets the DEBUG level.
// The returned function gives everything written since the call, one "<LEVEL>: <message>" per line.
func InitTestLogger() func() string {
	buffer := &syncBuffer{}

	mu.Lock()
	defer mu.Unlock()

	output = log.New(buffer, "", 0)
	minLevel = LevelDebug

	return buffer.String
}

// SetLevel sets the minimal level of the messages that are written.
func SetLevel(level Level) {
	mu.Lock()
	defer mu.Unlock()

	minLevel = level
}

// Debug writes a trace message; it is shown only at the DEBUG level.
func Debug(format string, args ...any) {
	write(LevelDebug, format, args...)
}

// Info writes a lifecycle or state change message.
func Info(format string, args ...any) {
	write(LevelInfo, format, args...)
}

// Warning writes a message about ignored, clipped or degraded input or state.
func Warning(format string, args ...any) {
	write(LevelWarning, format, args...)
}

// Error writes an error.
func Error(err error) {
	write(LevelError, "%v", err)
}

func write(level Level, format string, args ...any) {
	mu.RLock()
	defer mu.RUnlock()

	if level < minLevel {
		return
	}
	output.Print(level.String() + ": " + fmt.Sprintf(format, args...))
}

// syncBuffer is a bytes.Buffer safe for concurrent writes and reads.
type syncBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

var _ io.Writer = (*syncBuffer)(nil)

// Write appends p to the buffer.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.Write(p)
}

// String returns the buffer contents.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.String()
}
