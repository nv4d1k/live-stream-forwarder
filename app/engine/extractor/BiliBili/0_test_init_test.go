package BiliBili

import (
	"os"

	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/sirupsen/logrus"
)

func init() {
	global.Log = logrus.New()
	global.Log.SetLevel(logrus.DebugLevel)
	global.Log.SetOutput(os.Stdout)
}
