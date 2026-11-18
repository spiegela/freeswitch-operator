package runner

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/netem"
	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/scenario"
)

// impairments are a scenario's tc impairments in place on the runner's interface. The SIP layer's (drop, match)
// are applied per call instead (see Runner.tracedDiago).
type impairments struct {
	sh      *netem.Shaper
	plan    netem.Plan
	ims     []*netem.Impairment
	wg      sync.WaitGroup
	cancel  context.CancelFunc
	removed sync.Once
}

// impair builds the tc qdiscs and filters for s's tc impairments and turns on those without at. Call remove when
// the scenario ends, whatever happened. It returns nil when s has no tc impairments.
func (r *Runner) impair(ctx context.Context, s *scenario.Scenario) (*impairments, error) {
	var ims []*netem.Impairment
	for _, im := range s.Impaired {
		if !im.SIPLayer() {
			ims = append(ims, im)
		}
	}
	if len(ims) == 0 {
		return nil, nil
	}
	iface, _, err := netem.IfaceToward(r.cfg.Edge)
	if err != nil {
		return nil, err
	}
	_, portS, err := net.SplitHostPort(r.cfg.Edge)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portS)
	if err != nil {
		return nil, fmt.Errorf("edge port %q: %w", portS, err)
	}
	sh := &netem.Shaper{Iface: iface, SIPPort: port, Exec: r.cfg.TCExec}
	plan, err := sh.Plan(ims)
	if err != nil {
		return nil, err
	}
	x := &impairments{sh: sh, plan: plan, ims: ims, cancel: func() {}}
	sh.RunAll(ctx, plan.Teardown) // whatever an interrupted run left behind
	if err := sh.Run(ctx, plan.Setup); err != nil {
		x.remove()
		return nil, err
	}
	for i, im := range ims {
		if im.HasAt {
			continue
		}
		if err := sh.Run(ctx, plan.On[i]); err != nil {
			x.remove()
			return nil, err
		}
	}
	return x, nil
}

// schedule turns the impairments with at on that long after the call first answers, and off again after for.
// It gives up if the call ends unanswered.
func (x *impairments) schedule(ctx context.Context, first *callState, tl *timeline) {
	ctx, x.cancel = context.WithCancel(ctx)
	for i, im := range x.ims {
		if !im.HasAt {
			continue
		}
		x.wg.Go(func() {
			select {
			case <-first.answered:
			case <-first.ended:
				return
			case <-ctx.Done():
				return
			}
			if !sleep(ctx, im.At) {
				return
			}
			if err := x.sh.Run(ctx, x.plan.On[i]); err != nil {
				tl.fail("impairment %s: %v", im, err)
				return
			}
			tl.note("impairment on: %s", im)
			if !im.HasFor || !sleep(ctx, im.For) {
				return
			}
			if err := x.sh.Run(ctx, x.plan.Off[i]); err != nil {
				tl.fail("impairment %s: %v", im, err)
				return
			}
			tl.note("impairment off: %s", im)
		})
	}
}

// remove stops the schedule and takes every qdisc, filter and device away. Only the first call does anything.
func (x *impairments) remove() {
	x.removed.Do(func() {
		x.cancel()
		x.wg.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		x.sh.RunAll(ctx, x.plan.Teardown)
	})
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}
