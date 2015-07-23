// Copyright 2015 trivago GmbH
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package producer

import (
	kafka "github.com/shopify/sarama" // "gopkg.in/Shopify/sarama.v1"
	"github.com/trivago/gollum/core"
	"github.com/trivago/gollum/core/log"
	"github.com/trivago/gollum/shared"
	"strings"
	"sync"
	"time"
)

const (
	partRandom     = "random"
	partRoundrobin = "roundrobin"
	partHash       = "hash"
	compressNone   = "none"
	compressGZIP   = "zip"
	compressSnappy = "snappy"
)

// Kafka producer plugin
// Configuration example
//
//   - "producer.Kafka":
//     Enable: true
//     ClientId: "weblog"
//     Partitioner: "Roundrobin"
//     RequiredAcks: 1
//     TimeoutMs: 1500
//     SendRetries: 3
//     Compression: "None"
//     MaxOpenRequests: 5
//     BatchMinCount: 10
//     BatchMaxCount: 1
//     BatchSizeByte: 8192
//     BatchSizeMaxKB: 1024
//     BatchTimeoutSec: 3
//     ServerTimeoutSec: 30
//     SendTimeoutMs: 250
//     ElectRetries: 3
//     ElectTimeoutMs: 250
//     MetadataRefreshMs: 10000
//     Filter: "filter.All"
//     Servers:
//     	- "localhost:9092"
//     Topic:
//       "console" : "console"
//     Stream:
//       - "console"
//
// The kafka producer writes messages to a kafka cluster. This producer is
// backed by the sarama library so most settings relate to that library.
//
// ClientId sets the client id of this producer. By default this is "gollum".
//
// Partitioner sets the distribution algorithm to use. Valid values are:
// "Random","Roundrobin" and "Hash". By default "Hash" is set.
//
// RequiredAcks defines the acknowledgement level required by the broker.
// 0 = No responses required. 1 = wait for the local commit. -1 = wait for
// all replicas to commit. >1 = wait for a specific number of commits.
// By default this is set to 1.
//
// TimeoutMs denotes the maximum time the broker will wait for acks. This
// setting becomes active when RequiredAcks is set to wait for multiple commits.
// By default this is set to 1500.
//
// SendRetries defines how many times to retry sending data before marking a
// server as not reachable. By default this is set to 3.
//
// Compression sets the method of compression to use. Valid values are:
// "None","Zip" and "Snappy". By default "None" is set.
//
// MaxOpenRequests defines the number of simultanious connections are allowed.
// By default this is set to 5.
//
// BatchMinCount sets the minimum number of messages required to trigger a
// flush. By default this is set to 1.
//
// BatchMaxCount defines the maximum number of messages processed per
// request. By default this is set to 0 for "unlimited".
//
// BatchSizeByte sets the mimimum number of bytes to collect before a new flush
// is triggered. By default this is set to 8192.
//
// BatchSizeMaxKB defines the maximum allowed message size. By default this is
// set to 1024.
//
// BatchTimeoutSec sets the minimum time in seconds to pass after wich a new
// flush will be triggered. By default this is set to 3.
//
// MessageBufferCount sets the internal channel size for the kafka client.
// By default this is set to 256.
//
// ServerTimeoutSec defines the time after which a connection is set to timed
// out. By default this is set to 30 seconds.
//
// SendTimeoutMs defines the number of milliseconds to wait for a server to
// resond before triggering a timeout. Defaults to 250.
//
// ElectRetries defines how many times to retry during a leader election.
// By default this is set to 3.
//
// ElectTimeoutMs defines the number of milliseconds to wait for the cluster to
// elect a new leader. Defaults to 250.
//
// MetadataRefreshMs set the interval in seconds for fetching cluster metadata.
// By default this is set to 10000. This corresponds to the JVM setting
// `topic.metadata.refresh.interval.ms`.
//
// Servers contains the list of all kafka servers to connect to.  By default this
// is set to contain only "localhost:9092".
//
// Filter defines a filter function that removes or allows certain messages to
// pass through to kafka. By default this is set to filter.All.
//
// Topic maps a stream to a specific kafka topic. You can define the
// wildcard stream (*) here, too. If defined, all streams that do not have a
// specific mapping will go to this topic (including _GOLLUM_).
// If no topic mappings are set the stream names will be used as topic.
type Kafka struct {
	core.ProducerBase
	Filter    core.Filter
	servers   []string
	topic     map[core.MessageStreamID]string
	clientID  string
	client    kafka.Client
	config    *kafka.Config
	producer  kafka.AsyncProducer
	keepAlive bool
}

func init() {
	shared.RuntimeType.Register(Kafka{})
}

// Configure initializes this producer with values from a plugin config.
func (prod *Kafka) Configure(conf core.PluginConfig) error {
	err := prod.ProducerBase.Configure(conf)
	if err != nil {
		return err
	}

	plugin, err := core.NewPluginWithType(conf.GetString("Filter", "filter.All"), conf)
	if err != nil {
		return err // ### return, plugin load error ###
	}

	prod.Filter = plugin.(core.Filter)
	prod.servers = conf.GetStringArray("Servers", []string{"localhost:9092"})
	prod.topic = conf.GetStreamMap("Topic", "")
	prod.clientID = conf.GetString("ClientId", "gollum")
	prod.keepAlive = true

	prod.config = kafka.NewConfig()
	prod.config.ClientID = conf.GetString("ClientId", "gollum")
	prod.config.ChannelBufferSize = conf.GetInt("MessageBufferCount", 256)

	prod.config.Net.MaxOpenRequests = conf.GetInt("MaxOpenRequests", 5)
	prod.config.Net.DialTimeout = time.Duration(conf.GetInt("ServerTimeoutSec", 30)) * time.Second
	prod.config.Net.ReadTimeout = prod.config.Net.DialTimeout
	prod.config.Net.WriteTimeout = prod.config.Net.DialTimeout

	prod.config.Metadata.Retry.Max = conf.GetInt("ElectRetries", 3)
	prod.config.Metadata.Retry.Backoff = time.Duration(conf.GetInt("ElectTimeoutMs", 250)) * time.Millisecond
	prod.config.Metadata.RefreshFrequency = time.Duration(conf.GetInt("MetadataRefreshMs", 10000)) * time.Millisecond

	prod.config.Producer.MaxMessageBytes = conf.GetInt("BatchSizeMaxKB", 1<<10) << 10
	prod.config.Producer.RequiredAcks = kafka.RequiredAcks(conf.GetInt("RequiredAcks", int(kafka.WaitForLocal)))
	prod.config.Producer.Timeout = time.Duration(conf.GetInt("TimoutMs", 1500)) * time.Millisecond

	prod.config.Producer.Return.Errors = true
	prod.config.Producer.Return.Successes = true

	switch strings.ToLower(conf.GetString("Compression", compressNone)) {
	default:
		fallthrough
	case compressNone:
		prod.config.Producer.Compression = kafka.CompressionNone
	case compressGZIP:
		prod.config.Producer.Compression = kafka.CompressionGZIP
	case compressSnappy:
		prod.config.Producer.Compression = kafka.CompressionSnappy
	}

	switch strings.ToLower(conf.GetString("Partitioner", partRandom)) {
	case partRandom:
		prod.config.Producer.Partitioner = kafka.NewRandomPartitioner
	case partRoundrobin:
		prod.config.Producer.Partitioner = kafka.NewRoundRobinPartitioner
	default:
		fallthrough
	case partHash:
		prod.config.Producer.Partitioner = kafka.NewHashPartitioner
	}

	prod.config.Producer.Flush.Bytes = conf.GetInt("BatchSizeByte", 8192)
	prod.config.Producer.Flush.Messages = conf.GetInt("BatchMinCount", 1)
	prod.config.Producer.Flush.Frequency = time.Duration(conf.GetInt("BatchTimeoutSec", 3)) * time.Second
	prod.config.Producer.Flush.MaxMessages = conf.GetInt("BatchMaxCount", 0)
	prod.config.Producer.Retry.Max = conf.GetInt("SendRetries", 3)
	prod.config.Producer.Retry.Backoff = time.Duration(conf.GetInt("SendTimeoutMs", 100)) * time.Millisecond

	return nil
}

func (prod *Kafka) send(msg core.Message) {
	originalMsg := msg
	msg.Data, msg.StreamID = prod.ProducerBase.Format(msg)
	if !prod.Filter.Accepts(msg) {
		return // ### return, filtered ###
	}

	// Store current client and producer to avoid races
	client := prod.client
	producer := prod.producer

	// Check if connected
	if client == nil || producer == nil {
		prod.Drop(msg)
		return // ### return, not connected ###
	}

	// Send message

	topic, topicMapped := prod.topic[msg.StreamID]
	if !topicMapped {
		// Use wildcard fallback or stream name if not set
		topic, topicMapped = prod.topic[core.WildcardStreamID]
		if !topicMapped {
			topic = core.StreamTypes.GetStreamName(msg.StreamID)
		}
	}

	producer.Input() <- &kafka.ProducerMessage{
		Topic:    topic,
		Value:    kafka.ByteEncoder(msg.Data),
		Metadata: originalMsg,
	}
}

func (prod *Kafka) tryOpenConnection() bool {
	// Reconnect the client first
	if prod.client == nil {
		if client, err := kafka.NewClient(prod.servers, prod.config); err == nil {
			prod.client = client
		} else {
			Log.Error.Print("Kafka client error:", err)
			return false // ### return, connection failed ###
		}
	}

	// Make sure we have a producer up and running
	if prod.producer == nil {
		if producer, err := kafka.NewAsyncProducerFromClient(prod.client); err == nil {
			prod.producer = producer
		} else {
			Log.Error.Print("Kafka producer error:", err)
			prod.client.Close()
			prod.client = nil
			return false // ### return, connection failed ###
		}
	}

	return true
}

func (prod *Kafka) connectionKeepAlive() {
	defer prod.WorkerDone()
	defer prod.closeConnection(true)

	// Will be restarted if ever reaching 0
	for prod.keepAlive {
		if !prod.tryOpenConnection() {
			continue
		}

		select {
		case <-prod.producer.Successes():
		case err := <-prod.producer.Errors():
			Log.Error.Printf("Kafka producer error: %s", err.Error())
			if msg, hasMsg := err.Msg.Metadata.(core.Message); hasMsg {
				prod.Drop(msg)
			}
			prod.closeConnection(false)
		}
	}

	// Wait for the remaining errors / successes. Use a timeout to make sure
	// that delayed returns are not missed.
	retries := 0
	for retries < 3 {
		if prod.producer == nil {
			return // ### return, no producer to listen to ###
		}

		select {
		case <-prod.producer.Successes():
			retries = 0

		case err := <-prod.producer.Errors():
			retries = 0
			Log.Error.Printf("Kafka producer error: %s", err.Error())
			if msg, hasMsg := err.Msg.Metadata.(core.Message); hasMsg {
				prod.Drop(msg)
			}

		default:
			time.Sleep(prod.config.Producer.Timeout / 2)
			retries++
		}
	}
}

func (prod *Kafka) closeConnection(immediate bool) {
	producer := prod.producer
	client := prod.client
	closeFunc := func() {
		if producer != nil {
			producer.Close()
		}
		if client != nil && !client.Closed() {
			client.Close()
		}
	}

	prod.client = nil
	prod.producer = nil

	// Delay close so that active calls on the old producer may finish unless
	// requested to be immediate
	if immediate {
		closeFunc()
	} else {
		time.AfterFunc(prod.config.Producer.Timeout*2, closeFunc)
	}
}

// Close gracefully
func (prod *Kafka) Close() {
	defer prod.WorkerDone()
	prod.CloseGracefully(prod.send)
	prod.keepAlive = false
	// Keep alive job will handle WorkerDone
}

// Produce writes to a buffer that is sent to a given socket.
func (prod *Kafka) Produce(workers *sync.WaitGroup) {
	prod.AddMainWorker(workers)
	go prod.connectionKeepAlive()
	prod.DefaultControlLoop(prod.send)
}
