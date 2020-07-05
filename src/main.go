package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/google/gops/agent"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	cfgFile = flag.String("config", "../config/runtime.yml", "config file for monitoring")
)

type complete struct{} //emptry struct as a single for channel

func main() {
	// runtime.GOMAXPROCS does not the container's CPU quota in Kubernetes
	// therefore, it requires to be set explicitly
	runtime.GOMAXPROCS(StrToInt(os.Getenv("GOMAXPROCS"), 1))

	// gops debug instrument
	if err := agent.Listen(agent.Options{}); err != nil {
		log.Panicf("gops instrument error %v", err)
	}

	flag.Parse()
	effectiveCfgFile := AssignString(os.Getenv("PULSAR_OPS_MONITOR_CFG"), *cfgFile)
	log.Println("config file ", effectiveCfgFile)
	ReadConfigFile(effectiveCfgFile)

	exit := make(chan *complete)
	cfg := GetConfig()

	SetupAnalytics()

	AnalyticsAppStart(AssignString(cfg.Name, "dev"))
	RunInterval(PulsarTenants, TimeDuration(cfg.PulsarAdminConfig.IntervalSeconds, 120, time.Second))
	RunInterval(StartHeartBeat, TimeDuration(cfg.OpsGenieConfig.IntervalSeconds, 240, time.Second))
	RunInterval(UptimeHeartBeat, 30*time.Second) // fixed 30 seconds for heartbeat
	MonitorSites()
	TopicLatencyTestThread()
	WebSocketTopicLatencyTestThread()
	PushToPrometheusProxyThread()

	if cfg.PrometheusConfig.ExposeMetrics {
		log.Printf("start to listen to http port %s", cfg.PrometheusConfig.Port)
		http.Handle("/metrics", promhttp.Handler())
		http.ListenAndServe(cfg.PrometheusConfig.Port, nil)
	}
	for {
		select {
		case <-exit:
			os.Exit(2)
		}
	}
}
