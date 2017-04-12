package processor

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsm/redis-lock"

	"github.com/go-msgqueue/msgqueue"
	"github.com/go-msgqueue/msgqueue/internal"
)

const consumerBackoff = time.Second
const maxBackoff = 12 * time.Hour
const stopTimeout = 30 * time.Second

type Delayer interface {
	Delay() time.Duration
}

type Stats struct {
	InFlight    uint32
	Deleting    uint32
	Processed   uint32
	Retries     uint32
	Fails       uint32
	AvgDuration time.Duration
}

// Processor reserves messages from the queue, processes them,
// and then either releases or deletes messages from the queue.
type Processor struct {
	q   Queuer
	opt *msgqueue.Options

	handler         msgqueue.Handler
	fallbackHandler msgqueue.Handler

	wg sync.WaitGroup
	ch chan *msgqueue.Message

	workersWG   sync.WaitGroup
	stopWorkers chan struct{}

	workerChans []chan struct{}
	workerLocks []*lock.Lock

	delBatch *internal.Batcher

	_started uint32

	errCount   uint32
	delayCount uint32
	delaySec   uint32

	inFlight    uint32
	deleting    uint32
	processed   uint32
	fails       uint32
	retries     uint32
	avgDuration uint32
}

// New creates new Processor for the queue using provided processing options.
func New(q Queuer, opt *msgqueue.Options) *Processor {
	opt.Init()
	p := &Processor{
		q:   q,
		opt: opt,

		ch: make(chan *msgqueue.Message, opt.BufferSize),
	}

	if opt.MaxWorkers > 0 {
		p.workerChans = make([]chan struct{}, opt.MaxWorkers)
		p.workerLocks = make([]*lock.Lock, opt.MaxWorkers)
		for i := 0; i < opt.MaxWorkers; i++ {
			p.workerChans[i] = make(chan struct{}, 1)

			key := fmt.Sprintf("%s:worker-lock:%d", p.q.Name(), i)
			p.workerLocks[i] = lock.NewLock(opt.Redis, key, &lock.LockOptions{
				LockTimeout: opt.ReservationTimeout,
			})
		}

		go p.readWorkerMessages()
	}

	p.setHandler(opt.Handler)
	if opt.FallbackHandler != nil {
		p.setFallbackHandler(opt.FallbackHandler)
	}

	p.delBatch = internal.NewBatcher(p.opt.WorkerNumber, p.deleteBatch)

	return p
}

// Starts creates new Processor and starts it.
func Start(q Queuer, opt *msgqueue.Options) *Processor {
	p := New(q, opt)
	p.Start()
	return p
}

func (p *Processor) String() string {
	return fmt.Sprintf(
		"Processor<%s workers=%d buffer=%d>",
		p.q.Name(), p.opt.WorkerNumber, p.opt.BufferSize,
	)
}

// Stats returns processor stats.
func (p *Processor) Stats() *Stats {
	return &Stats{
		InFlight:    atomic.LoadUint32(&p.inFlight),
		Deleting:    atomic.LoadUint32(&p.deleting),
		Processed:   atomic.LoadUint32(&p.processed),
		Retries:     atomic.LoadUint32(&p.retries),
		Fails:       atomic.LoadUint32(&p.fails),
		AvgDuration: time.Duration(atomic.LoadUint32(&p.avgDuration)) * time.Millisecond,
	}
}

func (p *Processor) setHandler(handler interface{}) {
	p.handler = msgqueue.NewHandler(handler)
}

func (p *Processor) setFallbackHandler(handler interface{}) {
	p.fallbackHandler = msgqueue.NewHandler(handler)
}

// Add adds message to the processor internal queue.
func (p *Processor) Add(msg *msgqueue.Message) error {
	p.wg.Add(1)
	atomic.AddUint32(&p.inFlight, 1)
	p.ch <- msg
	return nil
}

// Add adds message to the processor internal queue with specified delay.
func (p *Processor) AddDelay(msg *msgqueue.Message, delay time.Duration) error {
	if delay == 0 {
		return p.Add(msg)
	}

	p.wg.Add(1)
	atomic.AddUint32(&p.inFlight, 1)
	time.AfterFunc(delay, func() {
		p.ch <- msg
	})
	return nil
}

// Process is low-level API to process message bypassing the internal queue.
func (p *Processor) Process(msg *msgqueue.Message) error {
	p.wg.Add(1)
	return p.process(-1, msg)
}

// Start starts processing messages in the queue.
func (p *Processor) Start() error {
	if !p.startWorkers() {
		return nil
	}

	p.workersWG.Add(1)
	go p.messageFetcher()

	return nil
}

func (p *Processor) startWorkers() bool {
	if !atomic.CompareAndSwapUint32(&p._started, 0, 1) {
		return false
	}

	p.stopWorkers = make(chan struct{})
	p.workersWG.Add(p.opt.WorkerNumber)
	for i := 0; i < p.opt.WorkerNumber; i++ {
		go p.worker(i)
	}
	return true
}

// Stop is StopTimeout with 30 seconds timeout.
func (p *Processor) Stop() error {
	return p.stopWorkersTimeout(stopTimeout)
}

// StopTimeout waits workers for timeout duration to finish processing current
// messages and stops workers.
func (p *Processor) StopTimeout(timeout time.Duration) error {
	return p.stopWorkersTimeout(timeout)
}

func (p *Processor) stopWorkersTimeout(timeout time.Duration) error {
	if !atomic.CompareAndSwapUint32(&p._started, 1, 0) {
		return nil
	}

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-time.After(timeout):
		return fmt.Errorf("workers did not stop after %s", timeout)
	case <-done:
		close(p.stopWorkers)
		p.workersWG.Wait()
		p.delBatch.Wait()
		return nil
	}
}

func (p *Processor) stopped() bool {
	return atomic.LoadUint32(&p._started) == 0
}

func (p *Processor) paused() time.Duration {
	const threshold = 100

	if atomic.LoadUint32(&p.delayCount) > threshold {
		return time.Duration(atomic.LoadUint32(&p.delaySec)) * time.Second
	}

	if atomic.LoadUint32(&p.errCount) > threshold {
		return time.Minute
	}

	return 0
}

// ProcessAll starts workers to process messages in the queue and then stops
// them when all messages are processed.
func (p *Processor) ProcessAll() error {
	p.startWorkers()
	var noWork int
	for {
		isIdle := atomic.LoadUint32(&p.inFlight) == 0
		n, err := p.fetchMessages()
		if err != nil {
			if err != internal.ErrNotSupported {
				return err
			}
		}
		if n == 0 && isIdle {
			noWork++
		} else {
			noWork = 0
		}
		if noWork == 2 {
			break
		}
		if err == internal.ErrNotSupported {
			// Don't burn CPU.
			time.Sleep(100 * time.Millisecond)
		}
	}
	return p.stopWorkersTimeout(stopTimeout)
}

// ProcessOne processes at most one message in the queue.
func (p *Processor) ProcessOne() error {
	msg, err := p.reserveOne()
	if err != nil {
		return err
	}

	firstErr := p.Process(msg)
	if err := p.delBatch.Wait(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (p *Processor) reserveOne() (*msgqueue.Message, error) {
	select {
	case msg := <-p.ch:
		return msg, nil
	default:
	}

	msgs, err := p.q.ReserveN(1)
	if err != nil && err != internal.ErrNotSupported {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, errors.New("msgqueue: queue is empty")
	}
	atomic.AddUint32(&p.inFlight, 1)
	return &msgs[0], nil
}

func (p *Processor) messageFetcher() {
	defer p.workersWG.Done()
	for {
		if p.stopped() {
			break
		}

		if pauseTime := p.paused(); pauseTime > 0 {
			p.resetPause()
			log.Printf("msgqueue: %s is automatically paused for dur=%s", p.q, pauseTime)
			time.Sleep(pauseTime)
			continue
		}

		_, err := p.fetchMessages()
		if err != nil {
			if err == internal.ErrNotSupported {
				break
			}

			log.Printf(
				"msgqueue: %s ReserveN failed: %s (sleeping for dur=%s)",
				p.q, err, consumerBackoff,
			)
			time.Sleep(consumerBackoff)
			continue
		}
	}
}

func (p *Processor) fetchMessages() (int, error) {
	msgs, err := p.q.ReserveN(p.opt.BufferSize)
	if err != nil {
		return 0, err
	}
	for i := range msgs {
		p.Add(&msgs[i])
	}
	return len(msgs), nil
}

func (p *Processor) worker(id int) {
	defer p.workersWG.Done()
	for {
		msg, ok := p.dequeueMessage()
		if !ok {
			break
		}

		if p.opt.RateLimiter != nil {
			for {
				delay, allow := p.opt.RateLimiter.AllowRate(p.q.Name(), p.opt.RateLimit)
				if allow {
					break
				}
				time.Sleep(delay)
			}
		}

		p.process(id, msg)
	}
}

func (p *Processor) process(workerId int, msg *msgqueue.Message) error {
	if msg.Delay > 0 {
		p.release(msg, nil)
		return nil
	}

	if workerId >= 0 && p.opt.MaxWorkers > 0 {
		p.lockWorker(workerId)
	}

	start := time.Now()
	err := p.handler.HandleMessage(msg)
	p.updateAvgDuration(time.Since(start))

	if workerId >= 0 && p.opt.MaxWorkers > 0 {
		p.unlockWorker(workerId)
	}

	if err == nil {
		atomic.AddUint32(&p.processed, 1)
		p.delete(msg, nil)
		return nil
	}

	if msg.ReservedCount < p.opt.RetryLimit {
		atomic.AddUint32(&p.retries, 1)
		p.release(msg, err)
	} else {
		atomic.AddUint32(&p.fails, 1)
		p.delete(msg, err)
	}

	return err
}

// Purge discards messages from the internal queue.
func (p *Processor) Purge() error {
	for {
		select {
		case msg := <-p.ch:
			p.delete(msg, nil)
		default:
			return nil
		}
	}
}

func (p *Processor) dequeueMessage() (*msgqueue.Message, bool) {
	select {
	case msg := <-p.ch:
		return msg, true
	case <-p.stopWorkers:
		select {
		case msg := <-p.ch:
			return msg, true
		default:
			return nil, false
		}
	}
}

func (p *Processor) release(msg *msgqueue.Message, reason error) {
	delay := p.releaseBackoff(msg, reason)

	if reason != nil {
		log.Printf("%s handler failed (retry in dur=%s): %s", p.q, delay, reason)
	}
	if err := p.q.Release(msg, delay); err != nil {
		log.Printf("%s Release failed: %s", p.q, err)
	}

	atomic.AddUint32(&p.inFlight, ^uint32(0))
	p.wg.Done()
}

func (p *Processor) releaseBackoff(msg *msgqueue.Message, reason error) time.Duration {
	if reason != nil {
		if delayer, ok := reason.(Delayer); ok {
			delay := delayer.Delay()
			if delay > time.Minute {
				atomic.StoreUint32(&p.delaySec, uint32(delay/time.Second))
				atomic.AddUint32(&p.delayCount, 1)
			}
			return delay
		}
	}

	if msg.Delay > 0 {
		return msg.Delay
	}

	return exponentialBackoff(p.opt.MinBackoff, msg.ReservedCount)
}

func (p *Processor) delete(msg *msgqueue.Message, reason error) {
	if reason == nil {
		p.resetPause()
	} else {
		log.Printf("%s handler failed: %s", p.q, reason)

		if p.fallbackHandler != nil {
			if err := p.fallbackHandler.HandleMessage(msg); err != nil {
				log.Printf("%s fallback handler failed: %s", p.q, err)
			}
		}
	}

	atomic.AddUint32(&p.inFlight, ^uint32(0))
	atomic.AddUint32(&p.deleting, 1)
	p.delBatch.Add(msg)
}

func (p *Processor) deleteBatch(msgs []*msgqueue.Message) {
	if err := p.q.DeleteBatch(msgs); err != nil {
		log.Printf("msgqueue: %s DeleteBatch failed: %s", p.q, err)
	}
	atomic.AddUint32(&p.deleting, ^uint32(len(msgs)-1))
	for i := 0; i < len(msgs); i++ {
		p.wg.Done()
	}
}

func (p *Processor) updateAvgDuration(dur time.Duration) {
	const decay = float64(1) / 100
	ms := float64(dur / time.Millisecond)
	for {
		avg := atomic.LoadUint32(&p.avgDuration)
		newAvg := uint32((1-decay)*float64(avg) + decay*ms)
		if atomic.CompareAndSwapUint32(&p.avgDuration, avg, newAvg) {
			break
		}
	}
}

func (p *Processor) resetPause() {
	atomic.StoreUint32(&p.errCount, 0)
	atomic.StoreUint32(&p.delayCount, 0)
}

func (p *Processor) redisWorkerFreeChannel() string {
	return p.q.Name() + ":" + "worker-free"
}

func (p *Processor) readWorkerMessages() {
	pubsub := p.opt.Redis.Subscribe(p.redisWorkerFreeChannel())
	ch := pubsub.Channel()
	for {
		msg, ok := <-ch
		if !ok {
			break
		}

		workerId, err := strconv.Atoi(msg.Payload)
		if err != nil {
			log.Printf("msgqueue: invalid workerId=%q", workerId)
			continue
		}

		workerCh := p.workerChans[workerId]
		select {
		case workerCh <- struct{}{}:
		default:
		}
	}
}

func (p *Processor) lockWorker(id int) {
	const timeout = 1234 * time.Millisecond

	ch := p.workerChans[id]
	lock := p.workerLocks[id]
	for {
		ok, err := lock.Lock()
		if err != nil {
			log.Printf("msgqueue: redlock.Lock failed: %s", err)
			return
		}
		if ok {
			return
		}

		select {
		case <-ch:
		case <-time.After(timeout):
		}
	}
}

func (p *Processor) unlockWorker(id int) {
	lock := p.workerLocks[id]
	if err := lock.Unlock(); err != nil {
		log.Printf("msgqueue: redlock.Unlock failed: %s", err)
	}

	channel := p.redisWorkerFreeChannel()
	err := p.opt.Redis.Publish(channel, strconv.Itoa(id)).Err()
	if err != nil {
		log.Printf("msgqueue: redis.Publish failed: %s", err)
	}
}

func exponentialBackoff(dur time.Duration, retry int) time.Duration {
	dur <<= uint(retry - 1)
	if dur > maxBackoff {
		dur = maxBackoff
	}
	return dur
}
