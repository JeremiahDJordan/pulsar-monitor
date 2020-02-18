package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/apache/pulsar-client-go/pulsar"
)

const (
	latencyBudget = 2400 // in Millisecond integer, will convert to time.Duration in evaluation
	failedLatency = 100 * time.Second
)

var (
	clients = make(map[string]pulsar.Client)
)

// MsgResult stores the result of message test
type MsgResult struct {
	InOrderDelivery bool
	Latency         time.Duration
	SentTime        time.Time
}

// PubSubLatency the latency including successful produce and consume of a message
func PubSubLatency(tokenStr, uri, topicName, outputTopic, msgPrefix, expectedSuffix string, payloads [][]byte, maxPayloadSize int) (MsgResult, error) {
	// uri is in the form of pulsar+ssl://useast1.gcp.kafkaesque.io:6651
	client, ok := clients[uri]
	if !ok {

		// Configuration variables pertaining to this consumer
		// RHEL CentOS:
		trustStore := AssignString(GetConfig().PulsarPerfConfig.TrustStore, "/etc/ssl/certs/ca-bundle.crt")
		// Debian Ubuntu:
		// trustStore := '/etc/ssl/certs/ca-certificates.crt'
		// OSX:
		// Export the default certificates to a file, then use that file:
		// security find-certificate -a -p /System/Library/Keychains/SystemCACertificates.keychain > ./ca-certificates.crt
		// trust_certs='./ca-certificates.crt'

		token := pulsar.NewAuthenticationToken(tokenStr)

		var err error
		client, err = pulsar.NewClient(pulsar.ClientOptions{
			URL:                   uri,
			Authentication:        token,
			TLSTrustCertsFilePath: trustStore,
		})

		if err != nil {
			return MsgResult{Latency: failedLatency}, err
		}
		clients[uri] = client
	}

	// it is important to close client after close of producer/consumer
	// defer client.Close()

	// Use the client to instantiate a producer
	producer, err := client.CreateProducer(pulsar.ProducerOptions{
		Topic: topicName,
	})

	if err != nil {
		// we guess something could have gone wrong if producer cannot be created
		client.Close()
		delete(clients, uri)
		return MsgResult{Latency: failedLatency}, err
	}

	defer producer.Close()

	subscriptionName := "latency-measure"
	consumerTopic := AssignString(outputTopic, topicName)
	consumer, err := client.Subscribe(pulsar.ConsumerOptions{
		Topic:                       consumerTopic,
		SubscriptionName:            subscriptionName,
		Type:                        pulsar.Exclusive,
		SubscriptionInitialPosition: pulsar.SubscriptionPositionLatest,
	})

	if err != nil {
		defer client.Close() //must defer to allow producer to be closed first
		delete(clients, uri)
		return MsgResult{Latency: failedLatency}, err
	}
	defer consumer.Close()

	// notify the main thread with the latency to complete the exit
	completeChan := make(chan MsgResult, 1)

	// error report channel
	errorChan := make(chan error, 1)

	// payloadStr := "measure-latency123" + time.Now().Format(time.UnixDate)
	receivedCount := len(payloads)

	// Key is payload in string, value is pointer to a MsgResult
	sentPayloads := make(map[string]*MsgResult, receivedCount)
	// Use mutex instead of sync.Map in favour of performance and simplicity
	//  and because no need to protect map iteration to calculate results
	mapMutex := &sync.Mutex{}

	receiveTimeout := TimeDuration(5+(maxPayloadSize/102400), 5, time.Second)
	go func() {

		lastMessageIndex := -1 // to track the message delivery order
		for receivedCount > 0 {
			cCtx, cancel := context.WithTimeout(context.Background(), receiveTimeout)
			defer cancel()

			msg, err := consumer.Receive(cCtx)
			if err != nil {
				receivedCount = 0 // play safe?
				errorChan <- fmt.Errorf("consumer Receive() error: %v", err)
				break
			}
			receivedTime := time.Now()
			receivedStr := string(msg.Payload())
			currentMsgIndex := GetMessageID(msgPrefix, receivedStr)

			mapMutex.Lock()
			result, ok := sentPayloads[receivedStr]
			mapMutex.Unlock()
			if ok {
				receivedCount--
				result.Latency = receivedTime.Sub(result.SentTime)
				if currentMsgIndex > lastMessageIndex {
					result.InOrderDelivery = true
					lastMessageIndex = currentMsgIndex
				}
			}
			consumer.Ack(msg)
			log.Printf("consumer received message index %d payload size %d\n", currentMsgIndex, len(receivedStr))
		}

		//successful case all message received
		if receivedCount == 0 {
			var total time.Duration
			inOrder := true
			for _, v := range sentPayloads {
				total += v.Latency
				inOrder = inOrder && v.InOrderDelivery
			}

			// receiverLatency <- total / receivedCount
			completeChan <- MsgResult{
				Latency:         time.Duration(int(total/time.Millisecond)/len(payloads)) * time.Millisecond,
				InOrderDelivery: inOrder,
			}
		}

	}()

	for _, payload := range payloads {
		ctx := context.Background()

		// Create a different message to send asynchronously
		asyncMsg := pulsar.ProducerMessage{
			Payload: payload,
		}

		sentTime := time.Now()
		expectedMsg := expectedMessage(string(payload), expectedSuffix)
		mapMutex.Lock()
		sentPayloads[expectedMsg] = &MsgResult{SentTime: sentTime}
		mapMutex.Unlock()
		// Attempt to send message asynchronously and handle the response
		producer.SendAsync(ctx, &asyncMsg, func(messageId pulsar.MessageID, msg *pulsar.ProducerMessage, err error) {
			if err != nil {
				errMsg := fmt.Sprintf("fail to instantiate Pulsar client: %v", err)
				log.Println(errMsg)
				// report error and exit
				errorChan <- errors.New(errMsg)
			}

			log.Println("successfully published ", sentTime)
		})
	}

	select {
	case receiverLatency := <-completeChan:
		return receiverLatency, nil
	case reportedErr := <-errorChan:
		return MsgResult{Latency: failedLatency}, reportedErr
	case <-time.Tick(time.Duration(5*len(payloads)) * time.Second):
		return MsgResult{Latency: failedLatency}, errors.New("latency measure not received after timeout")
	}
}

// MeasureLatency measures pub sub latency of each cluster
// TODO: merge this function with TopicBasedLatencyTest
func MeasureLatency() {
	for _, cluster := range GetConfig().PulsarPerfConfig.TopicCfgs {
		TestTopicLatency(cluster)
	}
}

// getNames in the format for reporting and Prometheus metrics
// Input URL pulsar+ssl://useast1.gcp.kafkaesque.io:6651
func getNames(url string) string {
	name := strings.Split(Trim(url), ":")[1]
	clusterName := strings.Replace(name, "//", "", -1)
	return clusterName
}

// SingleTopicLatencyTestThread tests a message delivery in topic and measure the latency.
func SingleTopicLatencyTestThread() {
	topics := GetConfig().PulsarTopicConfig
	log.Println(topics)

	for _, topic := range topics {
		log.Println(topic.Name)
		go func(t TopicCfg) {
			interval := TimeDuration(t.IntervalSeconds, 60, time.Second)
			TestTopicLatency(t)
			for {
				select {
				case <-time.Tick(interval):
					TestTopicLatency(t)
				}
			}
		}(topic)
	}
}

// TestTopicLatency test generic message delivery in topics and the latency
func TestTopicLatency(topicCfg TopicCfg) {
	token := AssignString(topicCfg.Token, GetConfig().PulsarOpsConfig.MasterToken)
	expectedLatency := TimeDuration(topicCfg.LatencyBudgetMs, latencyBudget, time.Millisecond)
	prefix := "messageid"
	payloads, maxPayloadSize := AllMsgPayloads(prefix, topicCfg.PayloadSizes, topicCfg.NumOfMessages)
	log.Printf("send %d messages to topic %s on cluster %s with latency budget %v, %v, %d\n",
		len(payloads), topicCfg.TopicName, topicCfg.PulsarURL, expectedLatency, topicCfg.PayloadSizes, topicCfg.NumOfMessages)
	result, err := PubSubLatency(token, topicCfg.PulsarURL, topicCfg.TopicName, topicCfg.OutputTopic, prefix, topicCfg.ExpectedMsg, payloads, maxPayloadSize)

	// uri is in the form of pulsar+ssl://useast1.gcp.kafkaesque.io:6651
	clusterName := getNames(topicCfg.PulsarURL)
	testName := AssignString(topicCfg.Name, pubSubSubsystem)
	log.Printf("cluster %s has message latency %v", clusterName, result.Latency)
	if err != nil {
		errMsg := fmt.Sprintf("cluster %s, %s latency test Pulsar error: %v", clusterName, testName, err)
		Alert(errMsg)
		ReportIncident(clusterName, "persisted latency test failure", errMsg, &topicCfg.AlertPolicy)
	} else if !result.InOrderDelivery {
		errMsg := fmt.Sprintf("cluster %s, %s test Pulsar message received out of order", clusterName, testName)
		Alert(errMsg)
	} else if result.Latency > expectedLatency {
		errMsg := fmt.Sprintf("cluster %s, %s test message latency %v over the budget %v",
			clusterName, testName, result.Latency, expectedLatency)
		Alert(errMsg)
		ReportIncident(clusterName, "persisted latency test failure", errMsg, &topicCfg.AlertPolicy)
	} else {
		log.Printf("send %d messages to topic %s on %s test cluster %s succeeded\n",
			len(payloads), topicCfg.TopicName, testName, topicCfg.PulsarURL)
		ClearIncident(clusterName)
	}
	PromLatencySum(GetGaugeType(topicCfg.Name), clusterName, result.Latency)
}

func expectedMessage(payload, expected string) string {
	if strings.HasPrefix(expected, "$") {
		return fmt.Sprintf("%s%s", payload, expected[1:len(expected)])
	}
	return payload
}
