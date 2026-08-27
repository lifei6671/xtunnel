package originconfig

import "testing"

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		fields Fields
		valid  bool
	}{
		{name: "HTTP DNS", fields: validFields("http", "origin.test"), valid: true},
		{name: "HTTPS explicit SNI", fields: func() Fields {
			value := validFields("https", "127.0.0.1")
			value.TLSServerName = "origin.test"
			return value
		}(), valid: true},
		{name: "TCP IPv6 private", fields: validFields("tcp", "fd00::1"), valid: true},
		{name: "reserved UDP scheme is not executable in V0.1", fields: validFields("udp", "origin.test")},
		{name: "reserved QUIC scheme is not executable in V0.1", fields: validFields("quic", "origin.test")},
		{name: "reserved Unix scheme is not executable in V0.1", fields: validFields("unix", "origin.test")},
		{name: "URI host", fields: validFields("tcp", "http://origin.test")},
		{name: "host with port", fields: validFields("tcp", "origin.test:80")},
		{name: "uppercase DNS", fields: validFields("tcp", "Origin.Test")},
		{name: "absolute DNS", fields: validFields("tcp", "origin.test.")},
		{name: "invalid dotted decimal", fields: validFields("tcp", "127.0.0.999")},
		{name: "IPv6 zone", fields: validFields("tcp", "fe80::1%eth0")},
		{name: "zero port", fields: func() Fields { value := validFields("tcp", "origin.test"); value.Port = 0; return value }()},
		{name: "zero timeout", fields: func() Fields { value := validFields("tcp", "origin.test"); value.ConnectTimeoutMS = 0; return value }()},
		{name: "TCP TLS name", fields: func() Fields {
			value := validFields("tcp", "origin.test")
			value.TLSServerName = "origin.test"
			return value
		}()},
		{name: "TCP HTTP host", fields: func() Fields {
			value := validFields("tcp", "origin.test")
			value.HTTPHostHeader = "origin.test"
			return value
		}()},
		{name: "header injection", fields: func() Fields {
			value := validFields("https", "origin.test")
			value.HTTPHostHeader = "origin.test\r\nx: bad"
			return value
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := Validate(test.fields); (err == nil) != test.valid {
				t.Fatalf("Validate(%#v) error = %v, valid=%v", test.fields, err, test.valid)
			}
		})
	}
}

func validFields(scheme, host string) Fields {
	return Fields{Scheme: scheme, Host: host, Port: 8080, ConnectTimeoutMS: 5_000, TLSVerify: true}
}
