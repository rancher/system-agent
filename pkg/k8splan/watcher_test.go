package k8splan

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
