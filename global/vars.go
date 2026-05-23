package global

import "github.com/sirupsen/logrus"

func init() {
	// Provide a default logger so packages that call global.Log during their
	// init() phase (e.g. flv.DefaultCache) don't panic with nil dereference
	// when running in test mode where the main binary's initialization hasn't run.
	if Log == nil {
		Log = logrus.New()
	}
}

var (
	Version   string
	BuildTime string
	GitCommit string
)

var (
	Log      *logrus.Logger
	LogLevel uint32
)
