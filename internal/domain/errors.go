package domain

import (
	"fmt"
	"time"
)

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return fmt.Sprintf("%s: %s", e.Field, e.Message) }

type ConflictError struct{ Message string }

func (e *ConflictError) Error() string { return e.Message }

type NotFoundError struct{ Resource string }

func (e *NotFoundError) Error() string { return e.Resource + " not found" }

type InvalidStateError struct{ Message string }

func (e *InvalidStateError) Error() string { return e.Message }

type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string { return "too many authentication attempts" }
