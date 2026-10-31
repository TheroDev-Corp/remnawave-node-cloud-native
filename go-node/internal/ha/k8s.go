package ha

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Lease is a coordination.k8s.io/v1 Lease, as k8s-lease.client.ts sends it.
type Lease struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Metadata   LeaseMetadata `json:"metadata"`
	Spec       LeaseSpec     `json:"spec"`
}

type LeaseMetadata struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

type LeaseSpec struct {
	HolderIdentity       *string `json:"holderIdentity"`
	LeaseDurationSeconds int     `json:"leaseDurationSeconds,omitempty"`
	AcquireTime          string  `json:"acquireTime,omitempty"`
	RenewTime            string  `json:"renewTime,omitempty"`
	LeaseTransitions     int     `json:"leaseTransitions,omitempty"`
}

type PodInfo struct {
	CreationTimestamp string
	Labels            map[string]string
	RestartCount      int
}

// K8s is the in-cluster API client: service-account token and CA, like
// K8sLeaseClient.
type K8s struct {
	base  string
	token string
	http  *http.Client
}

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

func NewK8s() *K8s {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	if host == "" {
		host = "kubernetes.default.svc"
	}
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if port == "" {
		port = "443"
	}
	token, _ := os.ReadFile(saDir + "/token")
	tlsCfg := &tls.Config{InsecureSkipVerify: true} // like rejectUnauthorized: Boolean(ca) without a CA
	if ca, err := os.ReadFile(saDir + "/ca.crt"); err == nil {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(ca)
		tlsCfg = &tls.Config{RootCAs: pool}
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return &K8s{
		base:  "https://" + host + ":" + port,
		token: strings.TrimSpace(string(token)),
		http:  &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}},
	}
}

// NewK8sAt points the client at an explicit API server (tests).
func NewK8sAt(base, token string, client *http.Client) *K8s {
	return &K8s{base: base, token: token, http: client}
}

func (k *K8s) do(ctx context.Context, method, path string, body any, ctype string) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+k.token)
	if body != nil {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, err
}

func leasePath(ns, name string) string {
	p := "/apis/coordination.k8s.io/v1/namespaces/" + url.PathEscape(ns) + "/leases"
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

func decodeLease(status, want int, raw []byte) (int, *Lease, error) {
	if status != want {
		return status, nil, nil
	}
	var l Lease
	if err := json.Unmarshal(raw, &l); err != nil {
		return status, nil, err
	}
	return status, &l, nil
}

func (k *K8s) GetLease(ctx context.Context, ns, name string) (int, *Lease, error) {
	st, raw, err := k.do(ctx, http.MethodGet, leasePath(ns, name), nil, "")
	if err != nil {
		return 0, nil, err
	}
	return decodeLease(st, http.StatusOK, raw)
}

func (k *K8s) CreateLease(ctx context.Context, ns string, l *Lease) (int, *Lease, error) {
	st, raw, err := k.do(ctx, http.MethodPost, leasePath(ns, ""), l, "application/json")
	if err != nil {
		return 0, nil, err
	}
	return decodeLease(st, http.StatusCreated, raw)
}

// UpdateLease PUTs the lease; the resourceVersion makes a concurrent writer
// fail with 409.
func (k *K8s) UpdateLease(ctx context.Context, ns, name string, l *Lease) (int, *Lease, error) {
	st, raw, err := k.do(ctx, http.MethodPut, leasePath(ns, name), l, "application/json")
	if err != nil {
		return 0, nil, err
	}
	return decodeLease(st, http.StatusOK, raw)
}

func podPath(ns, name string) string {
	return "/api/v1/namespaces/" + url.PathEscape(ns) + "/pods/" + url.PathEscape(name)
}

func (k *K8s) GetPod(ctx context.Context, ns, name string) (int, *PodInfo, error) {
	st, raw, err := k.do(ctx, http.MethodGet, podPath(ns, name), nil, "")
	if err != nil || st != http.StatusOK {
		return st, nil, err
	}
	var pod struct {
		Metadata struct {
			CreationTimestamp string            `json:"creationTimestamp"`
			Labels            map[string]string `json:"labels"`
		} `json:"metadata"`
		Status struct {
			ContainerStatuses []struct {
				Name         string `json:"name"`
				RestartCount int    `json:"restartCount"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &pod); err != nil {
		return st, nil, err
	}
	info := &PodInfo{CreationTimestamp: pod.Metadata.CreationTimestamp, Labels: pod.Metadata.Labels}
	cs := pod.Status.ContainerStatuses
	for _, c := range cs {
		if c.Name == "remnanode" {
			info.RestartCount = c.RestartCount
			return st, info, nil
		}
	}
	if len(cs) > 0 {
		info.RestartCount = cs[0].RestartCount
	}
	return st, info, nil
}

func (k *K8s) PatchPodLabels(ctx context.Context, ns, name string, labels map[string]string) (int, string, error) {
	st, raw, err := k.do(ctx, http.MethodPatch, podPath(ns, name),
		map[string]any{"metadata": map[string]any{"labels": labels}}, "application/strategic-merge-patch+json")
	return st, string(raw), err
}

// k8sMicroTime is formatK8sMicroTime: millisecond ISO time padded to micros.
func k8sMicroTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000") + "000Z"
}

func parseK8sTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad lease time %q: %w", s, err)
	}
	return t, nil
}
