package logger

import (
	"os"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

var (
	log  *zap.Logger
	once sync.Once
)

type Options struct {
	Verbosity int
	UseStderr bool
}

func Init() {
	InitWithOptions(Options{Verbosity: 1})
}

func InitWithDebug(debug bool) {
	v := 1
	if debug {
		v = 2
	}
	InitWithOptions(Options{Verbosity: v})
}

func InitWithOptions(opts Options) {
	once.Do(func() {
		lumberJackLogger := &lumberjack.Logger{
			Filename:   "./logs/app.log",
			MaxSize:    10,
			MaxBackups: 3,
			MaxAge:     28,
			Compress:   true,
		}

		// Verbosity: 0=Error, 1=Info, 2=Debug
		logLevel := zap.ErrorLevel
		if opts.Verbosity >= 2 {
			logLevel = zap.DebugLevel
		} else if opts.Verbosity == 1 {
			logLevel = zap.InfoLevel
		}

		encoderConfig := zap.NewProductionEncoderConfig()
		encoderConfig.TimeKey = "timestamp"
		encoderConfig.EncodeTime = func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
			jst := time.FixedZone("Asia/Tokyo", 9*60*60)
			enc.AppendString(t.In(jst).Format(time.RFC3339))
		}

		fileCore := zapcore.NewCore(
			zapcore.NewJSONEncoder(encoderConfig),
			zapcore.AddSync(lumberJackLogger),
			logLevel,
		)

		consoleEncoderConfig := zap.NewDevelopmentEncoderConfig()
		consoleEncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
		consoleEncoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout("15:04:05")

		consoleOutput := os.Stdout
		if opts.UseStderr {
			consoleOutput = os.Stderr
		}

		if opts.Verbosity >= 2 {
			consoleCore := zapcore.NewCore(
				zapcore.NewConsoleEncoder(consoleEncoderConfig),
				zapcore.AddSync(consoleOutput),
				logLevel,
			)
			log = zap.New(zapcore.NewTee(fileCore, consoleCore))
		} else if opts.UseStderr {
			consoleCore := zapcore.NewCore(
				zapcore.NewConsoleEncoder(consoleEncoderConfig),
				zapcore.AddSync(os.Stderr),
				logLevel,
			)
			log = zap.New(zapcore.NewTee(fileCore, consoleCore))
		} else {
			log = zap.New(fileCore)
		}
	})
}

func Sync() {
	if log != nil {
		_ = log.Sync()
	}
}

func Info(message string, fields ...zap.Field) {
	if log != nil {
		log.Info(message, fields...)
	}
}

func Debug(message string, fields ...zap.Field) {
	if log != nil {
		log.Debug(message, fields...)
	}
}

func Warn(message string, fields ...zap.Field) {
	if log != nil {
		log.Warn(message, fields...)
	}
}

func Error(message string, fields ...zap.Field) {
	if log != nil {
		log.Error(message, fields...)
	}
}
