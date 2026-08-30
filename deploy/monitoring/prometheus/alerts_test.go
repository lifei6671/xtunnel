package prometheusalerts

import (
	"os"
	"regexp"
	"strconv"
	"testing"

	"go.yaml.in/yaml/v3"
)

const certificateMetric = "xtunnel_gateway_certificate_expiry_seconds"

var remainingExpression = regexp.MustCompile(`^\(` + certificateMetric + ` - time\(\)\) <= ([0-9]+) and \(` + certificateMetric + ` - time\(\)\) > ([0-9]+)$`)

type alertRule struct {
	Name   string            `yaml:"alert"`
	Expr   string            `yaml:"expr"`
	Labels map[string]string `yaml:"labels"`
}

func TestCertificateExpiryAlertBoundariesDoNotOverlap(t *testing.T) {
	rules := loadAlertRules(t)
	if len(rules) != 4 {
		t.Fatalf("certificate alert rule count = %d, want 4", len(rules))
	}
	wantSeverities := []string{"warning", "warning", "critical", "critical"}
	for index, rule := range rules {
		if rule.Labels["severity"] != wantSeverities[index] {
			t.Fatalf("alert %s severity = %q, want %q", rule.Name, rule.Labels["severity"], wantSeverities[index])
		}
	}

	tests := []struct {
		name      string
		remaining int64
		want      string
	}{
		{name: "outside thirty day window", remaining: 2_592_001},
		{name: "thirty day boundary", remaining: 2_592_000, want: "XTunnelGatewayCertificateExpiresWithin30Days"},
		{name: "seven day boundary", remaining: 604_800, want: "XTunnelGatewayCertificateExpiresWithin7Days"},
		{name: "one day boundary", remaining: 86_400, want: "XTunnelGatewayCertificateExpiresWithin1Day"},
		{name: "one second remaining", remaining: 1, want: "XTunnelGatewayCertificateExpiresWithin1Day"},
		{name: "expiry boundary", remaining: 0, want: "XTunnelGatewayCertificateExpired"},
		{name: "already expired", remaining: -1, want: "XTunnelGatewayCertificateExpired"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			matched := make([]string, 0, 1)
			for _, rule := range rules {
				if alertMatches(t, rule.Expr, test.remaining) {
					matched = append(matched, rule.Name)
				}
			}
			if test.want == "" && len(matched) != 0 {
				t.Fatalf("matched alerts = %v, want none", matched)
			}
			if test.want != "" && (len(matched) != 1 || matched[0] != test.want) {
				t.Fatalf("matched alerts = %v, want [%s]", matched, test.want)
			}
		})
	}
}

func loadAlertRules(t *testing.T) []alertRule {
	t.Helper()
	contents, err := os.ReadFile("xtunnel-alerts-v1.yaml")
	if err != nil {
		t.Fatalf("open alert rules: %v", err)
	}
	var ruleFile struct {
		Groups []struct {
			Name  string      `yaml:"name"`
			Rules []alertRule `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(contents, &ruleFile); err != nil {
		t.Fatalf("parse alert rules: %v", err)
	}
	if len(ruleFile.Groups) != 1 || ruleFile.Groups[0].Name != "xtunnel.gateway-certificate.v1" {
		t.Fatalf("alert groups = %#v, want versioned certificate group", ruleFile.Groups)
	}
	for _, rule := range ruleFile.Groups[0].Rules {
		if rule.Name == "" || rule.Expr == "" {
			t.Fatalf("incomplete alert rule = %#v", rule)
		}
	}
	return ruleFile.Groups[0].Rules
}

func alertMatches(t *testing.T, expression string, remaining int64) bool {
	t.Helper()
	if expression == certificateMetric+" <= time()" {
		return remaining <= 0
	}
	matches := remainingExpression.FindStringSubmatch(expression)
	if matches == nil {
		t.Fatalf("unsupported certificate alert expression %q", expression)
	}
	upper, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		t.Fatalf("parse upper alert boundary: %v", err)
	}
	lower, err := strconv.ParseInt(matches[2], 10, 64)
	if err != nil {
		t.Fatalf("parse lower alert boundary: %v", err)
	}
	return remaining <= upper && remaining > lower
}
