package rabbit

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

type RabbitMQConfig struct {
	URL        string
	Queue      string
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
}

type RabbitProducer struct {
	config      RabbitMQConfig
	conn        *amqp.Connection
	channel     *amqp.Channel
	isConnected bool
}

func NewMessageSender(config RabbitMQConfig) (*RabbitProducer, error) {
	sender := &RabbitProducer{
		config: config,
	}

	if err := sender.connect(); err != nil {
		return nil, fmt.Errorf("failed to create RabbitMQ connection: %v", err)
	}

	return sender, nil
}

func (s *RabbitProducer) connect() error {
	if s.isConnected {
		return nil
	}

	conn, err := amqp.Dial(s.config.URL)
	if err != nil {
		return fmt.Errorf("failed to connect to RabbitMQ: %v", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return fmt.Errorf("failed to open channel: %v", err)
	}

	s.conn = conn
	s.channel = ch
	s.isConnected = true

	return nil
}

func (s *RabbitProducer) reconnect() error {
	if s.conn != nil {
		s.conn.Close()
	}
	if s.channel != nil {
		s.channel.Close()
	}
	s.isConnected = false

	return s.connect()
}

func (s *RabbitProducer) isConnectionClosed() bool {
	if s.conn == nil || s.channel == nil {
		return true
	}

	select {
	case <-s.conn.NotifyClose(make(chan *amqp.Error)):
		return true
	case <-s.channel.NotifyClose(make(chan *amqp.Error)):
		return true
	default:
		return false
	}
}

func (s *RabbitProducer) calculateExponentialBackoff(attempt int) time.Duration {
	if attempt == 0 {
		return 0
	}

	// Экспоненциальная задержка: base * 2^attempt
	delay := min(s.config.BaseDelay * time.Duration(1<<uint(attempt-1)), s.config.MaxDelay)

	jitter := time.Duration(rand.Int63n(int64(delay) / 5)) // ±20%
	if rand.Intn(2) == 0 {
		delay += jitter
	} else {
		delay -= jitter
	}

	return delay
}

func (s *RabbitProducer) SendBytesWithRetry(ctx context.Context, data []byte) error {
	var lastErr error

	for attempt := 0; attempt <= s.config.MaxRetries; attempt++ {
		if attempt > 0 {
			backoff := s.calculateExponentialBackoff(attempt)

			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return fmt.Errorf("cancelled after %d attempts: %v", attempt-1, ctx.Err())
			}
		}

		if s.isConnectionClosed() {
			if err := s.reconnect(); err != nil {
				lastErr = fmt.Errorf("reconnection failed: %v", err)
				continue
			}
		}

		err := s.sendBytes(ctx, data)
		if err == nil {
			return nil
		}

		lastErr = err

		select {
		case <-ctx.Done():
			return fmt.Errorf("cancelled after %d attempts: %v", attempt, ctx.Err())
		default:
		}
	}

	return fmt.Errorf("failed after %d attempts: %v", s.config.MaxRetries, lastErr)
}

func (s *RabbitProducer) sendBytes(ctx context.Context, data []byte) error {
	if !s.isConnected || s.channel == nil {
		return fmt.Errorf("RabbitMQ channel is not initialized")
	}

	publishing := amqp.Publishing{
		ContentType: "application/json",
		Body:        data,
		Timestamp:   time.Now(),
	}

	err := s.channel.PublishWithContext(
		ctx,
		"",             // exchange
		s.config.Queue, // routing key (имя очереди)
		false,          // mandatory
		false,          // immediate
		publishing,
	)
	if err != nil {
		s.isConnected = false 
		return fmt.Errorf("failed to publish to queue: %v", err)
	}

	return nil
}

func (s *RabbitProducer) Close() error {
	if s.channel != nil {
		s.channel.Close()
	}
	if s.conn != nil {
		return s.conn.Close()
	}
	return nil
}
