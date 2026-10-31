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
