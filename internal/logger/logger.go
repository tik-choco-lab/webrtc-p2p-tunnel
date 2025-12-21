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
	log       *zap.Logger
	once      sync.Once
	debugMode bool
)

func Init() {
	InitWithDebug(false)
}

func InitWithDebug(debug bool) {
	once.Do(func() {
		debugMode = debug

		lumberJackLogger := &lumberjack.Logger{
			Filename:   "./logs/app.log",
			MaxSize:    10,
			MaxBackups: 3,
			MaxAge:     28,
			Compress:   true,
		}

		logLevel := zap.InfoLevel
		if debug {
			logLevel = zap.DebugLevel
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

		if debug {
			consoleEncoderConfig := zap.NewDevelopmentEncoderConfig()
			consoleEncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
			consoleEncoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout("15:04:05")

			consoleCore := zapcore.NewCore(
				zapcore.NewConsoleEncoder(consoleEncoderConfig),
				zapcore.AddSync(os.Stdout),
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

func Error(message string, fields ...zap.Field) {
	if log != nil {
		log.Error(message, fields...)
	}
}
