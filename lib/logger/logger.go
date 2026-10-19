package logger

import (
	"fmt"
	nested "github.com/antonfisher/nested-logrus-formatter"
	log "github.com/sirupsen/logrus"
	"io"
	"os"
)

const (
	DebugMode   = "debug"
	ReleaseMode = "release"
)

type Config struct {
	Path         string
	Level        string
	ReportCaller bool
}

func New(c *Config) *log.Logger {
	l, _, err := Open(c)
	if err != nil {
		panic(err)
	}
	return l
}

func Open(c *Config) (*log.Logger, io.Closer, error) {
	l := log.New()
	l.SetFormatter(&nested.Formatter{
		// HideKeys:        true,
		TimestampFormat: "[2006-01-02 15:04:05]",
		NoColors:        true,
		NoFieldsColors:  true,
		//FieldsOrder:     []string{"name", "age"},
	})

	// 日志文件
	f := c.Path
	var write io.Writer
	var closer io.Closer
	if f != "" {
		fwriter, err := os.OpenFile(f, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
		if err != nil {
			return nil, nil, fmt.Errorf("open log file: %w", err)
		}
		write = io.MultiWriter(fwriter, os.Stdout)
		closer = fwriter
	} else {
		write = os.Stdout
	}

	l.SetOutput(write)

	l.SetReportCaller(c.ReportCaller)

	level, err2 := log.ParseLevel(c.Level)
	if err2 != nil {
		level = log.DebugLevel
	}
	l.SetLevel(level)

	return l, closer, nil
}
