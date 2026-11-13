package config

import (
	"strings"
	"testing"
)

func envOf(m map[string]string) Lookup {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestDefaultsAndGeneratedInternals(t *testing.T) {
	c, err := Load(envOf(map[string]string{"NODE_PORT": "2222", "SECRET_KEY": "x", "HOSTNAME": "pod-a"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.NodePort != 2222 || c.HealthPort != 0 || c.K8sLeaseName != "remnanode-leader" || c.SNIVerification || c.K8sLeaderElection {
		t.Fatalf("defaults: %+v", c)
	}
	if len(c.InternalRestToken) != 64 || !strings.HasPrefix(c.InternalSocketPath, "rwint-") ||
		!strings.HasPrefix(c.XtlsAPISocketPath, "xtls-api-") || c.InternalSocketPath[6:] != c.XtlsAPISocketPath[9:] {
		t.Fatalf("generated internals don't follow init-env.sh: %q %q %q", c.InternalRestToken, c.InternalSocketPath, c.XtlsAPISocketPath)
	}
	if c.PodName != "pod-a" || c.XrayBinary != "/usr/local/bin/rw-core" || c.MetricsPort != 0 {
		t.Fatalf("%+v", c)
	}
	if c.TrustedProxies != "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16" {
		t.Errorf("TRUSTED_PROXIES default = %q", c.TrustedProxies)
	}
	if c.CPUThrottleEnabled || c.CPUThrottleHighPct != 90 || c.CPUThrottleLowPct != 75 ||
		c.CPUThrottleRateMbps != 50 || c.CPUThrottleMinMbps != 10 || c.CPUThrottleControlFile != "/tmp/rw-ratelimit" {
		t.Fatalf("cpu throttle defaults: %+v", c)
	}
}

func TestValidation(t *testing.T) {
	for _, c := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no NODE_PORT", map[string]string{"SECRET_KEY": "x"}, "NODE_PORT"},
		{"no SECRET_KEY", map[string]string{"NODE_PORT": "1"}, "SECRET_KEY"},
		{"bad boolean", map[string]string{"NODE_PORT": "1", "SECRET_KEY": "x", "SNI_VERIFICATION": "yes"}, "SNI_VERIFICATION"},
		{"bad health port", map[string]string{"NODE_PORT": "1", "SECRET_KEY": "x", "HEALTH_PORT": "abc"}, "HEALTH_PORT"},
		{"bad metrics port", map[string]string{"NODE_PORT": "1", "SECRET_KEY": "x", "METRICS_PORT": "70000"}, "METRICS_PORT"},
	} {
		if _, err := Load(envOf(c.env)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v", c.name, err)
		}
	}
	c, err := Load(envOf(map[string]string{"NODE_PORT": "2222abc", "SECRET_KEY": "x", "SNI_VERIFICATION": "", "INTERNAL_REST_TOKEN": "fixed"}))
	if err != nil || c.NodePort != 2222 || c.SNIVerification || c.InternalRestToken != "fixed" {
		t.Errorf("parseInt / empty boolean / provided token: %+v %v", c, err)
	}
}

func TestCPUThrottleConfig(t *testing.T) {
	c, err := Load(envOf(map[string]string{
		"NODE_PORT":                "2222",
		"SECRET_KEY":               "x",
		"CPU_THROTTLE_ENABLED":     "true",
		"CPU_THROTTLE_HIGH_PCT":    "85",
		"CPU_THROTTLE_LOW_PCT":     "60",
		"CPU_THROTTLE_RATE_MBPS":   "100",
		"CPU_THROTTLE_MIN_MBPS":    "20",
		"CPU_THROTTLE_CONTROL_FILE": "/var/run/rw-ratelimit",
	}))
	if err != nil {
		t.Fatalf("valid cpu throttle config failed: %v", err)
	}
	if !c.CPUThrottleEnabled || c.CPUThrottleHighPct != 85 || c.CPUThrottleLowPct != 60 ||
		c.CPUThrottleRateMbps != 100 || c.CPUThrottleMinMbps != 20 || c.CPUThrottleControlFile != "/var/run/rw-ratelimit" {
		t.Fatalf("unexpected values: %+v", c)
	}

	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"bad enabled", map[string]string{"NODE_PORT": "1", "SECRET_KEY": "x", "CPU_THROTTLE_ENABLED": "yes"}, "CPU_THROTTLE_ENABLED"},
		{"high too low", map[string]string{"NODE_PORT": "1", "SECRET_KEY": "x", "CPU_THROTTLE_HIGH_PCT": "49"}, "CPU_THROTTLE_HIGH_PCT"},
		{"high too high", map[string]string{"NODE_PORT": "1", "SECRET_KEY": "x", "CPU_THROTTLE_HIGH_PCT": "100"}, "CPU_THROTTLE_HIGH_PCT"},
		{"low >= high", map[string]string{"NODE_PORT": "1", "SECRET_KEY": "x", "CPU_THROTTLE_HIGH_PCT": "80", "CPU_THROTTLE_LOW_PCT": "80"}, "CPU_THROTTLE_LOW_PCT"},
		{"low <= 0", map[string]string{"NODE_PORT": "1", "SECRET_KEY": "x", "CPU_THROTTLE_LOW_PCT": "0"}, "CPU_THROTTLE_LOW_PCT"},
		{"rate <= 0", map[string]string{"NODE_PORT": "1", "SECRET_KEY": "x", "CPU_THROTTLE_RATE_MBPS": "0"}, "CPU_THROTTLE_RATE_MBPS"},
		{"min <= 0", map[string]string{"NODE_PORT": "1", "SECRET_KEY": "x", "CPU_THROTTLE_MIN_MBPS": "0"}, "CPU_THROTTLE_MIN_MBPS"},
		{"min > rate", map[string]string{"NODE_PORT": "1", "SECRET_KEY": "x", "CPU_THROTTLE_RATE_MBPS": "20", "CPU_THROTTLE_MIN_MBPS": "25"}, "CPU_THROTTLE_MIN_MBPS"},
	} {
		if _, err := Load(envOf(tc.env)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
}
