package memory

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/micro/go-micro/broker"
	"github.com/micro/misc/lib/addr"
)

var (
	MAX_GOROUTINES = 1000000
)

type memoryBroker struct {
	opts broker.Options

	addr string
	sync.RWMutex
	connected   bool
	count       int64
	events      chan struct{}
	Subscribers map[string][]*memorySubscriber
}

type memorySubscriber struct {
	id    string
	topic string

	exit    chan bool
	handler broker.Handler
	opts    broker.SubscribeOptions
}

func (m *memoryBroker) Options() broker.Options {
	return m.opts
}

func (m *memoryBroker) Address() string {
	return m.addr
}

func (m *memoryBroker) Connect() error {
	m.Lock()
	defer m.Unlock()

	if m.connected {
		return nil
	}

	// use 127.0.0.1 to avoid scan of all network interfaces
	addr, err := addr.Extract("127.0.0.1")
	if err != nil {
		return err
	}
	i := rand.Intn(20000)
	// set addr with port
	addr = net.JoinHostPort(addr, fmt.Sprintf("%d", 10000+i))

	m.addr = addr
	m.connected = true

	return nil
}

func (m *memoryBroker) Disconnect() error {
	m.Lock()
	defer m.Unlock()

	if !m.connected {
		return nil
	}

	m.connected = false

	return nil
}

func (m *memoryBroker) Init(opts ...broker.Option) error {
	for _, o := range opts {
		o(&m.opts)
	}
	return nil
}

func (m *memoryBroker) Publish(topic string, msg *broker.Message, opts ...broker.PublishOption) error {
	m.RLock()
	if !m.connected {
		m.RUnlock()
		return errors.New("not connected")
	}

	subs, ok := m.Subscribers[topic]
	m.RUnlock()
	if !ok {
		return nil
	}

	for _, sub := range subs {
		m.events <- struct{}{}

		done := make(chan struct{}, 1)
		go func(sub2 *memorySubscriber, done chan struct{}) {
			defer close(done)
			if err := sub2.handler(&memoryEvent{topic: topic, message: msg}); err != nil {
				fmt.Println("Broker publication to subscriber error ", topic, err)
				//if eh := sub.opts.ErrorHandler; eh != nil {
				//	eh(msg, err)
				//}
				return
			}
		}(sub, done)

		select {
		case <-time.After(500 * time.Millisecond):
			fmt.Println("Broker publication to subscriber timed out ", topic)
			break
		case <-done:
			break
		}

		<-m.events
	}

	return nil
}

func (m *memoryBroker) Subscribe(topic string, handler broker.Handler, opts ...broker.SubscribeOption) (broker.Subscriber, error) {
	m.RLock()
	if !m.connected {
		m.RUnlock()
		return nil, errors.New("not connected")
	}
	m.RUnlock()

	var options broker.SubscribeOptions
	for _, o := range opts {
		o(&options)
	}

	sub := &memorySubscriber{
		exit:    make(chan bool, 1),
		id:      uuid.New().String(),
		topic:   topic,
		handler: handler,
		opts:    options,
	}

	m.Lock()
	m.Subscribers[topic] = append(m.Subscribers[topic], sub)
	m.Unlock()

	go func() {
		<-sub.exit
		m.Lock()
		var newSubscribers []*memorySubscriber
		for _, sb := range m.Subscribers[topic] {
			if sb.id == sub.id {
				continue
			}
			newSubscribers = append(newSubscribers, sb)
		}
		m.Subscribers[topic] = newSubscribers
		m.Unlock()
	}()

	return sub, nil
}

func (m *memoryBroker) String() string {
	return "memory"
}

func (m *memorySubscriber) Options() broker.SubscribeOptions {
	return m.opts
}

func (m *memorySubscriber) Topic() string {
	return m.topic
}

func (m *memorySubscriber) Unsubscribe() error {
	m.exit <- true
	return nil
}

type memoryEvent struct {
	topic   string
	err     error
	message *broker.Message
}

func (s *memoryEvent) Topic() string {
	return s.topic
}

func (s *memoryEvent) Message() *broker.Message {
	return s.message
}

func (s *memoryEvent) Ack() error {
	return nil
}

func NewBroker(opts ...broker.Option) broker.Broker {
	options := broker.Options{
		Context: context.Background(),
	}

	rand.Seed(time.Now().UnixNano())
	for _, o := range opts {
		o(&options)
	}

	return &memoryBroker{
		opts:        options,
		events:      make(chan struct{}, MAX_GOROUTINES),
		Subscribers: make(map[string][]*memorySubscriber),
	}
}
