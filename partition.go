package goka

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lovoo/goka/kafka"
	"github.com/lovoo/goka/logger"
	"github.com/lovoo/goka/storage"

	"github.com/Shopify/sarama"
	metrics "github.com/rcrowley/go-metrics"
)

const (
	defaultPartitionChannelSize = 10
	stallPeriod                 = 30 * time.Second
	stalledTimeout              = 2 * time.Minute
)

type partition struct {
	log   logger.Logger
	topic string

	ch      chan kafka.Event
	st      *storageProxy
	proxy   kafkaProxy
	process processCallback

	dying    chan bool
	done     chan bool
	stopFlag int64

	recoveredFlag int32
	hwm           int64
	offset        int64

	recoveredOnce sync.Once

	stats *partitionStats
}

type kafkaProxy interface {
	Add(string, int64)
	Remove(string)
	AddGroup()
	Stop()
}

type processCallback func(msg *message, st storage.Storage, wg *sync.WaitGroup, pstats *partitionStats) (int, error)

func newPartition(log logger.Logger, topic string, cb processCallback, st *storageProxy, proxy kafkaProxy, reg metrics.Registry, channelSize int) *partition {
	return &partition{
		log:   log,
		topic: topic,

		ch:    make(chan kafka.Event, channelSize),
		dying: make(chan bool),
		done:  make(chan bool),

		st:            st,
		recoveredOnce: sync.Once{},
		proxy:         proxy,
		process:       cb,

		stats: newStats(),
	}
}

func (p *partition) start() error {
	defer close(p.done)
	defer p.proxy.Stop()

	if !p.st.Stateless() {
		err := p.st.Open()
		if err != nil {
			return err
		}
		defer p.st.Close()

		if err := p.recover(); err != nil {
			return err
		}
	}

	// if stopped, just return
	if atomic.LoadInt64(&p.stopFlag) == 1 {
		return nil
	}
	return p.run()
}

func (p *partition) startCatchup() error {
	defer close(p.done)
	defer p.proxy.Stop()

	err := p.st.Open()
	if err != nil {
		return err
	}
	defer p.st.Close()

	return p.catchup()
}

func (p *partition) stop() {
	atomic.StoreInt64(&p.stopFlag, 1)
	close(p.dying)
	<-p.done
	close(p.ch)
}

///////////////////////////////////////////////////////////////////////////////
// processing
///////////////////////////////////////////////////////////////////////////////
func newMessage(ev *kafka.Message) *message {
	return &message{
		Topic:     string(ev.Topic),
		Partition: int32(ev.Partition),
		Offset:    int64(ev.Offset),
		Timestamp: ev.Timestamp,
		Data:      ev.Value,
		Key:       string(ev.Key),
	}
}

func (p *partition) run() error {
	var wg sync.WaitGroup
	p.proxy.AddGroup()
	defer wg.Wait()

	for {
		select {
		case ev, isOpen := <-p.ch:
			// channel already closed, ev will be nil
			if !isOpen {
				return nil
			}
			switch ev := ev.(type) {
			case *kafka.Message:
				if ev.Topic == p.topic {
					return fmt.Errorf("received message from group table topic after recovery")
				}

				updates, err := p.process(newMessage(ev), p.st, &wg, p.stats)
				if err != nil {
					return fmt.Errorf("error processing message: %v", err)
				}
				p.offset += int64(updates)
				p.hwm = p.offset + 1

				// metrics
				p.stats.Input.Count[ev.Topic]++
				p.stats.Input.Bytes[ev.Topic] += len(ev.Value)
				if !ev.Timestamp.IsZero() {
					p.stats.Input.Delay[ev.Topic] = time.Since(ev.Timestamp)
				}

			case *kafka.NOP:
				// don't do anything but also don't log.
			case *kafka.EOF:
				if ev.Topic != p.topic {
					return fmt.Errorf("received EOF of topic that is not ours. This should not happend (ours=%s, received=%s)", p.topic, ev.Topic)
				}
			default:
				return fmt.Errorf("load: cannot handle %T = %v", ev, ev)
			}

		case <-p.dying:
			return nil
		}

	}
}

///////////////////////////////////////////////////////////////////////////////
// loading storage
///////////////////////////////////////////////////////////////////////////////

func (p *partition) catchup() error {
	return p.load(true)
}

func (p *partition) recover() error {
	return p.load(false)
}

func (p *partition) recovered() bool {
	return atomic.LoadInt32(&p.recoveredFlag) == 1
}

func (p *partition) load(catchup bool) error {
	// fetch local offset
	local, err := p.st.GetOffset(sarama.OffsetOldest)
	if err != nil {
		return fmt.Errorf("Error reading local offset: %v", err)
	}
	p.proxy.Add(p.topic, local)
	defer p.proxy.Remove(p.topic)

	stallTicker := time.NewTicker(stallPeriod)
	defer stallTicker.Stop()

	var lastMessage time.Time
	for {
		select {
		case ev, isOpen := <-p.ch:

			// channel already closed, ev will be nil
			if !isOpen {
				return nil
			}

			switch ev := ev.(type) {
			case *kafka.BOF:
				p.hwm = ev.Hwm

				if ev.Offset == ev.Hwm {
					// nothing to recover
					if err := p.markRecovered(); err != nil {
						return fmt.Errorf("error setting recovered: %v", err)
					}
				}

			case *kafka.EOF:
				p.offset = ev.Hwm - 1
				p.hwm = ev.Hwm

				if err := p.markRecovered(); err != nil {
					return fmt.Errorf("error setting recovered: %v", err)
				}

				if catchup {
					continue
				}
				return nil

			case *kafka.Message:
				lastMessage = time.Now()
				if ev.Topic != p.topic {
					return fmt.Errorf("load: wrong topic = %s", ev.Topic)
				}
				err := p.storeEvent(ev)
				if err != nil {
					return fmt.Errorf("load: error updating storage: %v", err)
				}
				p.offset = ev.Offset
				if p.offset >= p.hwm-1 {
					if err := p.markRecovered(); err != nil {
						return fmt.Errorf("error setting recovered: %v", err)
					}
				}

				// update metrics
				p.stats.Input.Count[ev.Topic]++
				p.stats.Input.Bytes[ev.Topic] += len(ev.Value)
				if !ev.Timestamp.IsZero() {
					p.stats.Input.Delay[ev.Topic] = time.Since(ev.Timestamp)
				}
				if ev.Offset < p.hwm-1 {
					p.stats.Table.Stalled = false
				}

			case *kafka.NOP:
				// don't do anything

			default:
				return fmt.Errorf("load: cannot handle %T = %v", ev, ev)
			}

		case now := <-stallTicker.C:
			// only set to stalled, if the last message was earlier
			// than the stalled timeout
			if now.Sub(lastMessage) > stalledTimeout {
				p.stats.Table.Stalled = true
			}

		case <-p.dying:
			return nil
		}
	}
}

func (p *partition) storeEvent(msg *kafka.Message) error {
	err := p.st.Update(msg.Key, msg.Value)
	if err != nil {
		return fmt.Errorf("Error from the update callback while recovering from the log: %v", err)
	}
	err = p.st.SetOffset(int64(msg.Offset))
	if err != nil {
		return fmt.Errorf("Error updating offset in local storage while recovering from the log: %v", err)
	}
	return nil
}

// mark storage as recovered
func (p *partition) markRecovered() (err error) {
	p.stats.Table.Recovered = true
	p.recoveredOnce.Do(func() {
		atomic.StoreInt32(&p.recoveredFlag, 1)
		err = p.st.MarkRecovered()
	})
	return
}
