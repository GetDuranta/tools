package cmds

import (
	"fmt"
	"log/slog"
	"os"
)

// Log represents the logger
type Log struct {
	verbose bool
}

// Printf prints out formatted string into a log
func (l *Log) Printf(format string, v ...interface{}) {
	slog.Info(fmt.Sprintf(format, v...))
}

// Println prints out args into a log
func (l *Log) Println(args ...interface{}) {
	slog.Info(fmt.Sprintln(args...))
}

// Verbose shows if verbose print enabled
func (l *Log) Verbose() bool {
	return l.verbose
}

func (l *Log) fatal(args ...interface{}) {
	l.Println(args...)
	os.Exit(1)
}

func (l *Log) fatalErr(err error) {
	l.fatal("error:", err)
}
