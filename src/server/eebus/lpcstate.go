package eebus

import (
	"sync"
	"time"

	spine_api "github.com/enbility/spine-go/api"
	gin "github.com/gin-gonic/gin"

	model "github.com/tumbleowlee/eebus-go-rest/server/model"
)

// LPCState is the operating state of a remote Controllable System in the EEBUS
// LPC use case, as observed from this CEM (Energy Guard).
//
// The CEM side of the use case has no direct readback of the CS state machine,
// so the state is derived from the two things we can observe: the load control
// limit the CS reports (scenario 1) and whether the CS is still reachable.
// A CS that loses its Energy Guard falls back to its failsafe limit and has to
// hold it for at least FailsafeDurationMinimum (scenario 2) before it is free
// to act on its own again.
type LPCState string

const (
	// Not under our control, and the use case gives the Energy Guard no way to
	// tell which of the two it is: a CS that has not yet been limited looks
	// exactly like one that has outlasted its failsafe window and gone
	// autonomous. Reported as the one state rather than guessing between them.
	LPCStateInitOrAutonomous LPCState = "init_or_autonomous"

	// Reachable, no limit active: the CS consumes freely but under our control.
	LPCStateUnlimitedControlled LPCState = "unlimited_controlled"

	// An active limit that expires by itself once its duration runs out.
	LPCStateLimitedWithDuration LPCState = "limited_with_duration"

	// An active limit that holds until we change it.
	LPCStateLimitedWithoutDuration LPCState = "limited_without_duration"

	// Unreachable: the CS fell back to its failsafe limit.
	LPCStateFailsafe LPCState = "failsafe"
)

// Failsafe duration assumed while the CS has not reported its own minimum.
// The LPC use case allows 2h to 24h, and 2h is the floor.
const defaultFailsafeDuration = 2 * time.Hour

// How often derived states are re-evaluated, so that a limit running out of
// duration or a failsafe period elapsing is noticed without another event.
const lpcStateTickInterval = time.Second

// lpcDevice is the observed LPC data of one remote CS, from which its state is
// derived. All access goes through lpcStateTracker.mu.
type lpcDevice struct {
	connected      bool
	disconnectedAt time.Time

	// hasLimit reports whether the CS has sent load control limit data at all,
	// which is what the limit countdown is anchored to.
	hasLimit bool

	// wroteLimit reports whether this CEM has handed the CS a limit. A
	// Controllable System leaves its init state once its Energy Guard limits
	// it, and that handover is the only part of it we can observe. It stays
	// set across a reconnect, so a brief network drop does not read as the
	// device having powered back up into init.
	wroteLimit     bool
	limitActive    bool
	limitValue     float64
	limitDuration  time.Duration
	limitStartedAt time.Time

	failsafeValue    float64
	failsafeDuration time.Duration

	// published is the last state broadcast to clients, so that the periodic
	// re-evaluation only emits on an actual change.
	published LPCState
}

type lpcStateTracker struct {
	mu      sync.Mutex
	devices map[string]*lpcDevice
}

func newLPCStateTracker() *lpcStateTracker {
	return &lpcStateTracker{devices: make(map[string]*lpcDevice)}
}

// device returns the tracked CS for ski, creating it on first sight.
// The caller must hold t.mu.
func (t *lpcStateTracker) device(ski string) *lpcDevice {
	d, ok := t.devices[ski]
	if !ok {
		d = &lpcDevice{published: LPCStateInitOrAutonomous}
		t.devices[ski] = d
	}
	return d
}

// limitExpired reports whether a limit with a duration has already run out.
func (d *lpcDevice) limitExpired(now time.Time) bool {
	if d.limitDuration <= 0 || d.limitStartedAt.IsZero() {
		return false
	}
	return now.Sub(d.limitStartedAt) >= d.limitDuration
}

// failsafeWindow is the minimum time the CS has to stay in failsafe, falling
// back to the use case floor while the CS has not reported its own value.
func (d *lpcDevice) failsafeWindow() time.Duration {
	if d.failsafeDuration <= 0 {
		return defaultFailsafeDuration
	}
	return d.failsafeDuration
}

// state derives the CS state machine position from the observed data.
func (d *lpcDevice) state(now time.Time) LPCState {
	if !d.connected {
		// Out of contact, but only within the failsafe window can we say the CS
		// is holding its failsafe limit. Before it (never seen) and after it
		// (gone autonomous) are indistinguishable from here.
		if !d.disconnectedAt.IsZero() && now.Sub(d.disconnectedAt) < d.failsafeWindow() {
			return LPCStateFailsafe
		}
		return LPCStateInitOrAutonomous
	}

	// A limit in force is the clearest signal there is, whoever set it, so it
	// outranks the inference below and survives a restart of this CEM.
	if d.limitActive && !d.limitExpired(now) {
		if d.limitDuration > 0 {
			return LPCStateLimitedWithDuration
		}
		return LPCStateLimitedWithoutDuration
	}

	// Reachable and unlimited. Until we have handed it a limit we have not
	// taken control, so it is still either in init or acting on its own.
	if !d.wroteLimit {
		return LPCStateInitOrAutonomous
	}

	return LPCStateUnlimitedControlled
}

// remaining is the time left in the current state, for states that end on
// their own. It is zero for states that only change on an event.
func (d *lpcDevice) remaining(now time.Time, state LPCState) time.Duration {
	var left time.Duration

	switch state {
	case LPCStateLimitedWithDuration:
		left = d.limitDuration - now.Sub(d.limitStartedAt)
	case LPCStateFailsafe:
		left = d.failsafeWindow() - now.Sub(d.disconnectedAt)
	default:
		return 0
	}

	if left < 0 {
		return 0
	}
	return left
}

// payload is the wire form of one CS state, shared by the broadcast and the
// snapshot a client asks for after (re)connecting.
func (d *lpcDevice) payload(ski string, now time.Time) gin.H {
	state := d.state(now)

	return gin.H{
		"ski":               ski,
		"state":             string(state),
		"connected":         d.connected,
		"limit":             d.limitValue,
		"limit_active":      d.limitActive,
		"duration":          d.limitDuration.Seconds(),
		"remaining":         d.remaining(now, state).Seconds(),
		"failsafe_value":    d.failsafeValue,
		"failsafe_duration": d.failsafeDuration.Seconds(),
	}
}

// startLPCStateTracking re-evaluates every tracked CS on a ticker so that
// time-driven transitions — a limit reaching the end of its duration, a
// failsafe period elapsing into autonomous — are broadcast as they happen.
func (r *Runtime) startLPCStateTracking() {
	go func() {
		ticker := time.NewTicker(lpcStateTickInterval)
		defer ticker.Stop()

		for range ticker.C {
			r.publishChangedLPCStates()
		}
	}()
}

// publishChangedLPCStates broadcasts every CS whose derived state moved on.
func (r *Runtime) publishChangedLPCStates() {
	now := time.Now()

	r.lpcStates.mu.Lock()
	var changed []gin.H
	for ski, d := range r.lpcStates.devices {
		state := d.state(now)
		if state == d.published {
			continue
		}
		d.published = state
		changed = append(changed, d.payload(ski, now))
	}
	r.lpcStates.mu.Unlock()

	for _, p := range changed {
		r.Infof("LPC state of %v is now %v", p["ski"], p["state"])
		r.Hub.SendMessage(model.Message{Type: "lpc_state_update", Data: p})
	}
}

// publishLPCState broadcasts one CS unconditionally, so that an event-driven
// update reaches clients even when the derived state itself did not move
// (a new limit value under an already active limit, for instance).
func (r *Runtime) publishLPCState(ski string) {
	now := time.Now()

	r.lpcStates.mu.Lock()
	d, ok := r.lpcStates.devices[ski]
	if !ok {
		r.lpcStates.mu.Unlock()
		return
	}
	d.published = d.state(now)
	payload := d.payload(ski, now)
	r.lpcStates.mu.Unlock()

	r.Infof("LPC state of %s is now %v", ski, payload["state"])
	r.Hub.SendMessage(model.Message{Type: "lpc_state_update", Data: payload})
}

// LPCStates returns the current state of every tracked CS, for a client that
// has just (re)connected and needs to catch up.
func (r *Runtime) LPCStates() []gin.H {
	now := time.Now()

	r.lpcStates.mu.Lock()
	defer r.lpcStates.mu.Unlock()

	states := make([]gin.H, 0, len(r.lpcStates.devices))
	for ski, d := range r.lpcStates.devices {
		states = append(states, d.payload(ski, now))
	}
	return states
}

// SetLPCConnected records whether a CS is reachable. Losing a CS starts its
// failsafe window; getting it back clears it.
func (r *Runtime) SetLPCConnected(ski string, connected bool) {
	r.lpcStates.mu.Lock()
	d := r.lpcStates.device(ski)
	if d.connected == connected {
		r.lpcStates.mu.Unlock()
		return
	}

	d.connected = connected
	if connected {
		d.disconnectedAt = time.Time{}
	} else {
		d.disconnectedAt = time.Now()
	}
	r.lpcStates.mu.Unlock()

	r.publishLPCState(ski)
}

// MarkLPCLimitWritten records that this CEM has handed the CS a limit, which
// is what takes a Controllable System out of its init state. Deactivating a
// limit counts too: it is still us taking control of the device.
func (r *Runtime) MarkLPCLimitWritten(ski string) {
	r.lpcStates.mu.Lock()
	d := r.lpcStates.device(ski)
	if d.wroteLimit {
		r.lpcStates.mu.Unlock()
		return
	}
	d.wroteLimit = true
	r.lpcStates.mu.Unlock()

	r.publishLPCState(ski)
}

// RefreshLPCState re-reads the LPC data of one CS and broadcasts the state it
// implies. Called whenever the CS reports new limit or failsafe data, and
// after we write to it ourselves.
func (r *Runtime) RefreshLPCState(ski string, entity spine_api.EntityRemoteInterface) {
	if r.eg_lpc == nil || entity == nil {
		return
	}

	r.lpcStates.mu.Lock()
	d := r.lpcStates.device(ski)
	d.connected = true
	d.disconnectedAt = time.Time{}

	if limit, err := r.eg_lpc.ConsumptionLimit(entity); err == nil {
		// Restart the countdown only when the limit itself changed, so that a
		// repeated report of the same limit does not extend it.
		if !d.hasLimit || d.limitActive != limit.IsActive ||
			d.limitValue != limit.Value || d.limitDuration != limit.Duration {
			d.limitStartedAt = time.Now()
		}
		d.hasLimit = true
		d.limitActive = limit.IsActive
		d.limitValue = limit.Value
		d.limitDuration = limit.Duration
	} else {
		r.Debugf("No consumption limit available for %s yet: %v", ski, err)
	}

	if value, err := r.eg_lpc.FailsafeConsumptionActivePowerLimit(entity); err == nil {
		d.failsafeValue = value
	}

	if duration, err := r.eg_lpc.FailsafeDurationMinimum(entity); err == nil {
		d.failsafeDuration = duration
	}
	r.lpcStates.mu.Unlock()

	r.publishLPCState(ski)
}

// ForgetLPCState drops a CS that is no longer on the grid.
func (r *Runtime) ForgetLPCState(ski string) {
	r.lpcStates.mu.Lock()
	delete(r.lpcStates.devices, ski)
	r.lpcStates.mu.Unlock()
}

// remoteLPCEntity resolves the LPC-capable entity of a paired CS.
func (r *Runtime) remoteLPCEntity(ski string) spine_api.EntityRemoteInterface {
	if r.eg_lpc == nil {
		return nil
	}

	remoteDevice := r.service.LocalDevice().RemoteDeviceForSki(ski)
	if remoteDevice == nil {
		return nil
	}

	for _, entity := range remoteDevice.Entities() {
		if r.eg_lpc.IsCompatibleEntityType(entity) {
			return entity
		}
	}
	return nil
}
