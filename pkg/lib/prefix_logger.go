package lib

import (
	"fmt"
	"log"
	"os"
)

type PrefixLogger struct {
	debugLogger   *log.Logger
	infoLogger    *log.Logger
	warningLogger *log.Logger
	errorLogger   *log.Logger
	fatalLogger   *log.Logger
}

func (l *PrefixLogger)Debug(format string, v ...interface{}) {
	l.debugLogger.Printf(format, v...)
}

func (l *PrefixLogger)Info(format string, v ...interface{}) {
	l.infoLogger.Printf(format, v...)
}

func (l *PrefixLogger)Warning(format string, v ...interface{}) {
	l.warningLogger.Printf(format, v...)
}

func (l *PrefixLogger)Error(format string, v ...interface{}) {
	l.errorLogger.Printf(format, v...)
}

func (l *PrefixLogger)Fatal(format string, v ...interface{}) {
	l.fatalLogger.Fatalf(format, v...)
}

func NewPrefixLogger(prefix string) *PrefixLogger {
	flags := log.Ldate | log.Ltime | log.Lmicroseconds | log.LUTC | log.Lmsgprefix
	return &PrefixLogger {
		debugLogger: log.New(os.Stdout, fmt.Sprintf("D|%s ", prefix), flags),
		infoLogger: log.New(os.Stdout, fmt.Sprintf("I|%s ", prefix), flags),
		warningLogger: log.New(os.Stdout, fmt.Sprintf("W|%s ", prefix), flags),
		errorLogger: log.New(os.Stdout, fmt.Sprintf("E|%s ", prefix), flags),
		fatalLogger: log.New(os.Stdout, fmt.Sprintf("F|%s ", prefix), flags),
	}
}
