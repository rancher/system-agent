package k8splan

import (
	"testing"
)

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
