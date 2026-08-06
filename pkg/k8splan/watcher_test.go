package k8splan

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// pemEncodeCert PEM-encodes a DER certificate, for constructing rest.Config.TLSClientConfig.CAData
// in tests.
func pemEncodeCert(t *testing.T, der []byte) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// generateUnrelatedCAPEM returns a freshly generated, self-signed certificate, PEM-encoded. It is
// well-formed X.509/PEM but signed by a key with no relationship to any real server's
// certificate — used to force a genuine "certificate signed by unknown authority" TLS handshake
// error, as opposed to a PEM-parse error from garbage bytes.
//
// httptest.NewTLSServer cannot be used for this: every instance reuses the same canned
// certificate (net/http/httptest's built-in test cert), so a second httptest server's certificate
// is identical to the first's, not merely "different but valid."
func generateUnrelatedCAPEM(t *testing.T) []byte {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "unrelated-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}
	return pemEncodeCert(t, der)
}

type fakeDecoder struct {
	decode func(data []byte, defaults *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error)
}

func (f *fakeDecoder) Decode(data []byte, defaults *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	return f.decode(data, defaults, into)
}

func TestUnstructuredDecoderPassthrough(t *testing.T) {
	t.Parallel()

	wantObj := &corev1.Secret{}
	wantGVK := &schema.GroupVersionKind{Kind: "Secret"}
	inner := &fakeDecoder{
		decode: func(data []byte, defaults *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
			return wantObj, wantGVK, nil
		},
	}

	d := unstructuredDecoder{Decoder: inner}
	obj, gvk, err := d.Decode([]byte("{}"), nil, &corev1.Secret{})
	if err != nil {
		t.Fatalf("Decode returned error: %v", err)
	}
	if obj != runtime.Object(wantObj) {
		t.Errorf("expected passthrough object %v, got %v", wantObj, obj)
	}
	if gvk != wantGVK {
		t.Errorf("expected passthrough gvk %v, got %v", wantGVK, gvk)
	}
}

func TestUnstructuredDecoderFallsBackWhenNotRegistered(t *testing.T) {
	t.Parallel()

	var sawFallback bool
	inner := &fakeDecoder{
		decode: func(data []byte, defaults *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
			if into == nil {
				return nil, nil, runtime.NewNotRegisteredErrForKind("test", schema.GroupVersionKind{})
			}
			sawFallback = true
			if _, ok := into.(*unstructured.Unstructured); !ok {
				t.Errorf("expected fallback decode to receive *unstructured.Unstructured, got %T", into)
			}
			return into, nil, nil
		},
	}

	d := unstructuredDecoder{Decoder: inner}
	if _, _, err := d.Decode([]byte("{}"), nil, nil); err != nil {
		t.Fatalf("Decode returned error: %v", err)
	}
	if !sawFallback {
		t.Error("expected the fallback decode path to run")
	}
}

func TestValidateKCSucceedsWithTrustedCA(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	caPEM := pemEncodeCert(t, server.Certificate().Raw)
	cfg := &rest.Config{
		Host:            server.URL,
		TLSClientConfig: rest.TLSClientConfig{CAData: caPEM},
	}

	if err := validateKC(context.Background(), cfg); err != nil {
		t.Fatalf("validateKC returned error: %v", err)
	}
}

func TestValidateKCFailsWithUntrustedCA(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := &rest.Config{Host: server.URL} // no CAData: falls back to system roots, which don't trust the test server's cert

	err := validateKC(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected an error connecting with an untrusted certificate, got nil")
	}
	if !strings.Contains(err.Error(), "x509") {
		t.Errorf("expected an x509 trust error, got: %v", err)
	}
}

func TestConnectWithCAFallback(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	caPEM := pemEncodeCert(t, server.Certificate().Raw)
	wrongCAPEM := generateUnrelatedCAPEM(t)

	// Subtests intentionally do not call t.Parallel(): they share server's lifecycle, which is
	// closed via defer when this parent test function returns. Parallel subtests only actually
	// run after the parent returns, which would race the deferred Close() against the subtest
	// bodies.
	t.Run("succeeds with matching CA data", func(t *testing.T) {
		kc := &rest.Config{Host: server.URL, TLSClientConfig: rest.TLSClientConfig{CAData: append([]byte(nil), caPEM...)}}
		if err := connectWithCAFallback(context.Background(), kc, true); err != nil {
			t.Fatalf("expected success, got: %v", err)
		}
	})

	t.Run("retries without CA data on unknown authority when not strict", func(t *testing.T) {
		// httptest.NewTLSServer's certificate is self-signed and not in any real trust store, so
		// the retry (which falls back to system roots) cannot succeed in this test environment
		// either — it exercises the same "unknown authority" failure a second time. What this
		// verifies is that the retry was actually attempted: CAData is nullified, and the error
		// is the "nullified CA data" variant (produced only by the second validateKC call), not
		// the immediate non-retry error.
		kc := &rest.Config{Host: server.URL, TLSClientConfig: rest.TLSClientConfig{CAData: append([]byte(nil), wrongCAPEM...)}}
		err := connectWithCAFallback(context.Background(), kc, false)
		if err == nil {
			t.Fatal("expected an error (the retry also fails against an untrusted test certificate), got nil")
		}
		if !strings.Contains(err.Error(), "nullified CA data") {
			t.Errorf("expected the retry's distinct error variant, got: %v", err)
		}
		if kc.CAData != nil {
			t.Error("expected CAData to be nullified once the retry was attempted")
		}
	})

	t.Run("fails without retrying when strict verify is set", func(t *testing.T) {
		kc := &rest.Config{Host: server.URL, TLSClientConfig: rest.TLSClientConfig{CAData: append([]byte(nil), wrongCAPEM...)}}
		err := connectWithCAFallback(context.Background(), kc, true)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if strings.Contains(err.Error(), "nullified CA data") {
			t.Error("expected the non-retry error variant when strict verify prevents the fallback retry")
		}
		if kc.CAData == nil {
			t.Error("expected CAData to be left untouched when strict verify prevents the fallback retry")
		}
	})
}

func TestToInt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  int
	}{
		{name: "valid positive number", input: "42", want: 42},
		{name: "zero", input: "0", want: 0},
		{name: "empty string defaults to zero", input: "", want: 0},
		{name: "non-numeric string defaults to zero", input: "not-a-number", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := toInt(tt.input); got != tt.want {
				t.Errorf("toInt(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestIncrementCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input []byte
		want  string
	}{
		{name: "nil input starts at one", input: nil, want: "1"},
		{name: "empty input starts at one", input: []byte(""), want: "1"},
		{name: "valid count increments", input: []byte("5"), want: "6"},
		{name: "multi-digit count increments", input: []byte("99"), want: "100"},
		{name: "non-numeric input falls back to one", input: []byte("garbage"), want: "1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := string(incrementCount(tt.input)); got != tt.want {
				t.Errorf("incrementCount(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
