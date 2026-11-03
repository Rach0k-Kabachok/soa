package rpcclient

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"flightbooking/internal/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type Breaker struct {
	mu                    sync.Mutex
	state                 string
	failures              int
	windowStart, openedAt time.Time
	threshold             int
	timeout, window       time.Duration
	probe                 bool
	now                   func() time.Time
}

func NewBreaker() *Breaker {
	n, err := strconv.Atoi(storage.Env("CB_FAILURE_THRESHOLD", "5"))
	if err != nil || n < 1 {
		panic("CB_FAILURE_THRESHOLD must be positive")
	}
	return &Breaker{state: "CLOSED", threshold: n, timeout: storage.Duration("CB_OPEN_TIMEOUT", "10s"), window: storage.Duration("CB_WINDOW", "30s"), now: time.Now}
}

func transient(err error) bool {
	c := status.Code(err)
	return c == codes.Unavailable || c == codes.DeadlineExceeded
}

func (b *Breaker) transition(state string) {
	slog.Info("circuit breaker transition", "from", b.state, "to", state)
	b.state = state
}

// The generation (openedAt) prevents an old in-flight response from closing a
// breaker that was opened by another call. One probe is allowed in HALF_OPEN.
func (b *Breaker) before() (time.Time, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == "OPEN" {
		if b.now().Sub(b.openedAt) < b.timeout {
			return b.openedAt, status.Error(codes.Unavailable, "flight service circuit is OPEN; retry later")
		}
		b.transition("HALF_OPEN")
	}
	if b.state == "HALF_OPEN" {
		if b.probe {
			return b.openedAt, status.Error(codes.Unavailable, "flight service recovery probe in progress")
		}
		b.probe = true
	}
	return b.openedAt, nil
}

func (b *Breaker) after(generation time.Time, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !generation.Equal(b.openedAt) || b.state == "OPEN" {
		return
	}
	if b.state == "HALF_OPEN" {
		b.probe = false
		if transient(err) {
			b.openedAt = b.now()
			b.transition("OPEN")
		} else {
			b.failures = 0
			b.windowStart = time.Time{}
			b.transition("CLOSED")
		}
		return
	}
	if !transient(err) {
		b.failures = 0
		return
	}
	if b.windowStart.IsZero() || b.now().Sub(b.windowStart) >= b.window {
		b.failures = 0
		b.windowStart = b.now()
	}
	b.failures++
	if b.failures >= b.threshold {
		b.openedAt = b.now()
		b.transition("OPEN")
	}
}

// Circuit breaker surrounds the whole logical call; retry runs inside it.
// The HALF_OPEN probe has exactly one wire attempt.
func (b *Breaker) Interceptor(key string, attemptTimeout time.Duration) grpc.UnaryClientInterceptor {
	if key == "" {
		panic("GRPC_API_KEY is required")
	}
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		generation, err := b.before()
		if err != nil {
			return err
		}
		b.mu.Lock()
		halfOpen := b.state == "HALF_OPEN"
		b.mu.Unlock()
		ctx = metadata.AppendToOutgoingContext(ctx, "x-api-key", key)
		attempts := 3
		if halfOpen {
			attempts = 1
		}
		for i := 0; i < attempts; i++ {
			attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
			err = invoke(attemptCtx, method, req, reply, cc, opts...)
			cancel()
			if !transient(err) || ctx.Err() != nil || i == attempts-1 {
				break
			}
			delay := 100 * time.Millisecond * time.Duration(1<<i)
			slog.Warn("grpc retry", "method", method, "attempt", i+2, "backoff", delay, "code", status.Code(err))
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				err = status.FromContextError(ctx.Err()).Err()
			case <-timer.C:
			}
			if ctx.Err() != nil {
				break
			}
		}
		b.after(generation, err)
		return err
	}
}
