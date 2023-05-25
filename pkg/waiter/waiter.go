package waiter

import (
	"context"
	"fmt"
	"math"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/muesli/reflow/wordwrap"
	"github.com/rs/zerolog"
	v1 "namespacelabs.dev/breakpoint/api/private/v1"
	"namespacelabs.dev/breakpoint/pkg/webhook"
)

const (
	logTickInterval = 1 * time.Minute

	Stamp = time.Stamp + " MST"
)

type ManagerOpts struct {
	InitialDur time.Duration

	Webhooks []v1.Webhook
}

type Manager struct {
	ctx    context.Context
	logger zerolog.Logger

	opts ManagerOpts

	mu         sync.Mutex
	updated    chan struct{}
	expiration time.Time
	endpoint   string
}

func NewManager(ctx context.Context, opts ManagerOpts) (*Manager, context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	l := zerolog.Ctx(ctx).With().Logger()
	m := &Manager{
		ctx:        ctx,
		logger:     l,
		opts:       opts,
		updated:    make(chan struct{}, 1),
		expiration: time.Now().Add(opts.InitialDur),
	}

	go func() {
		defer cancel()
		m.loop(ctx)
	}()

	return m, ctx
}

func (m *Manager) Wait() error {
	<-m.ctx.Done()
	return m.ctx.Err()
}

func (m *Manager) loop(ctx context.Context) {
	exitTimer := time.NewTicker(time.Until(m.expiration))
	defer exitTimer.Stop()

	logTicker := time.NewTicker(logTick())
	defer logTicker.Stop()

	for {
		select {
		case _, ok := <-m.updated:
			if !ok {
				return
			}

			m.mu.Lock()
			newExp := m.expiration
			m.mu.Unlock()

			exitTimer.Reset(time.Until(newExp))
			m.announce()

		case <-exitTimer.C:
			// Timer has expired, terminate the program
			m.logger.Info().Msg("Breakpoint expired")
			return

		case <-logTicker.C:
			m.announce()

		case <-ctx.Done():
			return
		}
	}
}

func logTick() time.Duration {
	// If running in CI, announce on a regular basis.
	if os.Getenv("CI") != "" {
		return logTickInterval
	}

	return math.MaxInt64
}

func (m *Manager) ExtendWait(dur time.Duration) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.expiration = m.expiration.Add(dur)

	m.updated <- struct{}{}

	m.logger.Info().
		Dur("dur", dur).
		Time("expiration", m.expiration).
		Msg("Extend wait")
	return m.expiration
}

func (m *Manager) StopWait() {
	m.logger.Info().Msg("Resume requested")
	close(m.updated)
}

func (m *Manager) Expiration() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.expiration
}

func (m *Manager) SetEndpoint(addr string) {
	m.mu.Lock()
	m.endpoint = addr
	m.mu.Unlock()
	m.updated <- struct{}{}

	host, port, _ := net.SplitHostPort(addr)

	exp := m.Expiration()
	for _, wh := range m.opts.Webhooks {
		ctx, done := context.WithTimeout(m.ctx, 30*time.Second)
		defer done()

		payload := execTemplate(wh.Payload, func(key string) string {
			switch key {
			case "BREAKPOINT_ENDPOINT":
				return addr

			case "BREAKPOINT_HOST":
				return host

			case "BREAKPOINT_PORT":
				return port

			case "BREAKPOINT_TIME_LEFT":
				return strings.TrimSpace(humanize.RelTime(exp, time.Now(), "", ""))

			case "BREAKPOINT_EXPIRATION":
				return exp.Format(Stamp)
			}

			return os.Getenv(key)
		})

		t := time.Now()
		if err := webhook.Notify(ctx, wh.URL, payload); err != nil {
			m.logger.Err(err).Msg("Failed to notify Webhook")
		} else {
			m.logger.Info().Dur("took", time.Since(t)).Str("url", wh.URL).Msg("Notified webhook")
		}
	}
}

func execTemplate(input map[string]any, mapping func(string) string) map[string]any {
	if input == nil {
		return nil
	}

	out := map[string]any{}
	for key, value := range input {
		switch x := value.(type) {
		case map[string]any:
			out[key] = execTemplate(x, mapping)

		case string:
			out[key] = os.Expand(x, mapping)

		default:
			out[key] = value
		}
	}

	return out
}

func (m *Manager) announce() {
	m.mu.Lock()
	host, port, _ := net.SplitHostPort(m.endpoint)
	deadline := m.expiration
	m.mu.Unlock()

	if host == "" && port == "" {
		return
	}

	ww := wordwrap.NewWriter(80)
	fmt.Fprintln(ww)
	fmt.Fprintf(ww, "Breakpoint running until %v (%v).\n", deadline.Format(Stamp), humanize.Time(deadline))
	fmt.Fprintln(ww)
	fmt.Fprintf(ww, "Connect with: ssh -p %s runner@%s\n", port, host)
	_ = ww.Close()

	lines := strings.Split(ww.String(), "\n")

	longestLine := 0
	for _, l := range lines {
		if len(l) > longestLine {
			longestLine = len(l)
		}
	}

	longline := nchars('─', longestLine)
	spaces := nchars(' ', longestLine)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "┌─%s─┐\n", longline)
	for _, l := range lines {
		fmt.Fprintf(os.Stderr, "│ %s%s │\n", l, spaces[len(l):])
	}
	fmt.Fprintf(os.Stderr, "└─%s─┘\n", longline)
	fmt.Fprintln(os.Stderr)
}

func nchars(ch rune, n int) string {
	str := make([]rune, n)
	for k := 0; k < n; k++ {
		str[k] = ch
	}
	return string(str)
}
