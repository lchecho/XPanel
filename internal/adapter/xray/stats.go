package xray

import (
	"context"
	"fmt"

	statscommand "github.com/xtls/xray-core/app/stats/command"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"xpanel/internal/ports"
)

func CounterName(statisticsID string, direction ports.Direction) (string, error) {
	if statisticsID == "" || direction != ports.Uplink && direction != ports.Downlink {
		return "", &ports.AdapterError{Kind: ports.ErrorInvalidArgument, Operation: "counter_name", SafeSummary: "invalid counter identity or direction"}
	}
	return fmt.Sprintf("user>>>%s>>>traffic>>>%s", statisticsID, direction), nil
}

func (c *Client) ReadTraffic(ctx context.Context, query ports.TrafficQuery) (ports.TrafficRound, error) {
	if len(query.StatisticsIDs) > 20 {
		return ports.TrafficRound{}, &ports.AdapterError{Kind: ports.ErrorInvalidArgument, Operation: "read_traffic", SafeSummary: "traffic query exceeds supported allocation count"}
	}
	observation, probeErr := c.Probe(ctx, query.Target)
	if probeErr != nil {
		observation = ports.InstanceObservation{ObservedAt: c.now().UTC(), BootEpochKnown: false}
	}
	snapshots := make([]ports.CounterSnapshot, 0, len(query.StatisticsIDs)*2)
	for _, identity := range query.StatisticsIDs {
		for _, direction := range []ports.Direction{ports.Uplink, ports.Downlink} {
			name, err := CounterName(identity, direction)
			if err != nil {
				return ports.TrafficRound{}, err
			}
			callCtx, cancel := c.deadline(ctx)
			response, err := c.stats.GetStats(callCtx, &statscommand.GetStatsRequest{Name: name, Reset_: false})
			cancel()
			if status.Code(err) == codes.NotFound {
				snapshots = append(snapshots, ports.CounterSnapshot{StatisticsID: identity, Direction: direction,
					ObservedAt: observation.ObservedAt, Found: false})
				continue
			}
			if err != nil {
				return ports.TrafficRound{}, mapError("read_traffic", err)
			}
			stat := response.GetStat()
			if stat == nil || stat.GetName() != name || stat.GetValue() < 0 {
				return ports.TrafficRound{}, &ports.AdapterError{Kind: ports.ErrorInternal, Operation: "read_traffic", Retryable: true, SafeSummary: "malformed Xray statistics response"}
			}
			snapshots = append(snapshots, ports.CounterSnapshot{StatisticsID: identity, Direction: direction,
				Bytes: uint64(stat.GetValue()), ObservedAt: observation.ObservedAt, Found: true})
		}
	}
	return ports.TrafficRound{Observation: observation, Snapshots: snapshots}, nil
}
