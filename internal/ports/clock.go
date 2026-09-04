package ports

import "time"

type Clock interface{ Now() time.Time }

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

type FixedClock struct{ Time time.Time }

func (c *FixedClock) Now() time.Time              { return c.Time.UTC() }
func (c *FixedClock) Set(value time.Time)         { c.Time = value.UTC() }
func (c *FixedClock) Advance(value time.Duration) { c.Time = c.Time.Add(value) }
