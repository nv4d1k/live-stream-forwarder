/*
Copyright © 2023 Naumov Vadik <nv4d1k@ya.ru>
*/
package cmd

import (
	"fmt"
	"github.com/nv4d1k/live-stream-forwarder/app/http/controllers"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/szuecs/gin-glog"
	"github.com/toorop/gin-logrus"
)

var (
	listenAddress string
	listenPort    int
	proxy         string
	logFile       string
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "lsf",
	Short: "Live Stream Forwarder",
	/*Long: `A longer description that spans multiple lines and likely contains
	examples and usage of using your application. For example:

	Cobra is a CLI library for Go that empowers applications.
	This application is a tool to generate the needed files
	to quickly create a Cobra application.`,*/
	// Uncomment the following line if your bare application
	// has an action associated with it:
	Run: func(cmd *cobra.Command, args []string) {
		if global.LogLevel < 6 {
			gin.SetMode(gin.ReleaseMode)
		}
		corsConfig := cors.Config{
			AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "HEAD"},
			AllowHeaders:     []string{"Origin", "Content-Length", "Content-Type", "Authorization"},
			AllowCredentials: true,
			MaxAge:           12 * time.Hour,
			AllowAllOrigins:  true,
		}

		r := gin.Default()
		r.Use(ginglog.Logger(3 * time.Second))
		r.Use(cors.New(corsConfig))
		r.Use(func(ctx *gin.Context) {
			p := ctx.DefaultQuery("proxy", "")
			if proxy != "" {
				ctx.Set("proxy", proxy)
			}
			if p != "" {
				ctx.Set("proxy", p)
			}
			ctx.Next()
		})
		r.Use(ginlogrus.Logger(global.Log), gin.Recovery())
		r.GET("/:platform/:room", controllers.Forwarder)
		if global.LogLevel >= 6 {
			controllers.Debug(r.Group("/debug"))
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", listenAddress, listenPort))
		if err != nil {
			log.Fatalf("create listener error: %s\n", err.Error())
		}
		fmt.Printf("listening on %s ...\n", ln.Addr().String())
		fmt.Printf("access in player with room id. eg. http://%s/twitch/eslcs\n\n", ln.Addr().String())
		err = http.Serve(ln, r)
		if err != nil {
			log.Fatalf("http serve error: %s\n", err.Error())
		}
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)
	// Here you will define your flags and configuration settings.
	// Cobra supports persistent flags, which, if defined here,
	// will be global for your application.

	// rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.live-stream-forwarder.yaml)")

	// Cobra also supports local flags, which will only run
	// when this action is called directly.
	//rootCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
	rootCmd.PersistentFlags().StringVarP(&listenAddress, "listen-address", "l", "127.0.0.1", "listen address")
	rootCmd.PersistentFlags().IntVarP(&listenPort, "listen-port", "p", 0, "listen port")
	rootCmd.PersistentFlags().StringVar(&proxy, "proxy", "", "proxy url")
	rootCmd.PersistentFlags().StringVar(&logFile, "log-file", "", "logging file")
	rootCmd.PersistentFlags().Uint32Var(&global.LogLevel, "log-level", 3, "log level (0 - 7, 3 = warn , 6 = debug)")
}

func initConfig() {
	global.Log = logrus.New()
	var logWriter io.Writer
	if logFile == "" {
		logWriter = os.Stdout
	} else {
		logFileHandle, err := os.OpenFile(logFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
		if err != nil {
			panic(err.Error())
		}
		logWriter = io.MultiWriter(os.Stdout, logFileHandle)
	}
	global.Log.SetOutput(logWriter)
	global.Log.SetLevel(logrus.Level(global.LogLevel))
}
