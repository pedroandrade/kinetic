package producer

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/kinesis"

	"github.com/rewardStyle/kinetic/errs"
	"github.com/rewardStyle/kinetic/logging"
	"github.com/rewardStyle/kinetic/message"
)

// StreamWriter is an interface that abstracts the differences in API between Kinesis and Firehose.
type StreamWriter interface {
	PutRecords(message []*message.Message) ([]*message.Message, error)
}

// empty is used a as a dummy type for counting semaphore channels.
type empty struct{}

type producerOptions struct {
	batchSize        int
	batchTimeout     time.Duration
	queueDepth       int
	maxRetryAttempts int
	concurrency      int
	Stats 		 StatsCollector
}

// Producer sends records to Kinesis or Firehose.
type Producer struct {
	*producerOptions
	*logging.LogHelper

	writer         StreamWriter
	messages       chan *message.Message
	retries        chan *message.Message
	concurrencySem chan empty
	pipeOfDeath    chan empty
	outstanding    int
	shutdownCond   *sync.Cond
	producerWg     *sync.WaitGroup
	producing      bool
	producingMu    sync.Mutex
}

// NewProducer creates a new producer for writing records to a Kinesis or Firehose stream.
func NewProducer(c *aws.Config, w StreamWriter, fn ...func(*Config)) (*Producer, error) {
	cfg := NewConfig(c)
	for _, f := range fn {
		f(cfg)
	}
	return &Producer{
		producerOptions: cfg.producerOptions,
		LogHelper: &logging.LogHelper{
			LogLevel: cfg.LogLevel,
			Logger: cfg.AwsConfig.Logger,
		},
		writer: w,
		concurrencySem: make(chan empty, cfg.concurrency),
		pipeOfDeath:    make(chan empty),
	}, nil
}

// startConsuming will initialize the producer and set producing to true if there is not already another consume loop
// running.
func (p *Producer) startProducing() bool {
	p.producingMu.Lock()
	defer p.producingMu.Unlock()
	if !p.producing {
		p.producing = true
		p.messages = make(chan *message.Message, p.queueDepth)
		p.retries = make(chan *message.Message, p.queueDepth)
		p.shutdownCond = &sync.Cond{L: &sync.Mutex{}}
		p.producerWg = new(sync.WaitGroup)
		p.outstanding = 0
		return true
	}
	return false
}

// stopProducing handles any cleanup after a producing has stopped.
func (p *Producer) stopProducing() {
	p.producingMu.Lock()
	defer p.producingMu.Unlock()
	if p.messages != nil {
		close(p.messages)
	}
	p.producing = false
}

func (p *Producer) sendBatch(batch []*message.Message) {
	defer func() {
		p.shutdownCond.L.Lock()
		p.outstanding--
		p.shutdownCond.L.Unlock()
	}()

	attempts := 0
	var retries []*message.Message
	var err error
stop:
	for {
		retries, err = p.writer.PutRecords(batch)
		failed := len(retries)
		p.Stats.AddSent(len(batch) - failed)
		p.Stats.AddFailed(failed)
		if err == nil && failed == 0 {
			break stop
		}
		switch err := err.(type) {
		case net.Error:
			if err.Timeout() {
				p.Stats.AddPutRecordsTimeout(1)
				p.LogError("Received net error:", err.Error())
			} else {
				p.LogError("Received unknown net error:", err.Error())
			}
		case awserr.Error:
			switch err.Code() {
			case kinesis.ErrCodeProvisionedThroughputExceededException:
				// FIXME: It is not clear to me whether PutRecords would ever return a
				// ProvisionedThroughputExceeded error.  It seems that it would instead return a valid
				// response in which some or all the records within the response will contain an error
				// code and error message of ProvisionedThroughputExceeded.  The current assumption is
				// that if we receive an ProvisionedThroughputExceeded error, that the entire batch
				// should be retried.  Note we only increment the PutRecord stat, instead of the per-
				// message stat.  Furthermore, we do not increment the FailCount of the messages (as
				// the retry mechanism is different).
				p.Stats.AddPutRecordsProvisionedThroughputExceeded(1)
			default:
				p.LogError("Received AWS error:", err.Error())
			}
		case error:
			switch err {
			case errs.ErrRetryRecords:
				break stop
			default:
				p.LogError("Received error:", err.Error())
			}
		default:
			p.LogError("Received unknown error:", err.Error())
		}
		// NOTE: We may want to go through and increment the FailCount for each of the records and allow the
		// batch to be retried rather than retrying the batch as-is.  With this approach, we can kill the "stop"
		// for loop, and set the entire batch to retries to allow the below code to handle retrying the
		// messages.
		attempts++
		if attempts > p.maxRetryAttempts {
			p.LogError(fmt.Sprintf("Dropping batch after %d failed attempts to deliver to stream", attempts))
			p.Stats.AddDropped(len(batch))
			break stop
		}

		// Apply an exponential back-off before retrying
		time.Sleep(time.Duration(attempts * attempts) * time.Second)
	}
	// This frees up another sendBatch to run to allow drainage of the messages / retry queue.  This should improve
	// throughput as well as prevent a potential deadlock in which all batches are blocked on sending retries to the
	// retries channel, and thus no batches are allowed to drain the retry channel.
	<-p.concurrencySem
	for _, msg := range retries {
		if msg.FailCount < p.maxRetryAttempts {
			msg.FailCount++
			select {
			case p.retries <- msg:
			case <-p.pipeOfDeath:
				return
			}
		} else {
			p.Stats.AddDropped(1)
		}
	}
}

// produce calls the underlying writer's PutRecords implementation to deliver batches of messages to the target stream
// until the producer is stopped.
func (p *Producer) produce() {
	if !p.startProducing() {
		return
	}
	p.producerWg.Add(1)
	go func() {
		defer func() {
			p.stopProducing()
			p.producerWg.Done()
		}()

		for {
			var batch []*message.Message
			timer := time.After(p.batchTimeout)
		batch:
			for len(batch) <= p.batchSize {
				select {
				// Using the select, retry messages will
				// interleave with new messages.  This is
				// preferable to putting the messages at the end
				// of the channel as it minimizes the delay in
				// the delivery of retry messages.
				case msg, ok := <-p.messages:
					if !ok {
						p.messages = nil
					} else {
						batch = append(batch, msg)
					}
				case msg := <-p.retries:
					batch = append(batch, msg)
				case <-timer:
					break batch
				case <-p.pipeOfDeath:
					return
				}
			}
			p.shutdownCond.L.Lock()
			if len(batch) > 0 {
				p.outstanding++
				p.Stats.AddBatchSize(len(batch))
				p.concurrencySem <- empty{}
				go p.sendBatch(batch)
			} else if len(batch) == 0 {
				// We did not get any records -- check if we may be (gracefully) shutting down the
				// producer.  We can exit when:
				//   - The messages channel is nil and no new messages can be enqueued
				//   - There are no outstanding sendBatch goroutines and can therefore not produce any
				//     more messages to retry
				//   - The retry channel is empty
				if p.messages == nil && p.outstanding == 0 && len(p.retries) == 0 {
					close(p.retries)
					p.shutdownCond.Broadcast()
					p.shutdownCond.L.Unlock()
					return
				}
			}
			p.shutdownCond.L.Unlock()
		}
	}()
}

// CloseWithContext shuts down the producer, waiting for all outstanding messages and retries to flush.  Cancellation
// is supported through contexts.
func (p *Producer) CloseWithContext(ctx context.Context) {
	c := make(chan empty, 1)
	close(p.messages)
	go func() {
		p.shutdownCond.L.Lock()
		for p.outstanding != 0 {
			p.shutdownCond.Wait()
		}
		p.shutdownCond.L.Unlock()
		p.producerWg.Wait()
		c <- empty{}
	}()
	select {
	case <-c:
	case <-ctx.Done():
		close(p.pipeOfDeath)
	}
}

// Close shuts down the producer, waiting for all outstanding messages and retries to flush.
func (p *Producer) Close() {
	p.CloseWithContext(context.TODO())
}

// SendWithContext sends a message to the stream.  Cancellation supported through contexts.
func (p *Producer) SendWithContext(ctx context.Context, msg *message.Message) error {
	p.produce()
	select {
	case p.messages <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Send a message to the stream, waiting on the message to be put into the channel.
func (p *Producer) Send(msg *message.Message) error {
	return p.SendWithContext(context.TODO(), msg)
}

// TryToSend will attempt to send a message to the stream if the channel has capacity for a message, or will immediately
// return with an error if the channel is full.
func (p *Producer) TryToSend(msg *message.Message) error {
	select {
	case p.messages <- msg:
		return nil
	default:
		p.Stats.AddDropped(1)
		return errs.ErrDroppedMessage
	}
}