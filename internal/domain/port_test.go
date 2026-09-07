package domain

import (
	"errors"
	"testing"
	"time"
)

func nowPointer() *time.Time { value := time.Unix(0, 0).UTC(); return &value }

func TestNewPortPoolBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		start, end int
		wantErr    string
	}{
		{name: "常规区间", start: 20000, end: 20100},
		{name: "单端口池", start: 20000, end: 20000},
		{name: "下界", start: 1024, end: 1024},
		{name: "上界", start: 65535, end: 65535},
		{name: "低于下界", start: 1023, end: 2000, wantErr: "port_pool_start"},
		{name: "超出上界", start: 2000, end: 65536, wantErr: "port_pool_end"},
		{name: "区间倒置", start: 3000, end: 2999, wantErr: "port_pool_end"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pool, err := NewPortPool(test.start, test.end)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if pool.Capacity() != test.end-test.start+1 {
					t.Fatalf("capacity = %d", pool.Capacity())
				}
				return
			}
			var invalid *ValidationError
			if !errors.As(err, &invalid) || invalid.Field != test.wantErr {
				t.Fatalf("err = %v, want field %s", err, test.wantErr)
			}
		})
	}
}

func TestNextAvailablePortIsDeterministicAndFillsHoles(t *testing.T) {
	pool, _ := NewPortPool(20000, 20004)
	tests := []struct {
		name     string
		assigned []int
		want     int
	}{
		{name: "空池取下界", assigned: nil, want: 20000},
		{name: "顺序占用后取下一个", assigned: []int{20000, 20001}, want: 20002},
		{name: "优先填补空洞", assigned: []int{20000, 20002, 20003}, want: 20001},
		{name: "忽略池外占用", assigned: []int{19999, 30000}, want: 20000},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for i := 0; i < 3; i++ { // 同一输入必须得到同一结果
				port, err := NextAvailablePort(pool, test.assigned)
				if err != nil || port != test.want {
					t.Fatalf("NextAvailablePort = %d, %v; want %d", port, err, test.want)
				}
			}
		})
	}
}

func TestNextAvailablePortExhausted(t *testing.T) {
	pool, _ := NewPortPool(20000, 20002)
	_, err := NextAvailablePort(pool, []int{20000, 20001, 20002})
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want *ConflictError", err)
	}
}

func TestValidateRequestedPort(t *testing.T) {
	pool, _ := NewPortPool(20000, 20010)
	if err := ValidateRequestedPort(pool, 20005, []int{20000}); err != nil {
		t.Fatalf("free in-pool port rejected: %v", err)
	}
	var invalid *ValidationError
	if err := ValidateRequestedPort(pool, 19999, nil); !errors.As(err, &invalid) {
		t.Fatalf("out-of-pool err = %v", err)
	}
	var conflict *ConflictError
	if err := ValidateRequestedPort(pool, 20000, []int{20000}); !errors.As(err, &conflict) {
		t.Fatalf("taken port err = %v", err)
	}
}

func TestOutsidePoolPorts(t *testing.T) {
	pool, _ := NewPortPool(20000, 20010)
	outside := OutsidePoolPorts(pool, []int{20005, 30000, 19999, 20010})
	if len(outside) != 2 || outside[0] != 19999 || outside[1] != 30000 {
		t.Fatalf("outside = %v", outside)
	}
	if OutsidePoolPorts(pool, []int{20000, 20010}) != nil {
		t.Fatal("池内端口不应被标记")
	}
}
