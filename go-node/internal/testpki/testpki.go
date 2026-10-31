// Package testpki builds the key material the panel generates
// (backend/src/common/utils/certs/generate-certs.util.ts): an ECDSA P-256 CA,
// a client cert with the clientAuth EKU, a node cert with serverAuth, an
// RSA-2048 JWT key pair, the SECRET_KEY payload, and the API JWT the panel
// sends. Tests and the parity harness play the panel with it.
package testpki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type PKI struct {
	CACertPem, CAKeyPem         string
	ClientCertPem, ClientKeyPem string
	NodeCertPem, NodeKeyPem     string
	JWTPublicPem, JWTPrivatePem string

	CA         *x509.Certificate
	ClientCert tls.Certificate
	JWTKey     *rsa.PrivateKey
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func pemOf(typ string, der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}))
}

func New() *PKI {
	now := time.Now().Add(-time.Minute)
	caKey := must(ecdsa.GenerateKey(elliptic.P256(), rand.Reader))
	caTpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: now, NotAfter: now.AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER := must(x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey))
	ca := must(x509.ParseCertificate(caDER))

	leaf := func(serial int64, eku x509.ExtKeyUsage, ku x509.KeyUsage, years int) (string, string) {
		k := must(ecdsa.GenerateKey(elliptic.P256(), rand.Reader))
		tpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "leaf"},
			NotBefore: now, NotAfter: now.AddDate(years, 0, 0),
			BasicConstraintsValid: true, KeyUsage: ku, ExtKeyUsage: []x509.ExtKeyUsage{eku},
		}
		der := must(x509.CreateCertificate(rand.Reader, tpl, ca, &k.PublicKey, caKey))
		return pemOf("CERTIFICATE", der), pemOf("PRIVATE KEY", must(x509.MarshalPKCS8PrivateKey(k)))
	}
	p := &PKI{CA: ca}
	p.CACertPem = pemOf("CERTIFICATE", caDER)
	p.CAKeyPem = pemOf("PRIVATE KEY", must(x509.MarshalPKCS8PrivateKey(caKey)))
	p.ClientCertPem, p.ClientKeyPem = leaf(2, x509.ExtKeyUsageClientAuth, x509.KeyUsageDigitalSignature, 10)
	p.NodeCertPem, p.NodeKeyPem = leaf(time.Now().UnixMilli(), x509.ExtKeyUsageServerAuth,
		x509.KeyUsageDigitalSignature|x509.KeyUsageKeyEncipherment, 3)
	p.ClientCert = must(tls.X509KeyPair([]byte(p.ClientCertPem), []byte(p.ClientKeyPem)))

	p.JWTKey = must(rsa.GenerateKey(rand.Reader, 2048))
	p.JWTPublicPem = pemOf("PUBLIC KEY", must(x509.MarshalPKIXPublicKey(&p.JWTKey.PublicKey)))
	p.JWTPrivatePem = pemOf("PRIVATE KEY", must(x509.MarshalPKCS8PrivateKey(p.JWTKey)))
	return p
}

// SecretKey is encodeCertPayload(): base64(JSON.stringify({...})).
func (p *PKI) SecretKey() string {
	b, _ := json.Marshal(map[string]string{
		"nodeCertPem": p.NodeCertPem, "nodeKeyPem": p.NodeKeyPem,
		"caCertPem": p.CACertPem, "jwtPublicKey": p.JWTPublicPem,
	})
	return base64.StdEncoding.EncodeToString(b)
}

// PanelJWT is get-node-jwt.handler.ts: RS256 {uuid:null, username:null,
// role:'API'} with a 9999-day expiry.
func (p *PKI) PanelJWT() string {
	now := time.Now()
	t := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"uuid": nil, "username": nil, "role": "API",
		"iat": now.Unix(), "exp": now.Add(9999 * 24 * time.Hour).Unix(),
	})
	return must(t.SignedString(p.JWTKey))
}

// ClientTLS is the panel's mTLS agent: client cert, panel CA, no hostname
// check (checkServerIdentity is a no-op), TLS 1.3, SNI as given.
func (p *PKI) ClientTLS(serverName string) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(p.CA)
	return &tls.Config{
		Certificates:       []tls.Certificate{p.ClientCert},
		MinVersion:         tls.VersionTLS13,
		ServerName:         serverName,
		InsecureSkipVerify: true, // chain verified below, hostname not checked
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			cert, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			_, err = cert.Verify(x509.VerifyOptions{Roots: pool})
			return err
		},
	}
}
