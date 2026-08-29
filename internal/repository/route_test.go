package repository

import (
	"errors"
	"testing"
)

func TestServiceExposureValidate(t *testing.T) {
	httpRoute := HTTPRoute{
		ID: "http-main", ServiceID: "svc_01J00000000000000000000000",
		Hostname: "example.test", PathPrefix: "/", PreserveHost: true,
		Enabled: true, CreatedAt: 1, UpdatedAt: 1,
	}
	tcpRoute := TCPRoute{
		ID: "tcp-main", ServiceID: "svc_01J00000000000000000000000",
		PublicPort: 8443, Enabled: true, CreatedAt: 1, UpdatedAt: 1,
	}

	tests := []struct {
		name     string
		exposure ServiceExposure
		wantErr  bool
	}{
		{name: "empty"},
		{name: "HTTP", exposure: ServiceExposure{HTTP: &httpRoute}},
		{name: "TCP", exposure: ServiceExposure{TCP: &tcpRoute}},
		{name: "both types", exposure: ServiceExposure{HTTP: &httpRoute, TCP: &tcpRoute}, wantErr: true},
		{name: "invalid HTTP", exposure: ServiceExposure{HTTP: &HTTPRoute{}}, wantErr: true},
		{name: "invalid TCP", exposure: ServiceExposure{TCP: &TCPRoute{}}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.exposure.Validate()
			if got := errors.Is(err, ErrInvalidRoute); got != test.wantErr {
				t.Fatalf("Validate() error = %v, ErrInvalidRoute = %t, want %t", err, got, test.wantErr)
			}
		})
	}
}
