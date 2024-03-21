package lib

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
)

type Logger struct {
	debugLogger   *log.Logger
	infoLogger    *log.Logger
	warningLogger *log.Logger
	errorLogger   *log.Logger
	fatalLogger   *log.Logger
}

func (l *Logger)Debug(format string, v ...interface{}) {
	l.debugLogger.Printf(format, v...)
}

func (l *Logger)Info(format string, v ...interface{}) {
	l.infoLogger.Printf(format, v...)
}

func (l *Logger)Warning(format string, v ...interface{}) {
	l.warningLogger.Printf(format, v...)
}

func (l *Logger)Error(format string, v ...interface{}) {
	l.errorLogger.Printf(format, v...)
}

func (l *Logger)Fatal(format string, v ...interface{}) {
	l.fatalLogger.Fatalf(format, v...)
}

func NewLogger(prefix string) *Logger {
	flags := log.Ldate | log.Ltime | log.Lmicroseconds | log.LUTC | log.Lmsgprefix
	return &Logger {
		debugLogger: log.New(os.Stdout, fmt.Sprintf("D | %s ", prefix), flags),
		infoLogger: log.New(os.Stdout, fmt.Sprintf("I | %s ", prefix), flags),
		warningLogger: log.New(os.Stdout, fmt.Sprintf("W | %s ", prefix), flags),
		errorLogger: log.New(os.Stdout, fmt.Sprintf("E | %s ", prefix), flags),
		fatalLogger: log.New(os.Stdout, fmt.Sprintf("F | %s ", prefix), flags),
	}
}

func InterceptorLogger(l *Logger) logging.Logger {
	return logging.LoggerFunc(func(_ context.Context, lvl logging.Level, msg string, fields ...any) {
		switch lvl {
		case logging.LevelDebug:
			l.Debug(msg, fields...)
		case logging.LevelInfo:
			l.Info(msg, fields...)
		case logging.LevelWarn:
			l.Warning(msg, fields...)
		case logging.LevelError:
			l.Error(msg, fields...)
		default:
			panic(fmt.Sprintf("unknown level %v", lvl))
		}
	})
}
