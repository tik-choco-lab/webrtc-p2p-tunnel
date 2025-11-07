package logger

import (
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

func Init() {
	once.Do(func() {
		lumberJackLogger := &lumberjack.Logger{
			Filename:   "./logs/app.log",
			MaxSize:    10,
			MaxBackups: 3,
			MaxAge:     28,
			Compress:   true,
		}

		encoderConfig := zap.NewProductionEncoderConfig()
		encoderConfig.TimeKey = "timestamp"
		encoderConfig.EncodeTime = func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
			jst := time.FixedZone("Asia/Tokyo", 9*60*60)
			enc.AppendString(t.In(jst).Format(time.RFC3339))
		}

		core := zapcore.NewCore(
			zapcore.NewJSONEncoder(encoderConfig),
			zapcore.AddSync(lumberJackLogger),
			zap.InfoLevel,
		)

		log = zap.New(core)
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
