// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package m3ua

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestNewHandler(t *testing.T) {
	logger := zap.NewNop()

	tests := []struct {
		name    string
		config  *Config
		wantErr bool
		errMsg  string
	}{
		{
			name:    "nil config",
			config:  nil,
			wantErr: true,
			errMsg:  "config cannot be nil",
		},
		{
			name: "empty SGW address",
			config: &Config{
				SGWAddress: "",
				SGWPort:    2905,
			},
			wantErr: true,
			errMsg:  "sgw_address is required",
		},
		{
			name: "invalid port - too low",
			config: &Config{
				SGWAddress: "10.0.1.100",
				SGWPort:    0,
			},
			wantErr: true,
			errMsg:  "sgw_port must be between 1 and 65535",
		},
		{
			name: "invalid port - too high",
			config: &Config{
				SGWAddress: "10.0.1.100",
				SGWPort:    70000,
			},
			wantErr: true,
			errMsg:  "sgw_port must be between 1 and 65535",
		},
		{
			name: "invalid traffic mode",
			config: &Config{
				SGWAddress:  "10.0.1.100",
				SGWPort:     2905,
				TrafficMode: 5,
			},
			wantErr: true,
			errMsg:  "traffic_mode must be 1 (Override) or 2 (Loadshare)",
		},
		{
			name: "multi-homing with insufficient IPs",
			config: &Config{
				SGWAddress:         "10.0.1.100",
				SGWPort:            2905,
				TrafficMode:        1,
				MultiHomingEnabled: true,
				LocalIPs:           []string{"10.0.2.10"},
			},
			wantErr: true,
			errMsg:  "multi-homing requires at least 2 local IPs",
		},
		{
			name: "invalid ASPUP ACK timeout",
			config: &Config{
				SGWAddress:      "10.0.1.100",
				SGWPort:         2905,
				TrafficMode:     1,
				LocalIPs:        []string{"10.0.2.10"},
				ASPUPACKTimeout: 500 * time.Millisecond,
			},
			wantErr: true,
			errMsg:  "aspup_ack_timeout must be at least 1 second",
		},
		{
			name: "valid single-homed config",
			config: &Config{
				SGWAddress:        "10.0.1.100",
				SGWPort:           2905,
				ASPIdentifier:     1,
				RoutingContext:    1,
				NetworkAppearance: 0,
				TrafficMode:       1,
				LocalIPs:          []string{"10.0.2.10"},
				HeartbeatInterval: 30 * time.Second,
				PathMaxRetrans:    5,
				RTOMax:            2 * time.Second,
				RTOInitial:        1 * time.Second,
				RTOMin:            500 * time.Millisecond,
				AssocMaxRetrans:   10,
				ASPUPACKTimeout:   5 * time.Second,
				ASPACACKTimeout:   5 * time.Second,
				ReconnectInterval: 5 * time.Second,
			},
			wantErr: false,
		},
		{
			name: "valid multi-homed config",
			config: &Config{
				SGWAddress:         "10.0.1.100",
				SGWPort:            2905,
				ASPIdentifier:      1,
				RoutingContext:     1,
				NetworkAppearance:  0,
				TrafficMode:        2,
				MultiHomingEnabled: true,
				LocalIPs:           []string{"10.0.2.10", "10.0.2.11"},
				HeartbeatInterval:  30 * time.Second,
				PathMaxRetrans:     5,
				RTOMax:             2 * time.Second,
				RTOInitial:         1 * time.Second,
				RTOMin:             500 * time.Millisecond,
				AssocMaxRetrans:    10,
				ASPUPACKTimeout:    5 * time.Second,
				ASPACACKTimeout:    5 * time.Second,
				ReconnectInterval:  5 * time.Second,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, err := NewHandler(tt.config, logger)

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
				assert.Nil(t, handler)
			} else {
				require.NoError(t, err)
				require.NotNil(t, handler)
				assert.Equal(t, ASPStateDown, handler.GetState())
				assert.NotNil(t, handler.incomingCh)
				assert.NotNil(t, handler.outgoingCh)
				assert.NotNil(t, handler.errorCh)
				assert.NotNil(t, handler.shutdownCh)
				assert.NotNil(t, handler.metrics)
			}
		})
	}
}

func TestASPStateString(t *testing.T) {
	tests := []struct {
		state    ASPState
		expected string
	}{
		{ASPStateDown, "DOWN"},
		{ASPStateInactive, "INACTIVE"},
		{ASPStateActive, "ACTIVE"},
		{ASPState(99), "UNKNOWN"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.state.String())
		})
	}
}

func TestHandlerStateManagement(t *testing.T) {
	config := &Config{
		SGWAddress:        "10.0.1.100",
		SGWPort:           2905,
		ASPIdentifier:     1,
		RoutingContext:    1,
		NetworkAppearance: 0,
		TrafficMode:       1,
		LocalIPs:          []string{"10.0.2.10"},
		HeartbeatInterval: 30 * time.Second,
		PathMaxRetrans:    5,
		RTOMax:            2 * time.Second,
		RTOInitial:        1 * time.Second,
		RTOMin:            500 * time.Millisecond,
		AssocMaxRetrans:   10,
		ASPUPACKTimeout:   5 * time.Second,
		ASPACACKTimeout:   5 * time.Second,
		ReconnectInterval: 5 * time.Second,
	}

	logger := zap.NewNop()
	handler, err := NewHandler(config, logger)
	require.NoError(t, err)

	// Test initial state
	assert.Equal(t, ASPStateDown, handler.GetState())

	// Test state transitions
	handler.setState(ASPStateInactive)
	assert.Equal(t, ASPStateInactive, handler.GetState())

	handler.setState(ASPStateActive)
	assert.Equal(t, ASPStateActive, handler.GetState())

	handler.setState(ASPStateDown)
	assert.Equal(t, ASPStateDown, handler.GetState())
}

func TestHandlerStateCallbacks(t *testing.T) {
	config := &Config{
		SGWAddress:        "10.0.1.100",
		SGWPort:           2905,
		ASPIdentifier:     1,
		RoutingContext:    1,
		NetworkAppearance: 0,
		TrafficMode:       1,
		LocalIPs:          []string{"10.0.2.10"},
		HeartbeatInterval: 30 * time.Second,
		PathMaxRetrans:    5,
		RTOMax:            2 * time.Second,
		RTOInitial:        1 * time.Second,
		RTOMin:            500 * time.Millisecond,
		AssocMaxRetrans:   10,
		ASPUPACKTimeout:   5 * time.Second,
		ASPACACKTimeout:   5 * time.Second,
		ReconnectInterval: 5 * time.Second,
	}

	logger := zap.NewNop()
	handler, err := NewHandler(config, logger)
	require.NoError(t, err)

	// Test OnStateChange callback
	var stateChangeCalled bool
	var oldState, newState ASPState
	handler.OnStateChange(func(old, new ASPState) {
		stateChangeCalled = true
		oldState = old
		newState = new
	})

	handler.transitionState(ASPStateDown, ASPStateInactive)
	assert.True(t, stateChangeCalled)
	assert.Equal(t, ASPStateDown, oldState)
	assert.Equal(t, ASPStateInactive, newState)

	// Test OnLinkUp callback
	var linkUpCalled bool
	handler.OnLinkUp(func() {
		linkUpCalled = true
	})

	// Simulate ASPAC ACK (this would normally come from readLoop)
	handler.setState(ASPStateActive)
	handler.metrics.LinkStatus.Store(1)
	if handler.onLinkUp != nil {
		handler.onLinkUp()
	}
	assert.True(t, linkUpCalled)

	// Test OnLinkDown callback
	var linkDownCalled bool
	handler.OnLinkDown(func() {
		linkDownCalled = true
	})

	// Simulate connection error
	handler.setState(ASPStateDown)
	handler.metrics.LinkStatus.Store(0)
	if handler.onLinkDown != nil {
		handler.onLinkDown()
	}
	assert.True(t, linkDownCalled)
}

func TestHandlerIsActive(t *testing.T) {
	config := &Config{
		SGWAddress:        "10.0.1.100",
		SGWPort:           2905,
		ASPIdentifier:     1,
		RoutingContext:    1,
		NetworkAppearance: 0,
		TrafficMode:       1,
		LocalIPs:          []string{"10.0.2.10"},
		HeartbeatInterval: 30 * time.Second,
		PathMaxRetrans:    5,
		RTOMax:            2 * time.Second,
		RTOInitial:        1 * time.Second,
		RTOMin:            500 * time.Millisecond,
		AssocMaxRetrans:   10,
		ASPUPACKTimeout:   5 * time.Second,
		ASPACACKTimeout:   5 * time.Second,
		ReconnectInterval: 5 * time.Second,
	}

	logger := zap.NewNop()
	handler, err := NewHandler(config, logger)
	require.NoError(t, err)

	// Test initial state
	assert.False(t, handler.IsActive())

	// Test inactive state
	handler.setState(ASPStateInactive)
	assert.False(t, handler.IsActive())

	// Test active state
	handler.setState(ASPStateActive)
	assert.True(t, handler.IsActive())
}

func TestHandlerWaitForActive(t *testing.T) {
	config := &Config{
		SGWAddress:        "10.0.1.100",
		SGWPort:           2905,
		ASPIdentifier:     1,
		RoutingContext:    1,
		NetworkAppearance: 0,
		TrafficMode:       1,
		LocalIPs:          []string{"10.0.2.10"},
		HeartbeatInterval: 30 * time.Second,
		PathMaxRetrans:    5,
		RTOMax:            2 * time.Second,
		RTOInitial:        1 * time.Second,
		RTOMin:            500 * time.Millisecond,
		AssocMaxRetrans:   10,
		ASPUPACKTimeout:   5 * time.Second,
		ASPACACKTimeout:   5 * time.Second,
		ReconnectInterval: 5 * time.Second,
	}

	logger := zap.NewNop()
	handler, err := NewHandler(config, logger)
	require.NoError(t, err)

	// Test timeout
	err = handler.WaitForActive(200 * time.Millisecond)
	assert.Equal(t, ErrTimeout, err)

	// Test successful activation
	go func() {
		time.Sleep(100 * time.Millisecond)
		handler.setState(ASPStateActive)
	}()

	err = handler.WaitForActive(500 * time.Millisecond)
	assert.NoError(t, err)
}

func TestHandlerGetHealthStatus(t *testing.T) {
	config := &Config{
		SGWAddress:        "10.0.1.100",
		SGWPort:           2905,
		ASPIdentifier:     1,
		RoutingContext:    1,
		NetworkAppearance: 0,
		TrafficMode:       1,
		LocalIPs:          []string{"10.0.2.10"},
		HeartbeatInterval: 30 * time.Second,
		PathMaxRetrans:    5,
		RTOMax:            2 * time.Second,
		RTOInitial:        1 * time.Second,
		RTOMin:            500 * time.Millisecond,
		AssocMaxRetrans:   10,
		ASPUPACKTimeout:   5 * time.Second,
		ASPACACKTimeout:   5 * time.Second,
		ReconnectInterval: 5 * time.Second,
	}

	logger := zap.NewNop()
	handler, err := NewHandler(config, logger)
	require.NoError(t, err)

	// Test initial health status
	health := handler.GetHealthStatus()
	assert.Equal(t, "DOWN", health.State)
	assert.Equal(t, "DOWN", health.LinkStatus)
	assert.Equal(t, "", health.ActivePath)
	assert.Equal(t, uint64(0), health.MessagesSent)
	assert.Equal(t, uint64(0), health.MessagesRecv)
	assert.Equal(t, uint64(0), health.Errors)

	// Update metrics
	handler.setState(ASPStateActive)
	handler.metrics.LinkStatus.Store(1)
	handler.metrics.MessagesSent.Add(100)
	handler.metrics.MessagesReceived.Add(50)
	handler.metrics.Errors.Add(5)
	handler.metrics.mu.Lock()
	handler.metrics.ActivePath = "10.0.2.10"
	handler.metrics.mu.Unlock()

	// Test updated health status
	health = handler.GetHealthStatus()
	assert.Equal(t, "ACTIVE", health.State)
	assert.Equal(t, "UP", health.LinkStatus)
	assert.Equal(t, "10.0.2.10", health.ActivePath)
	assert.Equal(t, uint64(100), health.MessagesSent)
	assert.Equal(t, uint64(50), health.MessagesRecv)
	assert.Equal(t, uint64(5), health.Errors)
}

func TestMTP3MessageValidation(t *testing.T) {
	config := &Config{
		SGWAddress:        "10.0.1.100",
		SGWPort:           2905,
		ASPIdentifier:     1,
		RoutingContext:    1,
		NetworkAppearance: 0,
		TrafficMode:       1,
		LocalIPs:          []string{"10.0.2.10"},
		HeartbeatInterval: 30 * time.Second,
		PathMaxRetrans:    5,
		RTOMax:            2 * time.Second,
		RTOInitial:        1 * time.Second,
		RTOMin:            500 * time.Millisecond,
		AssocMaxRetrans:   10,
		ASPUPACKTimeout:   5 * time.Second,
		ASPACACKTimeout:   5 * time.Second,
		ReconnectInterval: 5 * time.Second,
	}

	logger := zap.NewNop()
	handler, err := NewHandler(config, logger)
	require.NoError(t, err)

	// Test message too large
	largePayload := make([]byte, MaxM3UAMessageSize+1)
	mtp3Msg := &MTP3Message{
		OPC:         1,
		DPC:         2,
		SI:          3,
		NI:          2,
		MP:          0,
		SLS:         1,
		PayloadData: largePayload,
	}

	// This should fail because link is not active
	err = handler.sendMTP3(mtp3Msg)
	assert.Equal(t, ErrNotActive, err)

	// Set link to active
	handler.setState(ASPStateActive)

	// This should fail because message is too large
	err = handler.sendMTP3(mtp3Msg)
	assert.Equal(t, ErrMessageTooLarge, err)
}

func TestHandlerChannels(t *testing.T) {
	config := &Config{
		SGWAddress:        "10.0.1.100",
		SGWPort:           2905,
		ASPIdentifier:     1,
		RoutingContext:    1,
		NetworkAppearance: 0,
		TrafficMode:       1,
		LocalIPs:          []string{"10.0.2.10"},
		HeartbeatInterval: 30 * time.Second,
		PathMaxRetrans:    5,
		RTOMax:            2 * time.Second,
		RTOInitial:        1 * time.Second,
		RTOMin:            500 * time.Millisecond,
		AssocMaxRetrans:   10,
		ASPUPACKTimeout:   5 * time.Second,
		ASPACACKTimeout:   5 * time.Second,
		ReconnectInterval: 5 * time.Second,
	}

	logger := zap.NewNop()
	handler, err := NewHandler(config, logger)
	require.NoError(t, err)

	// Test IncomingMessages channel
	incomingCh := handler.IncomingMessages()
	assert.NotNil(t, incomingCh)
	assert.Equal(t, cap(incomingCh), ChannelBufferSize)

	// Test OutgoingMessages channel
	outgoingCh := handler.OutgoingMessages()
	assert.NotNil(t, outgoingCh)
	assert.Equal(t, cap(outgoingCh), ChannelBufferSize)

	// Test Errors channel
	errorCh := handler.Errors()
	assert.NotNil(t, errorCh)
	assert.Equal(t, cap(errorCh), 10)
}

func TestHandlerMetrics(t *testing.T) {
	config := &Config{
		SGWAddress:        "10.0.1.100",
		SGWPort:           2905,
		ASPIdentifier:     1,
		RoutingContext:    1,
		NetworkAppearance: 0,
		TrafficMode:       1,
		LocalIPs:          []string{"10.0.2.10"},
		HeartbeatInterval: 30 * time.Second,
		PathMaxRetrans:    5,
		RTOMax:            2 * time.Second,
		RTOInitial:        1 * time.Second,
		RTOMin:            500 * time.Millisecond,
		AssocMaxRetrans:   10,
		ASPUPACKTimeout:   5 * time.Second,
		ASPACACKTimeout:   5 * time.Second,
		ReconnectInterval: 5 * time.Second,
	}

	logger := zap.NewNop()
	handler, err := NewHandler(config, logger)
	require.NoError(t, err)

	// Test initial metrics
	metrics := handler.GetMetrics()
	assert.NotNil(t, metrics)
	assert.Equal(t, int64(0), metrics.LinkStatus.Load())
	assert.Equal(t, uint64(0), metrics.MessagesSent.Load())
	assert.Equal(t, uint64(0), metrics.MessagesReceived.Load())
	assert.Equal(t, uint64(0), metrics.BytesSent.Load())
	assert.Equal(t, uint64(0), metrics.BytesReceived.Load())
	assert.Equal(t, uint64(0), metrics.Errors.Load())
	assert.Equal(t, uint64(0), metrics.StateTransitions.Load())

	// Update metrics
	metrics.LinkStatus.Store(1)
	metrics.MessagesSent.Add(100)
	metrics.MessagesReceived.Add(50)
	metrics.BytesSent.Add(10000)
	metrics.BytesReceived.Add(5000)
	metrics.Errors.Add(3)
	metrics.StateTransitions.Add(5)

	// Verify updates
	assert.Equal(t, int64(1), metrics.LinkStatus.Load())
	assert.Equal(t, uint64(100), metrics.MessagesSent.Load())
	assert.Equal(t, uint64(50), metrics.MessagesReceived.Load())
	assert.Equal(t, uint64(10000), metrics.BytesSent.Load())
	assert.Equal(t, uint64(5000), metrics.BytesReceived.Load())
	assert.Equal(t, uint64(3), metrics.Errors.Load())
	assert.Equal(t, uint64(5), metrics.StateTransitions.Load())
}

func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name    string
		config  *Config
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid config",
			config: &Config{
				SGWAddress:      "10.0.1.100",
				SGWPort:         2905,
				TrafficMode:     1,
				LocalIPs:        []string{"10.0.2.10"},
				ASPUPACKTimeout: 5 * time.Second,
			},
			wantErr: false,
		},
		{
			name: "empty SGW address",
			config: &Config{
				SGWAddress: "",
			},
			wantErr: true,
			errMsg:  "sgw_address is required",
		},
		{
			name: "invalid port",
			config: &Config{
				SGWAddress: "10.0.1.100",
				SGWPort:    0,
			},
			wantErr: true,
			errMsg:  "sgw_port must be between 1 and 65535",
		},
		{
			name: "invalid traffic mode",
			config: &Config{
				SGWAddress:  "10.0.1.100",
				SGWPort:     2905,
				TrafficMode: 0,
			},
			wantErr: true,
			errMsg:  "traffic_mode must be 1 (Override) or 2 (Loadshare)",
		},
		{
			name: "multi-homing with one IP",
			config: &Config{
				SGWAddress:         "10.0.1.100",
				SGWPort:            2905,
				TrafficMode:        1,
				MultiHomingEnabled: true,
				LocalIPs:           []string{"10.0.2.10"},
			},
			wantErr: true,
			errMsg:  "multi-homing requires at least 2 local IPs",
		},
		{
			name: "timeout too short",
			config: &Config{
				SGWAddress:      "10.0.1.100",
				SGWPort:         2905,
				TrafficMode:     1,
				ASPUPACKTimeout: 500 * time.Millisecond,
			},
			wantErr: true,
			errMsg:  "aspup_ack_timeout must be at least 1 second",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfig(tt.config)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestHandlerConcurrentStateAccess(t *testing.T) {
	config := &Config{
		SGWAddress:        "10.0.1.100",
		SGWPort:           2905,
		ASPIdentifier:     1,
		RoutingContext:    1,
		NetworkAppearance: 0,
		TrafficMode:       1,
		LocalIPs:          []string{"10.0.2.10"},
		HeartbeatInterval: 30 * time.Second,
		PathMaxRetrans:    5,
		RTOMax:            2 * time.Second,
		RTOInitial:        1 * time.Second,
		RTOMin:            500 * time.Millisecond,
		AssocMaxRetrans:   10,
		ASPUPACKTimeout:   5 * time.Second,
		ASPACACKTimeout:   5 * time.Second,
		ReconnectInterval: 5 * time.Second,
	}

	logger := zap.NewNop()
	handler, err := NewHandler(config, logger)
	require.NoError(t, err)

	// Test concurrent state reads and writes
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan bool)

	// Writer goroutines
	for i := 0; i < 10; i++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					done <- true
					return
				default:
					handler.setState(ASPStateInactive)
					handler.setState(ASPStateActive)
					handler.setState(ASPStateDown)
				}
			}
		}()
	}

	// Reader goroutines
	for i := 0; i < 10; i++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					done <- true
					return
				default:
					_ = handler.GetState()
					_ = handler.IsActive()
				}
			}
		}()
	}

	// Wait for all goroutines to finish
	for i := 0; i < 20; i++ {
		<-done
	}
}
