package listener

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/kinesis"
	"github.com/aws/aws-sdk-go/service/kinesis/kinesisiface"

	"github.com/rewardStyle/kinetic/errs"
	"github.com/rewardStyle/kinetic/logging"
	"github.com/rewardStyle/kinetic/message"
)

// kinesisReaderOptions is used to hold all of the configurable settings of a KinesisReader.
type kinesisReaderOptions struct {
	batchSize     int
	shardIterator *ShardIterator
	readTimeout   time.Duration
	Stats         StatsCollector
}

// KinesisReader handles the API to read records from Kinesis.
type KinesisReader struct {
	*kinesisReaderOptions
	*logging.LogHelper

	stream            string
	shard             string
	throttleSem       chan empty
	nextShardIterator string
	client            kinesisiface.KinesisAPI
}

// NewKinesisReader creates a new KinesisReader object which implements the StreamReader interface to read records from
// Kinesis.
func NewKinesisReader(c *aws.Config, stream string, shard string, fn ...func(*KinesisReaderConfig)) (*KinesisReader, error) {
	cfg := NewKinesisReaderConfig(c)
	for _, f := range fn {
		f(cfg)
	}
	sess, err := cfg.GetSession()
	if err != nil {
		return nil, err
	}
	return &KinesisReader{
		kinesisReaderOptions: cfg.kinesisReaderOptions,
		LogHelper: &logging.LogHelper{
			LogLevel: cfg.LogLevel,
			Logger:  cfg.AwsConfig.Logger,
		},
		stream: stream,
		shard: shard,
		throttleSem: make(chan empty, 5),
		client: kinesis.New(sess),
	}, nil
}

// ensureShardIterator will lazily make sure that we have a valid ShardIterator, calling the GetShardIterator API with
// the configured ShardIteratorType (with any applicable StartingSequenceNumber or Timestamp) if necessary.
//
// Not thread-safe.  Only called from getRecords Care must be taken to ensure that only one call to Listen and
// Retrieve/RetrieveFn can be running at a time.
func (r *KinesisReader) ensureShardIterator() error {
	if r.nextShardIterator != "" {
		return nil
	}

	resp, err := r.client.GetShardIterator(&kinesis.GetShardIteratorInput{
		ShardId:                aws.String(r.shard),                           // Required
		ShardIteratorType:      aws.String(r.shardIterator.shardIteratorType), // Required
		StreamName:             aws.String(r.stream),                          // Required
		StartingSequenceNumber: r.shardIterator.getStartingSequenceNumber(),
		Timestamp:              r.shardIterator.getTimestamp(),
	})
	if err != nil {
		r.LogError(err)
		return err
	}
	if resp == nil {
		return errs.ErrNilGetShardIteratorResponse
	}
	if resp.ShardIterator == nil {
		return errs.ErrNilShardIterator
	}
	return r.setNextShardIterator(*resp.ShardIterator)
}

// setNextShardIterator sets the nextShardIterator to use when calling GetRecords.
//
// Not thread-safe.  Only called from getRecords (and ensureShardIterator, which is called from getRecords).  Care must
// be taken to ensure that only one call to Listen and Retrieve/RetrieveFn can be running at a time.
func (r *KinesisReader) setNextShardIterator(shardIterator string) error {
	if len(shardIterator) == 0 {
		return errs.ErrEmptyShardIterator
	}
	r.nextShardIterator = shardIterator
	return nil
}

// setSequenceNumber sets the sequenceNumber of shardIterator to the last delivered message and updates the
// shardIteratorType to AT_SEQUENCE_NUMBER. This is only used when we need to call getShardIterator (say, to refresh the
// shard iterator).
//
// Not thread-safe.  Only called from getRecords.  Care must be taken to ensure
// that only one call to Listen and Retrieve/RetrieveFn can be running at a
// time.
func (r *KinesisReader) setSequenceNumber(sequenceNumber string) error {
	if len(sequenceNumber) == 0 {
		return errs.ErrEmptySequenceNumber
	}
	r.shardIterator.AtSequenceNumber(sequenceNumber)
	return nil
}

// Kinesis allows five read ops per second per shard.
// http://docs.aws.amazon.com/kinesis/latest/dev/service-sizes-and-limits.html
func (r *KinesisReader) throttle(sem chan empty) {
	sem <- empty{}
	time.AfterFunc(1*time.Second, func() {
		<-sem
	})
}

// FIXME: Need to investigate that the timeout implementation doesn't result in an fd leak.  Since we call Read on the
// HTTPResonse.Body in a select with a timeout channel, we do prevent ourself from blocking.  Once we timeout, we return
// an error to the outer ioutil.ReadAll, which should result in a call to our io.ReadCloser's Close function.  This will
// in turn call Close on the underlying HTTPResponse.Body.  The question is whether this actually shuts down the TCP
// connection.  Worst case scenario is that our client Timeout eventually fires and closes the socket, but this can be
// susceptible to FD exhaustion.
func (r *KinesisReader) getRecords(ctx context.Context, fn MessageFn, batchSize int) (int, error) {
	if err := r.ensureShardIterator(); err != nil {
		return 0, err
	}

	r.throttle(r.throttleSem)

	// We use the GetRecordsRequest method of creating requests to allow for registering custom handlers for better
	// control over the API request.
	var startReadTime time.Time
	var startUnmarshalTime time.Time
	start := time.Now()

	req, resp := r.client.GetRecordsRequest(&kinesis.GetRecordsInput{
		Limit:         aws.Int64(int64(batchSize)),
		ShardIterator: aws.String(r.nextShardIterator),
	})

	// If debug is turned on, add some handlers for GetRecords logging
	if r.LogLevel.AtLeast(logging.LogDebug) {
		req.Handlers.Send.PushBack(func(req *request.Request) {
			r.LogDebug("Finished getRecords Send, took", time.Since(start))
		})
	}

	// Here, we insert a handler to be called after the Send handler and before the the Unmarshal handler in the
	// aws-go-sdk library.
	//
	// The Send handler will call http.Client.Do() on the request, which blocks until the response headers have been
	// read before returning an HTTPResponse.
	//
	// The Unmarshal handler will ultimately call ioutil.ReadAll() on the HTTPResponse.Body stream.
	//
	// Our handler wraps the HTTPResponse.Body with our own ReadCloser so that we can implement a timeout mechanism
	// on the Read() call (which is called by the ioutil.ReadAll() function)
	req.Handlers.Unmarshal.PushFront(func(req *request.Request) {
		r.LogDebug("Started getRecords Unmarshal, took", time.Since(start))
		// Here, we set a timer that the initial Read() call on HTTPResponse.Body must return by.  Note that the
		// normal http.Client Timeout is still in effect.
		startReadTime = time.Now()
		timer := time.NewTimer(r.readTimeout)

		req.HTTPResponse.Body = &ReadCloserWrapper{
			ReadCloser: req.HTTPResponse.Body,
			OnReadFn: func(stream io.ReadCloser, b []byte) (n int, err error) {
				// The OnReadFn will be called each time ioutil.ReadAll calls Read on the
				// ReadCloserWrapper.

				// First, we set up a struct that to hold the results of the Read() call that can go
				// through a channel
				type Result struct {
					n   int
					err error
				}

				// Next, we build a channel with which to pass the Read() results
				c := make(chan Result, 1)

				// Now, we call the Read() on the HTTPResponse.Body in a goroutine and feed the results
				// into the channel
				readStart := time.Now()
				go func() {
					var result Result
					result.n, result.err = stream.Read(b)
					c <- result
				}()

				// Finally, we poll for the Read() to complete or the timer to elapse.
				select {
				case result := <-c:
					// If we sucessfully Read() from the HTTPResponse.Body, we reset our timeout and
					// return the results from the Read()
					timer.Reset(r.readTimeout)
					n, err = result.n, result.err
					r.LogDebug(fmt.Sprintf("getRecords read %d bytes, took %v", n, time.Since(readStart)))
				case <-timer.C:
					// If we timeout, we return an error that will unblock ioutil.ReadAll(). This
					// will cause the Unmarshal handler to return an error.  This error will
					// propogate to the original req.Send() call (below)
					r.LogDebug(fmt.Sprintf("getRecords read timed out after %v", time.Since(readStart)))
					err = errs.ErrTimeoutReadResponseBody
				case <-ctx.Done():
					// The pipe of death will abort any pending reads on a GetRecords call.
					r.LogDebug(fmt.Sprintf("getRecords received ctx.Done() after %v", time.Since(readStart)))
					err = ctx.Err()
				}
				return
			},
			OnCloseFn: func() {
				r.Stats.AddGetRecordsReadResponseDuration(time.Since(startReadTime))
				r.LogDebug("Finished GetRecords body read, took", time.Since(start))
				startUnmarshalTime = time.Now()
			},
		}
	})

	req.Handlers.Unmarshal.PushBack(func(req *request.Request) {
		r.Stats.AddGetRecordsUnmarshalDuration(time.Since(startUnmarshalTime))
		r.LogDebug("Finished GetRecords Unmarshal, took", time.Since(start))
	})

	// Send the GetRecords request
	r.LogDebug("Starting GetRecords Build/Sign request, took", time.Since(start))
	r.Stats.AddGetRecordsCalled(1)
	if err := req.Send(); err != nil {
		r.LogError("Error getting records:", err)
		switch err.(awserr.Error).Code() {
		case kinesis.ErrCodeProvisionedThroughputExceededException:
			r.Stats.AddProvisionedThroughputExceeded(1)
		default:
			r.LogDebug("Received AWS error:", err.Error())
		}
		return 0, err
	}
	r.Stats.AddGetRecordsDuration(time.Since(start))

	// Process Records
	r.LogDebug(fmt.Sprintf("Finished GetRecords request, %d records from shard %s, took %v\n", len(resp.Records), r.shard, time.Since(start)))
	if resp == nil {
		return 0, errs.ErrNilGetRecordsResponse
	}
	delivered := 0
	r.Stats.AddBatchSize(len(resp.Records))
	for _, record := range resp.Records {
		if record != nil {
			// Allow (only) a pipeOfDeath to trigger an instance shutdown of the loop to deliver messages.
			// Otherwise, a normal cancellation will not prevent getRecords from completing the delivery of
			// the current batch of records.
			select {
			case <-ctx.Done():
				r.LogInfo(fmt.Sprintf("getRecords received ctx.Done() while delivering messages, %d delivered, ~%d dropped", delivered, len(resp.Records)-delivered))
				return delivered, ctx.Err()
			default:
				var wg sync.WaitGroup
				wg.Add(1)
				go fn(message.FromRecord(record), &wg)
				wg.Wait()
				delivered++
				r.Stats.AddConsumed(1)
				if record.SequenceNumber != nil {
					// We can safely ignore if this call returns error, as if we somehow receive an
					// empty sequence number from AWS, we will simply not set it.  At worst, this
					// causes us to reprocess this record if we happen to refresh the iterator.
					r.setSequenceNumber(*record.SequenceNumber)
				}
			}
		}
	}
	if resp.NextShardIterator != nil {
		// TODO: According to AWS docs:
		// http://docs.aws.amazon.com/sdk-for-go/api/service/kinesis/#GetRecordsOutput
		//
		// NextShardIterator: The next position in the shard from which to start sequentially reading data
		// records.  If set to null, the shard has been closed and the requested iterator will not return any
		// more data.
		//
		// When dealing with streams that will merge or split, we need to detect that the shard has closed and
		// notify the client library.
		//
		// TODO: I don't know if we should be ignoring an error returned by setShardIterator in case of an empty
		// shard iterator in the response.  There isn't much we can do, and the best path for recovery may be
		// simply to reprocess the batch and see if we get a valid NextShardIterator from AWS the next time
		// around.
		r.setNextShardIterator(*resp.NextShardIterator)
	}
	return delivered, nil
}

// GetRecord calls getRecords and delivers one record into the messages channel.
func (r *KinesisReader) GetRecord(ctx context.Context, fn MessageFn) (int, error) {
	return r.getRecords(ctx, fn, 1)
}

// GetRecords calls getRecords and delivers each record into the messages channel.
func (r *KinesisReader) GetRecords(ctx context.Context, fn MessageFn) (int, error) {
	return r.getRecords(ctx, fn, r.batchSize)
}