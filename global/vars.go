package global

import "github.com/sirupsen/logrus"

var (
	Version   string
	BuildTime string
	GitCommit string
)

var (
	Log      *logrus.Logger
	LogLevel uint32
)
