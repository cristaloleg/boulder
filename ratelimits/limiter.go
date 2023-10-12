package ratelimits

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/jmhodges/clock"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// Allowed is used for rate limit metrics, it's the value of the 'decision'
	// label when a request was allowed.
	Allowed = "allowed"

	// Denied is used for rate limit metrics, it's the value of the 'decision'
	// label when a request was denied.
	Denied = "denied"
)

// ErrInvalidCost indicates that the cost specified was <= 0.
var ErrInvalidCost = fmt.Errorf("invalid cost, must be > 0")

// ErrInvalidCostForCheck indicates that the check cost specified was < 0.
var ErrInvalidCostForCheck = fmt.Errorf("invalid check cost, must be >= 0")

// ErrInvalidCostOverLimit indicates that the cost specified was > limit.Burst.
var ErrInvalidCostOverLimit = fmt.Errorf("invalid cost, must be <= limit.Burst")

var errZeroBucketsInBatch = errors.New("invalid batch, a batch must have >= 1 bucket")

// errLimitDisabled indicates that the limit name specified is valid but is not
// currently configured.
var errLimitDisabled = errors.New("limit disabled")

// disabledLimitDecision is an "allowed" *Decision that should be returned when
// a checked limit is found to be disabled.
var disabledLimitDecision = &Decision{true, 0, 0, 0, time.Time{}}

// Limiter provides a high-level interface for rate limiting requests by
// utilizing a leaky bucket-style approach.
type Limiter struct {
	// defaults stores default limits by 'name'.
	defaults limits

	// overrides stores override limits by 'name:id'.
	overrides limits

	// source is used to store buckets. It must be safe for concurrent use.
	source source
	clk    clock.Clock

	spendLatency       *prometheus.HistogramVec
	overrideUsageGauge *prometheus.GaugeVec
}

// NewLimiter returns a new *Limiter. The provided source must be safe for
// concurrent use. The defaults and overrides paths are expected to be paths to
// YAML files that contain the default and override limits, respectively. The
// overrides file is optional, all other arguments are required.
func NewLimiter(clk clock.Clock, source source, defaults, overrides string, stats prometheus.Registerer) (*Limiter, error) {
	limiter := &Limiter{source: source, clk: clk}

	var err error
	limiter.defaults, err = loadAndParseDefaultLimits(defaults)
	if err != nil {
		return nil, err
	}

	limiter.spendLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "ratelimits_spend_latency",
		Help: fmt.Sprintf("Latency of ratelimit checks labeled by limit=[name] and decision=[%s|%s], in seconds", Allowed, Denied),
		// Exponential buckets ranging from 0.0005s to 3s.
		Buckets: prometheus.ExponentialBuckets(0.0005, 3, 8),
	}, []string{"limit", "decision"})
	stats.MustRegister(limiter.spendLatency)

	if overrides == "" {
		// No overrides specified, initialize an empty map.
		limiter.overrides = make(limits)
		return limiter, nil
	}

	limiter.overrides, err = loadAndParseOverrideLimits(overrides)
	if err != nil {
		return nil, err
	}

	limiter.overrideUsageGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ratelimits_override_usage",
		Help: "Proportion of override limit used, by limit name and bucket key.",
	}, []string{"limit", "bucket_key"})
	stats.MustRegister(limiter.overrideUsageGauge)

	return limiter, nil
}

type Decision struct {
	// Allowed is true if the bucket possessed enough capacity to allow the
	// request given the cost.
	Allowed bool

	// Remaining is the number of requests the client is allowed to make before
	// they're rate limited.
	Remaining int64

	// RetryIn is the duration the client MUST wait before they're allowed to
	// make a request.
	RetryIn time.Duration

	// ResetIn is the duration the bucket will take to refill to its maximum
	// capacity, assuming no further requests are made.
	ResetIn time.Duration

	// newTAT indicates the time at which the bucket will be full. It is the
	// theoretical arrival time (TAT) of next request. It must be no more than
	// (burst * (period / count)) in the future at any single point in time.
	newTAT time.Time
}

// Check DOES NOT deduct the cost of the request from the provided bucket's
// capacity. The returned *Decision indicates whether the capacity exists to
// satisfy the cost and represents the hypothetical state of the bucket IF the
// cost WERE to be deducted. If no bucket exists it will NOT be created. No
// state is persisted to the underlying datastore.
func (l *Limiter) Check(ctx context.Context, bucket BucketWithCost) (*Decision, error) {
	if bucket.cost < 0 {
		return nil, ErrInvalidCostForCheck
	}

	limit, err := l.getLimit(bucket.name, bucket.key)
	if err != nil {
		if errors.Is(err, errLimitDisabled) {
			return disabledLimitDecision, nil
		}
		return nil, err
	}

	if bucket.cost > limit.Burst {
		return nil, ErrInvalidCostOverLimit
	}

	// Remove cancellation from the request context so that transactions are not
	// interrupted by a client disconnect.
	ctx = context.WithoutCancel(ctx)
	tat, err := l.source.Get(ctx, bucket.key)
	if err != nil {
		if !errors.Is(err, ErrBucketNotFound) {
			return nil, err
		}
		// First request from this client. No need to initialize the bucket
		// because this is a check, not a spend. A TAT of "now" is equivalent to
		// a full bucket.
		return maybeSpend(l.clk, limit, l.clk.Now(), bucket.cost), nil
	}
	return maybeSpend(l.clk, limit, tat, bucket.cost), nil
}

// Spend attempts to deduct the cost from the provided bucket's capacity. The
// returned *Decision The returned *Decision indicates whether the capacity
// existed to satisfy the cost and represents the current state of the bucket.
// If no bucket exists it WILL be created WITH the cost factored into its
// initial state. The new bucket state is persisted to the underlying datastore,
// if applicable, before returning.
func (l *Limiter) Spend(ctx context.Context, bucket BucketWithCost) (*Decision, error) {
	if bucket.cost <= 0 {
		return nil, ErrInvalidCost
	}

	limit, err := l.getLimit(bucket.name, bucket.key)
	if err != nil {
		if errors.Is(err, errLimitDisabled) {
			return disabledLimitDecision, nil
		}
		return nil, err
	}

	if bucket.cost > limit.Burst {
		return nil, ErrInvalidCostOverLimit
	}

	start := l.clk.Now()
	status := Denied
	defer func() {
		l.spendLatency.WithLabelValues(bucket.name.String(), status).Observe(l.clk.Since(start).Seconds())
	}()

	// Remove cancellation from the request context so that transactions are not
	// interrupted by a client disconnect.
	ctx = context.WithoutCancel(ctx)
	tat, err := l.source.Get(ctx, bucket.key)
	if err != nil {
		if errors.Is(err, ErrBucketNotFound) {
			// First request from this client.
			d, err := l.initialize(ctx, limit, bucket)
			if err != nil {
				return nil, err
			}
			if d.Allowed {
				status = Allowed
			}
			return d, nil
		}
		return nil, err
	}

	d := maybeSpend(l.clk, limit, tat, bucket.cost)

	if limit.isOverride {
		// Calculate the current utilization of the override limit.
		utilization := float64(limit.Burst-d.Remaining) / float64(limit.Burst)
		l.overrideUsageGauge.WithLabelValues(bucket.name.String(), bucket.key).Set(utilization)
	}

	if !d.Allowed {
		return d, nil
	}

	err = l.source.Set(ctx, bucket.key, d.newTAT)
	if err != nil {
		return nil, err
	}
	status = Allowed
	return d, nil
}

// BatchSpend attempts to deduct the cost from the provided buckets' capacities
// in a batch. The returned consolidated *Decision indicates the following:
//   - Allowed is true if all spend requests were successful,
//   - Remaining is the smallest value across all processed buckets, and
//   - RetryIn and ResetIn are the largest values across all processed buckets.
//
// Non-existent buckets will be created WITH the cost factored into the initial
// state. The new bucket states are persisted to the underlying datastore, if
// applicable, before returning.
func (l *Limiter) BatchSpend(ctx context.Context, buckets []BucketWithCost) (*Decision, error) {
	if len(buckets) == 0 {
		return nil, errZeroBucketsInBatch
	}

	bucketKeys := make([]string, 0, len(buckets))
	for _, bucket := range buckets {
		if bucket.cost <= 0 {
			return nil, ErrInvalidCost
		}
		bucketKeys = append(bucketKeys, bucket.key)
	}

	// Remove cancellation from the request context so that transactions are not
	// interrupted by a client disconnect.
	ctx = context.WithoutCancel(ctx)
	tats, err := l.source.BatchGet(ctx, bucketKeys)
	if err != nil {
		return nil, err
	}

	// Track the limits that were checked for metrics purposes.
	var limitsForMetrics []string

	var minRemaining int64 = math.MaxInt64
	var maxRetryIn time.Duration
	var maxResetIn time.Duration
	var maxNewTAT time.Time
	newTATs := make(map[string]time.Time)
	allowed := true

	// Assign "now" TAT outside of the loop to avoid clock skew.
	nowTAT := l.clk.Now()

	for _, bucket := range buckets {
		tat, exists := tats[bucket.key]
		limit, err := l.getLimit(bucket.name, bucket.key)
		if err != nil {
			if errors.Is(err, errLimitDisabled) {
				// Ignore disabled limit.
				continue
			}
			return nil, err
		}
		if !slices.Contains(limitsForMetrics, bucket.name.String()) {
			limitsForMetrics = append(limitsForMetrics, bucket.name.String())
		}

		if !exists || tat.IsZero() {
			// First request from this client. Initialize the bucket with a TAT of
			// "now", which is equivalent to a full bucket.
			tat = nowTAT
		}

		// Spend the cost and update the consolidated decision.
		d := maybeSpend(l.clk, limit, tat, bucket.cost)
		if d.Allowed {
			newTATs[bucket.key] = d.newTAT
		}

		if limit.isOverride {
			// Calculate the current utilization of the override limit.
			utilization := float64(limit.Burst-d.Remaining) / float64(limit.Burst)
			l.overrideUsageGauge.WithLabelValues(bucket.name.String(), bucket.key).Set(utilization)
		}

		// All spend decisions must be allowed for the batch to be considered
		// allowed.
		allowed = allowed && d.Allowed
		minRemaining = min(minRemaining, d.Remaining)
		maxRetryIn = max(maxRetryIn, d.RetryIn)
		maxResetIn = max(maxResetIn, d.ResetIn)
		if d.newTAT.After(maxNewTAT) {
			maxNewTAT = d.newTAT
		}
	}

	start := l.clk.Now()
	status := Denied
	defer func() {
		// Sort the limits for metrics so that the order is consistent.
		slices.Sort(limitsForMetrics)
		batch := strings.Join(limitsForMetrics, ",")
		l.spendLatency.WithLabelValues(batch, status).Observe(l.clk.Since(start).Seconds())
	}()

	// Conditionally, spend the batch.
	if len(newTATs) > 0 && allowed {
		err = l.source.BatchSet(ctx, newTATs)
		if err != nil {
			return nil, err
		}
	}
	status = Allowed

	// Consolidated decision for the batch.
	return &Decision{
		Allowed:   allowed,
		Remaining: minRemaining,
		RetryIn:   maxRetryIn,
		ResetIn:   maxResetIn,
		newTAT:    maxNewTAT,
	}, nil
}

// Refund attempts to refund all of the cost to the capacity of the specified
// bucket. The returned *Decision indicates whether the refund was successful
// and represents the current state of the bucket. The new bucket state is
// persisted to the underlying datastore, if applicable, before returning. If no
// bucket exists it will NOT be created.
//
// Note: The amount refunded cannot cause the bucket to exceed its maximum
// capacity. Partial refunds are allowed and are considered successful. For
// instance, if a bucket has a maximum capacity of 10 and currently has 5
// requests remaining, a refund request of 7 will result in the bucket reaching
// its maximum capacity of 10, not 12.
func (l *Limiter) Refund(ctx context.Context, bucket BucketWithCost) (*Decision, error) {
	if bucket.cost <= 0 {
		return nil, ErrInvalidCost
	}

	limit, err := l.getLimit(bucket.name, bucket.key)
	if err != nil {
		if errors.Is(err, errLimitDisabled) {
			return disabledLimitDecision, nil
		}
		return nil, err
	}

	// Remove cancellation from the request context so that transactions are not
	// interrupted by a client disconnect.
	ctx = context.WithoutCancel(ctx)
	tat, err := l.source.Get(ctx, bucket.key)
	if err != nil {
		return nil, err
	}
	d := maybeRefund(l.clk, limit, tat, bucket.cost)
	if !d.Allowed {
		// The bucket is already at maximum capacity.
		return d, nil
	}
	return d, l.source.Set(ctx, bucket.key, d.newTAT)
}

// BatchRefund attempts to refund quota to specified buckets in a batch by
// adding back all or some of the cost to the bucket's capacity. The returned
// consolidated *Decision indicates the following:
//   - Allowed is true if at least one refund was successful,
//   - Remaining is the smallest value across all processed buckets, and
//   - RetryIn and ResetIn are the largest values across all processed buckets.
//
// Non-existent buckets within the batch are disregarded without error, as this
// is equivalent to the bucket being full. The new bucket state is persisted to
// the underlying datastore, if applicable, before returning.
func (l *Limiter) BatchRefund(ctx context.Context, buckets []BucketWithCost) (*Decision, error) {
	if len(buckets) == 0 {
		return nil, errZeroBucketsInBatch
	}

	bucketKeys := make([]string, 0, len(buckets))
	for _, bucket := range buckets {
		if bucket.cost <= 0 {
			return nil, ErrInvalidCost
		}
		bucketKeys = append(bucketKeys, bucket.key)
	}

	// Remove cancellation from the request context so that transactions are not
	// interrupted by a client disconnect.
	ctx = context.WithoutCancel(ctx)
	tats, err := l.source.BatchGet(ctx, bucketKeys)
	if err != nil {
		return nil, err
	}

	var minRemaining int64 = math.MaxInt64
	var maxRetryIn time.Duration
	var maxResetIn time.Duration
	var maxNewTAT time.Time
	var allowed bool
	newTATs := make(map[string]time.Time)

	for _, bucket := range buckets {
		tat, exists := tats[bucket.key]
		limit, err := l.getLimit(bucket.name, bucket.key)
		if err != nil {
			if errors.Is(err, errLimitDisabled) {
				// Ignore disabled limit.
				continue
			}
			return nil, err
		}

		if !exists || tat.IsZero() {
			// If the bucket no longer exists, ignore it. A missing bucket is
			// equivalent to a full bucket.
			continue
		}

		// Refund the cost and update the consolidated decision.
		d := maybeRefund(l.clk, limit, tat, bucket.cost)
		if d.Allowed {
			newTATs[bucket.key] = d.newTAT
		}

		// At least one refund must be allowed for the batch to be considered
		// allowed.
		allowed = allowed || d.Allowed
		minRemaining = min(minRemaining, d.Remaining)
		maxRetryIn = max(maxRetryIn, d.RetryIn)
		maxResetIn = max(maxResetIn, d.ResetIn)
		if d.newTAT.After(maxNewTAT) {
			maxNewTAT = d.newTAT
		}
	}

	// Conditionally, refund the batch.
	if len(newTATs) > 0 {
		err = l.source.BatchSet(ctx, newTATs)
		if err != nil {
			return nil, err
		}
	}

	// Consolidated decision for the batch.
	return &Decision{
		Allowed:   allowed,
		Remaining: minRemaining,
		RetryIn:   maxRetryIn,
		ResetIn:   maxResetIn,
		newTAT:    maxNewTAT,
	}, nil
}

// Reset resets the specified bucket to its maximum capacity. The new bucket
// state is persisted to the underlying datastore before returning.
func (l *Limiter) Reset(ctx context.Context, bucket Bucket) error {
	// Remove cancellation from the request context so that transactions are not
	// interrupted by a client disconnect.
	ctx = context.WithoutCancel(ctx)
	return l.source.Delete(ctx, bucket.key)
}

// initialize creates a new bucket and sets its TAT to now, which is equivalent
// to a full bucket. The new bucket state is persisted to the underlying
// datastore before returning.
func (l *Limiter) initialize(ctx context.Context, rl limit, bucket BucketWithCost) (*Decision, error) {
	d := maybeSpend(l.clk, rl, l.clk.Now(), bucket.cost)

	// Remove cancellation from the request context so that transactions are not
	// interrupted by a client disconnect.
	ctx = context.WithoutCancel(ctx)
	err := l.source.Set(ctx, bucket.key, d.newTAT)
	if err != nil {
		return nil, err
	}
	return d, nil

}

// getLimit returns the limit for the specified by name and bucketKey, name is
// required, bucketKey is optional. If bucketKey is not specified, the default
// limit is returned. If bucketKey is specified, the override limit is returned
// if it exists, otherwise the default limit is returned.
func (l *Limiter) getLimit(name Name, bucketKey string) (limit, error) {
	if !name.isValid() {
		// This should never happen. Callers should only be specifying the limit
		// Name enums defined in this package.
		return limit{}, fmt.Errorf("specified name enum %q, is invalid", name)
	}
	if bucketKey != "" {
		// Check for override.
		ol, ok := l.overrides[bucketKey]
		if ok {
			return ol, nil
		}
	}
	dl, ok := l.defaults[name.EnumString()]
	if ok {
		return dl, nil
	}
	return limit{}, errLimitDisabled
}
