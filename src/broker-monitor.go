package main

import (
	"fmt"
	"log"
	"time"

	"github.com/kafkaesque-io/pulsar-monitor/src/brokers"
	"github.com/kafkaesque-io/pulsar-monitor/src/util"
)

// EvaluateBrokers evaluates and reports all brokers health
func EvaluateBrokers(prefixURL, token string) error {
	name := GetConfig().Name // again this is for in-cluster monitoring only

	cfg := GetConfig().BrokersConfig
	failedBrokers, err := brokers.TestBrokers(prefixURL, token)

	if failedBrokers > 0 {
		errMsg := fmt.Sprintf("cluster %s has %d unhealthy brokers, error message %v", name, failedBrokers, err)
		Alert(errMsg)
		ReportIncident(name, name, "brokers are unhealthy reported by pulsar-monitor", errMsg, &cfg.AlertPolicy)
	} else if err != nil {
		errMsg := fmt.Sprintf("cluster %s Pulsar brokers test failed, error message %v", name, err)
		Alert(errMsg)
	}
	return nil
}

// MonitorBrokers start K8sPulsarClusterMonitor thread
func MonitorBrokers() error {
	token := GetConfig().Token
	if token == "" {
		log.Printf("MonitorBroker exits since no token is specified")
		return nil
	}

	prefixURL := GetConfig().BrokersConfig.InClusterRESTURL
	if prefixURL == "" {
		log.Printf("MonitorBroker exits since no in-cluster REST URL prefix is specified")
		return nil
	}

	interval := util.TimeDuration(GetConfig().BrokersConfig.IntervalSeconds, 60, time.Second)

	go func(restURL, jwt string, loopInterval time.Duration) {
		log.Printf("start all brokers monitoring every %v...", loopInterval)
		ticker := time.NewTicker(loopInterval)
		for {
			select {
			case <-ticker.C:
				if err := EvaluateBrokers(restURL, jwt); err != nil {
					log.Printf("pulsar brokers monitoring failed, error: %v", err)
				}
			}
		}
	}(prefixURL, token, interval)
	return nil
}
