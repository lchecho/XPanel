package xray

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	handlercommand "github.com/xtls/xray-core/app/proxyman/command"
	statscommand "github.com/xtls/xray-core/app/stats/command"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"xpanel/internal/ports"
)

type trafficStatsStub struct {
	statscommand.UnimplementedStatsServiceServer
	mu        sync.Mutex
	values    map[string]int64
	malformed map[string]bool
	requests  []*statscommand.GetStatsRequest
	sysErr    error
}

func (s *trafficStatsStub) GetSysStats(context.Context, *statscommand.SysStatsRequest) (*statscommand.SysStatsResponse, error) {
	if s.sysErr != nil {
		return nil, s.sysErr
	}
	return &statscommand.SysStatsResponse{Uptime: 120}, nil
}

func (s *trafficStatsStub) GetStats(_ context.Context, request *statscommand.GetStatsRequest) (*statscommand.GetStatsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request)
	if s.malformed[request.Name] {
		return &statscommand.GetStatsResponse{Stat: &statscommand.Stat{Name: "user>>>other>>>traffic>>>uplink", Value: 1}}, nil
	}
	value, ok := s.values[request.Name]
	if !ok {
		return nil, status.Error(codes.NotFound, request.Name+" not found")
	}
	return &statscommand.GetStatsResponse{Stat: &statscommand.Stat{Name: request.Name, Value: value}}, nil
}

func newTrafficClient(t *testing.T, stats *trafficStatsStub) *Client {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	handlercommand.RegisterHandlerServiceServer(server, &handlerStub{})
	statscommand.RegisterStatsServiceServer(server, stats)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return NewWithConnection(conn, time.Second)
}

func TestReadTrafficUsesExactNamesWithoutReset(t *testing.T) {
	stats := &trafficStatsStub{values: map[string]int64{
		"user>>>xpanel-a>>>traffic>>>uplink":   1500,
		"user>>>xpanel-a>>>traffic>>>downlink": 0,
	}}
	client := newTrafficClient(t, stats)
	fixed := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	client.now = func() time.Time { return fixed }
	round, err := client.ReadTraffic(context.Background(), ports.TrafficQuery{StatisticsIDs: []string{"xpanel-a", "xpanel-b"}})
	if err != nil {
		t.Fatal(err)
	}
	if !round.Observation.BootEpochKnown || !round.Observation.BootEpoch.Equal(fixed.Add(-120*time.Second)) {
		t.Fatalf("observation = %#v", round.Observation)
	}
	if len(round.Snapshots) != 4 {
		t.Fatalf("snapshots = %#v", round.Snapshots)
	}
	byName := map[string]ports.CounterSnapshot{}
	for _, snapshot := range round.Snapshots {
		byName[snapshot.StatisticsID+"/"+string(snapshot.Direction)] = snapshot
	}
	if s := byName["xpanel-a/uplink"]; !s.Found || s.Bytes != 1500 {
		t.Fatalf("uplink snapshot = %#v", s)
	}
	if s := byName["xpanel-a/downlink"]; !s.Found || s.Bytes != 0 {
		t.Fatalf("zero counter must be found: %#v", s)
	}
	if s := byName["xpanel-b/uplink"]; s.Found {
		t.Fatalf("missing counter must not be found: %#v", s)
	}
	for _, request := range stats.requests {
		if request.GetReset_() {
			t.Fatalf("reset requested: %#v", request)
		}
		if _, _, err := parseCounter(request.GetName()); err != nil {
			t.Fatalf("non-exact counter name %q", request.GetName())
		}
	}
	if len(stats.requests) != 4 {
		t.Fatalf("requests = %d", len(stats.requests))
	}
}

func parseCounter(name string) (string, string, error) {
	var id, direction string
	for _, candidate := range []string{"uplink", "downlink"} {
		prefix := "user>>>"
		suffix := ">>>traffic>>>" + candidate
		if len(name) > len(prefix)+len(suffix) && name[:len(prefix)] == prefix && name[len(name)-len(suffix):] == suffix {
			id, direction = name[len(prefix):len(name)-len(suffix)], candidate
			return id, direction, nil
		}
	}
	return "", "", &ports.AdapterError{Kind: ports.ErrorInvalidArgument}
}

func TestReadTrafficRejectsMalformedAndTooMany(t *testing.T) {
	stats := &trafficStatsStub{values: map[string]int64{}, malformed: map[string]bool{"user>>>xpanel-a>>>traffic>>>uplink": true}}
	client := newTrafficClient(t, stats)
	_, err := client.ReadTraffic(context.Background(), ports.TrafficQuery{StatisticsIDs: []string{"xpanel-a"}})
	adapterErr, ok := err.(*ports.AdapterError)
	if !ok || adapterErr.Kind != ports.ErrorInternal || !adapterErr.Retryable {
		t.Fatalf("malformed mapping = %T %v", err, err)
	}
	ids := make([]string, 21)
	for i := range ids {
		ids[i] = "xpanel-x"
	}
	if _, err := client.ReadTraffic(context.Background(), ports.TrafficQuery{StatisticsIDs: ids}); err == nil {
		t.Fatal("more than 20 identities accepted")
	}
}

func TestReadTrafficWithoutSysStatsMarksEpochUnknown(t *testing.T) {
	stats := &trafficStatsStub{values: map[string]int64{"user>>>xpanel-a>>>traffic>>>uplink": 5}, sysErr: status.Error(codes.Unavailable, "down")}
	client := newTrafficClient(t, stats)
	round, err := client.ReadTraffic(context.Background(), ports.TrafficQuery{StatisticsIDs: []string{"xpanel-a"}})
	if err != nil || round.Observation.BootEpochKnown {
		t.Fatalf("round = %#v, %v", round.Observation, err)
	}
}
