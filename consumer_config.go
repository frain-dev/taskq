package taskq

import (
	"fmt"
	"time"
)

type perfProfile struct {
	start     time.Time
	processed int
	retries   int
	timing    time.Duration

	tps       float64
	errorRate float64
}

func (p *perfProfile) Reset(processed, retries int) {
	p.start = time.Now()
	p.processed = processed
	p.retries = retries
}

func (p *perfProfile) Update(processed, retries int, timing time.Duration) {
	processedDiff := processed - p.processed
	retriesDiff := retries - p.retries
	total := processedDiff + retriesDiff
	elapsed := time.Since(p.start)

	elapsedMS := float64(elapsed) / float64(time.Millisecond)
	p.tps = float64(processedDiff) / elapsedMS

	if total > 0 {
		p.errorRate = float64(retriesDiff) / float64(total)
	} else {
		p.errorRate = 0
	}

	p.timing = timing
}

func (p *perfProfile) TPS() float64 {
	return p.tps
}

func (p *perfProfile) ErrorRate() float64 {
	return p.errorRate
}

func (p *perfProfile) Timing() time.Duration {
	return p.timing
}

//------------------------------------------------------------------------------

type consumerConfig struct {
	NumFetcher int32
	NumWorker  int32

	perfProfile

	NumSelected int
	Score       float64
}

func NewConsumerConfig(numFetcher, numWorker int32) *consumerConfig {
	return &consumerConfig{
		NumFetcher: numFetcher,
		NumWorker:  numWorker,
	}
}

func (cfg *consumerConfig) SetScore(score float64) {
	if cfg.Score == 0 {
		cfg.Score = score
	} else {
		cfg.Score = (cfg.Score + score) / 2
	}
}

func (cfg *consumerConfig) String() string {
	return fmt.Sprintf("fetchers=%d workers=%d tps=%f failure=%f timing=%s score=%f selected=%d",
		cfg.NumFetcher, cfg.NumWorker, cfg.tps, cfg.ErrorRate(), cfg.timing, cfg.Score, cfg.NumSelected)
}

func (cfg *consumerConfig) Equal(other *consumerConfig) bool {
	if other == nil {
		return false
	}
	return cfg.NumWorker == other.NumWorker && cfg.NumFetcher == other.NumFetcher
}

func (cfg *consumerConfig) Clone() *consumerConfig {
	return &consumerConfig{
		NumWorker:  cfg.NumWorker,
		NumFetcher: cfg.NumFetcher,
	}
}

//------------------------------------------------------------------------------

type configRoulette struct {
	opt *QueueOptions

	maxTPS    float64
	maxTiming time.Duration
	currCfg   *consumerConfig
}

func newConfigRoulette(opt *QueueOptions) *configRoulette {
	r := &configRoulette{
		opt: opt,
	}

	r.resetConfig()
	return r
}

func (r *configRoulette) Select(currCfg *consumerConfig, queueEmpty bool) *consumerConfig {
	r.currCfg = currCfg
	r.incrementConfig(queueEmpty)
	return r.currCfg
}

func (r *configRoulette) resetConfig() {
	r.maxTPS = 0
	r.maxTiming = 0
}
func (r *configRoulette) incrementConfig(queueEmpty bool) {
	r.withMoreWorkers(5, queueEmpty)
	r.withMoreFetchers()
}

func (r *configRoulette) withMoreWorkers(n int32, queueEmpty bool) *consumerConfig {
	if !hasFreeSystemResources() {
		return r.currCfg
	}
	if queueEmpty {
		return r.currCfg
	}
	r.currCfg.NumWorker += n
	return r.currCfg
}

func (r *configRoulette) withMoreFetchers() *consumerConfig {
	if !hasFreeSystemResources() {
		return r.currCfg
	}
	if r.currCfg.NumFetcher >= r.opt.MaxNumFetcher {
		return r.currCfg
	}
	r.currCfg.NumFetcher++
	return r.currCfg
}
