// Package adb provides an ADB client that communicates with the ADB server
// over the smart-sockets wire protocol. This file implements ADB reconnection
// with exponential backoff (cenkalti/backoff/v4) and reverse-forward re-issuance.
package adb

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cenkalti/backoff/v4"
)

// ReverseMappingSpec stores the device-side and host-side socket specs for a
// reverse forward mapping. Used to re-issue reverse forwards after adbd restarts,
// when the original ReverseMapping connections have been lost.
type ReverseMappingSpec struct {
	DeviceSpec string // e.g. "localabstract:scrcpy_00001234"
	HostSpec   string // e.g. "tcp:42001"
}

// Reconnector provides ADB server reconnection with exponential backoff.
// After adbd restarts, all existing reverse forward mappings are lost (Pitfall 3).
// The reconnector dials the ADB server repeatedly until it becomes available,
// then callers can re-issue reverse forwards for active sessions.
type Reconnector struct {
	client *Client
}

// NewReconnector creates a Reconnector for the given ADB client.
func NewReconnector(client *Client) *Reconnector {
	return &Reconnector{client: client}
}

// AwaitADBReady attempts to connect to the ADB server with exponential backoff.
// It retries indefinitely (until the context is cancelled) with an initial
// interval of 100ms and a max interval of 5s per ADB-01/FND-01.
// Returns nil once a successful connection is made, or the context error
// if the context is cancelled before ADB becomes available.
func (r *Reconnector) AwaitADBReady(ctx context.Context) error {
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 100 * time.Millisecond
	bo.MaxInterval = 5 * time.Second
	bo.MaxElapsedTime = 0 // retry indefinitely until ctx cancels

	op := func() error {
		// Check context first: if cancelled, stop retrying immediately.
		if ctx.Err() != nil {
			return backoff.Permanent(ctx.Err())
		}
		conn, err := r.client.dial(ctx)
		if err != nil {
			slog.Warn("adb server not available, retrying", "error", err)
			return fmt.Errorf("dial adb: %w", err)
		}
		conn.Close()
		return nil
	}

	if err := backoff.Retry(op, backoff.WithContext(bo, ctx)); err != nil {
		return fmt.Errorf("adb server never became available: %w", err)
	}

	slog.Info("adb server connection restored")
	return nil
}

// ReissueReverseForwards re-establishes reverse forward mappings for active sessions
// after an ADB server restart. For each spec, it calls client.ReverseForward to
// create a new persistent connection that keeps the mapping alive.
// Returns the first error encountered, or nil if all forwards were re-issued.
func (r *Reconnector) ReissueReverseForwards(ctx context.Context, serial string, specs []ReverseMappingSpec) ([]*ReverseMapping, error) {
	var mappings []*ReverseMapping
	for _, spec := range specs {
		rm, err := r.client.ReverseForward(ctx, serial, spec.DeviceSpec, spec.HostSpec)
		if err != nil {
			// Clean up any mappings already created before this failure.
			for _, m := range mappings {
				m.Close()
			}
			return nil, fmt.Errorf("reissue reverse forward %s -> %s: %w", spec.DeviceSpec, spec.HostSpec, err)
		}
		mappings = append(mappings, rm)
		slog.Info("re-issued reverse forward",
			"device", serial,
			"device_spec", spec.DeviceSpec,
			"host_spec", spec.HostSpec,
		)
	}
	return mappings, nil
}