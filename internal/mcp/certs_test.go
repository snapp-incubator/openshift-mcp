package mcp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/snapp-incubator/openshift-mcp/internal/k8s"
)

func certSecret(t *testing.T, name string, notAfter time.Time) *corev1.Secret {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "router.apps.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		DNSNames:     []string{"router.apps.example.com", "*.apps.example.com"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pemCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "openshift-ingress"},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": pemCert, "tls.key": []byte("PRIVATE")},
	}
}

func TestListCertificatesReportsExpiryNeverKeys(t *testing.T) {
	want := time.Now().Add(720 * time.Hour)
	c := &k8s.Client{Clientset: fake.NewSimpleClientset(certSecret(t, "router-certs", want))}

	out, err := handleListCertificates(context.Background(), c, args{"namespace": "openshift-ingress"})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["count"].(int) != 1 {
		t.Fatalf("count=%v", m["count"])
	}
	cert := m["certificates"].([]map[string]any)[0]
	if cert["subject"] != "router.apps.example.com" {
		t.Fatalf("subject=%v", cert["subject"])
	}
	if cert["issuer"] != "router.apps.example.com" { // self-signed: issuer == subject
		t.Fatalf("issuer=%v", cert["issuer"])
	}
	if d := cert["days_left"].(int); d < 28 || d > 31 {
		t.Fatalf("days_left=%d, want ~30", d)
	}
	if cert["expired"].(bool) {
		t.Fatal("cert marked expired")
	}
	// Private key must never surface anywhere in the output.
	for k, v := range cert {
		if strings.Contains(strings.ToLower(k), "key") {
			t.Fatalf("key-like field leaked: %s=%v", k, v)
		}
	}
}

func TestListCertificatesFlagsExpired(t *testing.T) {
	c := &k8s.Client{Clientset: fake.NewSimpleClientset(certSecret(t, "old", time.Now().Add(-time.Hour)))}
	out, _ := handleListCertificates(context.Background(), c, args{"namespace": "openshift-ingress"})
	cert := out.(map[string]any)["certificates"].([]map[string]any)[0]
	if !cert["expired"].(bool) {
		t.Fatalf("expired cert not flagged: %+v", cert)
	}
}
