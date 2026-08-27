package repository

import (
	"errors"
	"testing"
)

const testServiceID = "svc_01J00000000000000000000000"

func TestServiceValidate(t *testing.T) {
	valid := validHTTPService()
	tests := []struct {
		name   string
		mutate func(*Service)
	}{
		{name: "合法 HTTP Service", mutate: func(*Service) {}},
		{name: "错误 Service ID", mutate: func(service *Service) { service.ID = "svc_invalid" }},
		{name: "错误 Tunnel owner", mutate: func(service *Service) { service.TunnelID = "tun_invalid" }},
		{name: "空白名称", mutate: func(service *Service) { service.Name = " \t" }},
		{name: "空白 Origin Host", mutate: func(service *Service) { service.OriginHost = " \t" }},
		{name: "Required Revision 为负数", mutate: func(service *Service) { service.RequiredRevision = -1 }},
		{name: "Origin Port 为零", mutate: func(service *Service) { service.OriginPort = 0 }},
		{name: "Origin Port 超过 uint16", mutate: func(service *Service) { service.OriginPort = 65_536 }},
		{name: "Connect Timeout 为零", mutate: func(service *Service) { service.ConnectTimeoutMS = 0 }},
		{name: "Version 为零", mutate: func(service *Service) { service.Version = 0 }},
		{name: "CreatedAt 为零", mutate: func(service *Service) { service.CreatedAt = 0 }},
		{name: "UpdatedAt 为零", mutate: func(service *Service) { service.UpdatedAt = 0 }},
		{name: "未知 Origin Scheme", mutate: func(service *Service) { service.OriginScheme = "udp" }},
		{name: "Origin Host 含 URI", mutate: func(service *Service) { service.OriginHost = "http://origin.example" }},
		{name: "Origin Host 含端口", mutate: func(service *Service) { service.OriginHost = "origin.example:80" }},
		{name: "Origin Host 非规范大小写", mutate: func(service *Service) { service.OriginHost = "Origin.Example" }},
		{name: "Origin Host 尾点", mutate: func(service *Service) { service.OriginHost = "origin.example." }},
		{name: "Origin Host 畸形 IPv4", mutate: func(service *Service) { service.OriginHost = "127.0.0.999" }},
		{name: "HTTP 设置 TLS Server Name", mutate: func(service *Service) { service.TLSServerName = "origin.example" }},
		{name: "可选 HTTP Host 仅空白", mutate: func(service *Service) { service.OriginHTTPHost = " " }},
		{name: "HTTP Host 注入换行", mutate: func(service *Service) { service.OriginHTTPHost = "origin.example\r\nx: bad" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			err := candidate.Validate()
			if test.name == "合法 HTTP Service" && err != nil {
				t.Fatalf("Service.Validate() error = %v", err)
			}
			if test.name != "合法 HTTP Service" && !errors.Is(err, ErrInvalidService) {
				t.Fatalf("Service.Validate() error = %v, want ErrInvalidService", err)
			}
		})
	}
}

func TestServiceValidateOriginSchemesAndOptionalFields(t *testing.T) {
	tests := []struct {
		name    string
		service Service
		wantErr bool
	}{
		{name: "HTTP 可带 Origin Host Header", service: validHTTPService()},
		{name: "未发布 Service Revision 可为零", service: func() Service {
			service := validHTTPService()
			service.RequiredRevision = 0
			return service
		}()},
		{name: "HTTP 可省略 Origin Host Header", service: func() Service {
			service := validHTTPService()
			service.OriginHTTPHost = ""
			return service
		}()},
		{name: "HTTP 保留非适用 TLS Verify 值", service: func() Service {
			service := validHTTPService()
			service.TLSVerify = true
			return service
		}()},
		{name: "HTTPS TLS Verify 与 Server Name", service: validHTTPSService()},
		{name: "HTTPS 可从 Origin Host 推导 Server Name", service: func() Service {
			service := validHTTPSService()
			service.TLSServerName = ""
			return service
		}()},
		{name: "HTTPS 可关闭证书校验但保留 SNI", service: func() Service {
			service := validHTTPSService()
			service.TLSVerify = false
			return service
		}()},
		{name: "TCP 无 HTTP TLS 可选字段", service: validTCPService()},
		{name: "TCP 禁止 HTTP Host", service: func() Service {
			service := validTCPService()
			service.OriginHTTPHost = "origin.example"
			return service
		}(), wantErr: true},
		{name: "TCP 保留非适用 TLS Verify 值", service: func() Service {
			service := validTCPService()
			service.TLSVerify = true
			return service
		}()},
		{name: "TCP 禁止 TLS Server Name", service: func() Service {
			service := validTCPService()
			service.TLSServerName = "origin.example"
			return service
		}(), wantErr: true},
		{name: "HTTPS TLS Server Name 仅空白", service: func() Service {
			service := validHTTPSService()
			service.TLSServerName = " "
			return service
		}(), wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.service.Validate()
			if test.wantErr && !errors.Is(err, ErrInvalidService) {
				t.Fatalf("Service.Validate() error = %v, want ErrInvalidService", err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("Service.Validate() error = %v", err)
			}
		})
	}
}

func TestServiceValidateHealth(t *testing.T) {
	validTCPHealth := HealthCheck{
		Type: HealthTypeTCP, IntervalMS: 1_000, TimeoutMS: 100,
		FailureThreshold: 1, SuccessThreshold: 20,
	}
	validHTTPHealth := HealthCheck{
		Type: HealthTypeHTTP, Path: "/health", IntervalMS: 3_600_000, TimeoutMS: 3_599_999,
		ExpectedStatusMin: 100, ExpectedStatusMax: 599, FailureThreshold: 20, SuccessThreshold: 1,
	}
	tests := []struct {
		name    string
		health  *HealthCheck
		wantErr bool
	}{
		{name: "nil 表示 Disabled", health: nil},
		{name: "合法 TCP 边界", health: &validTCPHealth},
		{name: "合法 HTTP 边界", health: &validHTTPHealth},
		{name: "未知 Health Type", health: healthCopy(validTCPHealth, func(health *HealthCheck) { health.Type = "UDP" }), wantErr: true},
		{name: "Interval 低于下限", health: healthCopy(validTCPHealth, func(health *HealthCheck) { health.IntervalMS = 999 }), wantErr: true},
		{name: "Interval 高于上限", health: healthCopy(validTCPHealth, func(health *HealthCheck) { health.IntervalMS = 3_600_001 }), wantErr: true},
		{name: "Timeout 低于下限", health: healthCopy(validTCPHealth, func(health *HealthCheck) { health.TimeoutMS = 99 }), wantErr: true},
		{name: "Timeout 等于 Interval", health: healthCopy(validTCPHealth, func(health *HealthCheck) { health.TimeoutMS = 1_000 }), wantErr: true},
		{name: "Failure Threshold 为零", health: healthCopy(validTCPHealth, func(health *HealthCheck) { health.FailureThreshold = 0 }), wantErr: true},
		{name: "Failure Threshold 超限", health: healthCopy(validTCPHealth, func(health *HealthCheck) { health.FailureThreshold = 21 }), wantErr: true},
		{name: "Success Threshold 为零", health: healthCopy(validTCPHealth, func(health *HealthCheck) { health.SuccessThreshold = 0 }), wantErr: true},
		{name: "Success Threshold 超限", health: healthCopy(validTCPHealth, func(health *HealthCheck) { health.SuccessThreshold = 21 }), wantErr: true},
		{name: "TCP 禁止 Path", health: healthCopy(validTCPHealth, func(health *HealthCheck) { health.Path = "/" }), wantErr: true},
		{name: "TCP 禁止最小状态码", health: healthCopy(validTCPHealth, func(health *HealthCheck) { health.ExpectedStatusMin = 200 }), wantErr: true},
		{name: "TCP 禁止最大状态码", health: healthCopy(validTCPHealth, func(health *HealthCheck) { health.ExpectedStatusMax = 299 }), wantErr: true},
		{name: "HTTP Path 必须以斜杠开头", health: healthCopy(validHTTPHealth, func(health *HealthCheck) { health.Path = "health" }), wantErr: true},
		{name: "HTTP Path 不能为空", health: healthCopy(validHTTPHealth, func(health *HealthCheck) { health.Path = "" }), wantErr: true},
		{name: "HTTP 最小状态码过低", health: healthCopy(validHTTPHealth, func(health *HealthCheck) { health.ExpectedStatusMin = 99 }), wantErr: true},
		{name: "HTTP 最大状态码过高", health: healthCopy(validHTTPHealth, func(health *HealthCheck) { health.ExpectedStatusMax = 600 }), wantErr: true},
		{name: "HTTP 状态码倒置", health: healthCopy(validHTTPHealth, func(health *HealthCheck) { health.ExpectedStatusMin = 400; health.ExpectedStatusMax = 399 }), wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := validHTTPService()
			service.Health = test.health
			err := service.Validate()
			if test.wantErr && !errors.Is(err, ErrInvalidService) {
				t.Fatalf("Service.Validate() error = %v, want ErrInvalidService", err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("Service.Validate() error = %v", err)
			}
		})
	}
}

func validHTTPService() Service {
	return Service{
		ID: testServiceID, TunnelID: testTunnelID, Name: "office web", RequiredRevision: 1,
		OriginScheme: OriginSchemeHTTP, OriginHost: "127.0.0.1", OriginPort: 8080,
		OriginHTTPHost: "origin.example", ConnectTimeoutMS: 5_000, Enabled: true,
		Version: 1, CreatedAt: 1, UpdatedAt: 1,
	}
}

func validHTTPSService() Service {
	service := validHTTPService()
	service.OriginScheme = OriginSchemeHTTPS
	service.TLSVerify = true
	service.TLSServerName = "origin.example"
	return service
}

func validTCPService() Service {
	service := validHTTPService()
	service.OriginScheme = OriginSchemeTCP
	service.OriginHTTPHost = ""
	return service
}

func healthCopy(source HealthCheck, mutate func(*HealthCheck)) *HealthCheck {
	candidate := source
	mutate(&candidate)
	return &candidate
}
