package xray

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	handlercommand "github.com/xtls/xray-core/app/proxyman/command"
	statscommand "github.com/xtls/xray-core/app/stats/command"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"xpanel/internal/ports"
)

type Client struct {
	conn    *grpc.ClientConn
	handler handlercommand.HandlerServiceClient
	stats   statscommand.StatsServiceClient
	timeout time.Duration
	now     func() time.Time
}

func New(target ports.InstanceTarget) (*Client, error) {
	host, _, err := net.SplitHostPort(target.APIEndpoint)
	if err != nil {
		return nil, &ports.AdapterError{Kind: ports.ErrorInvalidArgument, Operation: "connect", SafeSummary: "invalid Xray API endpoint"}
	}
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return nil, &ports.AdapterError{Kind: ports.ErrorInvalidArgument, Operation: "connect", SafeSummary: "Xray API must be loopback"}
	}
	timeout := target.RPCTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	conn, err := grpc.NewClient(target.APIEndpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, mapError("connect", err)
	}
	return NewWithConnection(conn, timeout), nil
}

func NewWithConnection(conn *grpc.ClientConn, timeout time.Duration) *Client {
	return &Client{conn: conn, handler: handlercommand.NewHandlerServiceClient(conn),
		stats: statscommand.NewStatsServiceClient(conn), timeout: timeout, now: func() time.Time { return time.Now().UTC() }}
}

func (c *Client) Close() error {
	if c.conn == nil {
		return errors.New("Xray client has no connection")
	}
	return c.conn.Close()
}

func (c *Client) deadline(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.timeout)
}
