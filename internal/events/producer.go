package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
)

const producerBufferSize = 1024

// asyncProducer owns a Kafka writer and drains a buffered channel in the
// background so callers are never blocked by network I/O.
type asyncProducer struct {
	writer *kafka.Writer
	msgCh  chan kafka.Message
	wg     sync.WaitGroup
	logger *slog.Logger
}

func newAsyncProducer(brokers []string, logger *slog.Logger) *asyncProducer {
	// When the broker's advertised.listeners is set to localhost (a common
	// default), kafka-go receives "localhost:9092" from the metadata response
	// and tries to dial it — from inside the container that resolves to the
	// wrong host. We intercept those dials and redirect them to the canonical
	// bootstrap host so writes always reach the actual broker.
	canonicalHost := ""
	if len(brokers) > 0 {
		if h, _, err := net.SplitHostPort(brokers[0]); err == nil {
			canonicalHost = h
		}
	}

	transport := &kafka.Transport{
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			if canonicalHost != "" {
				host, port, err := net.SplitHostPort(address)
				if err == nil && (host == "localhost" || host == "127.0.0.1" || host == "::1") {
					address = net.JoinHostPort(canonicalHost, port)
					logger.Debug("kafka: redirecting localhost dial",
						"from", host, "to", canonicalHost, "port", port)
				}
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}

	w := &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Balancer:               &kafka.LeastBytes{},
		AllowAutoTopicCreation: true,
		Transport:              transport,
	}
	p := &asyncProducer{
		writer: w,
		msgCh:  make(chan kafka.Message, producerBufferSize),
		logger: logger,
	}
	p.wg.Add(1)
	go p.drain()
	return p
}

// drain runs in a dedicated goroutine and serialises Kafka writes.
func (p *asyncProducer) drain() {
	defer p.wg.Done()
	for msg := range p.msgCh {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := p.writer.WriteMessages(ctx, msg); err != nil {
			p.logger.Error("kafka: failed to publish message",
				"topic", msg.Topic,
				"key", string(msg.Key),
				"error", err,
			)
		}
		cancel()
	}
}

// publish serialises value and enqueues the message for async delivery.
// If the buffer is full the message is dropped and a warning is logged.
func (p *asyncProducer) publish(topic, key string, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		p.logger.Error("kafka: failed to marshal event", "topic", topic, "error", err)
		return
	}
	msg := kafka.Message{
		Topic: topic,
		Key:   []byte(key),
		Value: data,
	}
	select {
	case p.msgCh <- msg:
	default:
		p.logger.Warn("kafka: producer buffer full, dropping event",
			"topic", topic, "key", key)
	}
}

// close drains the channel and shuts down the underlying writer.
func (p *asyncProducer) close() error {
	close(p.msgCh)
	p.wg.Wait()
	return p.writer.Close()
}
