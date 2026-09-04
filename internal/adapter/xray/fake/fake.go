package fake

import (
	"context"
	"sync"
	"time"

	"xpanel/internal/ports"
)

type Failure struct {
	Err     error
	Applied bool
}

type Call struct {
	Operation    string
	ProfileTag   string
	StatisticsID string
	Reset        bool
}

type Adapter struct {
	mu        sync.Mutex
	Users     map[string]map[string]ports.RemoteUser
	Counters  map[string]map[ports.Direction]uint64
	Profiles  map[string]ports.ProfileCapabilities
	Failures  map[string][]Failure
	Calls     []Call
	Available bool
	BootEpoch time.Time
	Now       func() time.Time
}

func New() *Adapter {
	now := time.Now().UTC()
	return &Adapter{Users: make(map[string]map[string]ports.RemoteUser), Counters: make(map[string]map[ports.Direction]uint64),
		Profiles: make(map[string]ports.ProfileCapabilities), Failures: make(map[string][]Failure), Available: true,
		BootEpoch: now, Now: func() time.Time { return time.Now().UTC() }}
}

func (a *Adapter) failure(operation string) (Failure, bool) {
	list := a.Failures[operation]
	if len(list) == 0 {
		return Failure{}, false
	}
	result := list[0]
	a.Failures[operation] = list[1:]
	return result, true
}

func (a *Adapter) unavailable(operation string) error {
	if a.Available {
		return nil
	}
	return &ports.AdapterError{Kind: ports.ErrorInstanceUnavailable, Operation: operation, Retryable: true, SafeSummary: "fake Xray unavailable"}
}

func (a *Adapter) Probe(context.Context, ports.InstanceTarget) (ports.InstanceObservation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Calls = append(a.Calls, Call{Operation: "probe"})
	if err := a.unavailable("probe"); err != nil {
		return ports.InstanceObservation{}, err
	}
	now := a.Now()
	return ports.InstanceObservation{ObservedAt: now, BootEpoch: a.BootEpoch, BootEpochKnown: true,
		UptimeSeconds: uint32(now.Sub(a.BootEpoch).Seconds())}, nil
}

func (a *Adapter) ValidateProfile(_ context.Context, profile ports.RuntimeProfile) (ports.ProfileCapabilities, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Calls = append(a.Calls, Call{Operation: "validate_profile", ProfileTag: profile.InboundTag})
	if err := a.unavailable("validate_profile"); err != nil {
		return ports.ProfileCapabilities{}, err
	}
	if failure, ok := a.failure("validate_profile"); ok {
		return ports.ProfileCapabilities{}, failure.Err
	}
	if result, ok := a.Profiles[profile.InboundTag]; ok {
		return result, nil
	}
	return ports.ProfileCapabilities{InboundPresent: true, ProtocolSupported: true, MethodSupported: true,
		MultiUserSupported: true, IndependentStats: true, BootstrapVisible: true}, nil
}

func (a *Adapter) ListUsers(_ context.Context, profile ports.RuntimeProfile) ([]ports.RemoteUser, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Calls = append(a.Calls, Call{Operation: "list_users", ProfileTag: profile.InboundTag})
	if err := a.unavailable("list_users"); err != nil {
		return nil, err
	}
	result := make([]ports.RemoteUser, 0, len(a.Users[profile.InboundTag]))
	for _, user := range a.Users[profile.InboundTag] {
		result = append(result, user)
	}
	return result, nil
}

func (a *Adapter) AddUser(_ context.Context, command ports.AddUserCommand) (ports.MutationReceipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Calls = append(a.Calls, Call{Operation: "add_user", ProfileTag: command.ProfileTag, StatisticsID: command.StatisticsID})
	if err := a.unavailable("add_user"); err != nil {
		return ports.MutationReceipt{}, err
	}
	failure, fails := a.failure("add_user")
	if a.Users[command.ProfileTag] == nil {
		a.Users[command.ProfileTag] = make(map[string]ports.RemoteUser)
	}
	if !fails || failure.Applied {
		a.Users[command.ProfileTag][command.StatisticsID] = ports.RemoteUser{StatisticsID: command.StatisticsID,
			CredentialVersion: command.CredentialVersion, Present: true, Kind: "managed"}
	}
	if fails {
		return ports.MutationReceipt{}, failure.Err
	}
	return ports.MutationReceipt{OperationID: command.OperationID, ObservedAt: a.Now()}, nil
}

func (a *Adapter) RemoveUser(_ context.Context, command ports.RemoveUserCommand) (ports.MutationReceipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Calls = append(a.Calls, Call{Operation: "remove_user", ProfileTag: command.ProfileTag, StatisticsID: command.StatisticsID})
	if err := a.unavailable("remove_user"); err != nil {
		return ports.MutationReceipt{}, err
	}
	failure, fails := a.failure("remove_user")
	if !fails || failure.Applied {
		delete(a.Users[command.ProfileTag], command.StatisticsID)
	}
	if fails {
		return ports.MutationReceipt{}, failure.Err
	}
	return ports.MutationReceipt{OperationID: command.OperationID, ObservedAt: a.Now()}, nil
}

func (a *Adapter) ReadTraffic(_ context.Context, query ports.TrafficQuery) (ports.TrafficRound, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Calls = append(a.Calls, Call{Operation: "read_traffic"})
	if err := a.unavailable("read_traffic"); err != nil {
		return ports.TrafficRound{}, err
	}
	now := a.Now()
	result := ports.TrafficRound{Observation: ports.InstanceObservation{ObservedAt: now, BootEpoch: a.BootEpoch, BootEpochKnown: true}}
	for _, id := range query.StatisticsIDs {
		for _, direction := range []ports.Direction{ports.Uplink, ports.Downlink} {
			value, found := a.Counters[id][direction]
			result.Snapshots = append(result.Snapshots, ports.CounterSnapshot{StatisticsID: id, Direction: direction,
				Bytes: value, ObservedAt: now, Found: found})
		}
	}
	return result, nil
}

func (a *Adapter) Restart() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for profile, users := range a.Users {
		for id, user := range users {
			if user.Kind == "managed" {
				delete(a.Users[profile], id)
			}
		}
	}
	a.Counters = make(map[string]map[ports.Direction]uint64)
	a.BootEpoch = a.Now()
}

func (a *Adapter) SetCounter(id string, direction ports.Direction, value uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Counters[id] == nil {
		a.Counters[id] = make(map[ports.Direction]uint64)
	}
	a.Counters[id][direction] = value
}
