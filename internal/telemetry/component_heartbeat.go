package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	metricapi "go.opentelemetry.io/otel/metric"
)

type ComponentHeartbeatHandle struct {
	registration metricapi.Registration
}

func RegisterComponentHeartbeat(scope string) (*ComponentHeartbeatHandle, error) {
	meter := otel.Meter(scope)
	up, err := meter.Int64ObservableGauge(
		"doctor.component.up",
		metricapi.WithDescription("Constant liveness gauge for a running Doctor component instance."),
	)
	if err != nil {
		return nil, err
	}
	registration, err := meter.RegisterCallback(func(_ context.Context, observer metricapi.Observer) error {
		observer.ObserveInt64(up, 1)
		return nil
	}, up)
	if err != nil {
		return nil, err
	}
	return &ComponentHeartbeatHandle{registration: registration}, nil
}

func (h *ComponentHeartbeatHandle) Unregister() error {
	if h == nil || h.registration == nil {
		return nil
	}
	return h.registration.Unregister()
}
