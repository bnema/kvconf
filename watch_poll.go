package kvconf

import "time"

// poller reports every watched directory at each tick; the file stamps tell
// what changed. It works everywhere.
type poller struct {
	every   time.Duration
	targets []*target
	woken   chan struct{}
	timer   *time.Timer
}

func newPoller(every time.Duration) *poller {
	t := time.NewTimer(time.Hour)
	t.Stop()
	return &poller{every: every, woken: make(chan struct{}, 1), timer: t}
}

func (p *poller) set(targets []*target) (bool, error) {
	p.targets = targets
	return false, nil
}

func (p *poller) wait(timeout time.Duration, evs []event) ([]event, error) {
	d := p.every
	if timeout >= 0 && timeout < d {
		d = timeout
	}
	p.timer.Reset(d)
	select {
	case <-p.timer.C:
	case <-p.woken:
		p.timer.Stop()
		return evs, nil
	}
	for _, t := range p.targets {
		evs = append(evs, event{dir: t.dir})
	}
	return evs, nil
}

func (p *poller) wake() {
	select {
	case p.woken <- struct{}{}:
	default:
	}
}

func (p *poller) close() error { return nil }
