// Copyright © 2025 SUSE LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package framework

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"

	"github.com/rancher/system-agent/pkg/applyinator"
)

// DecodeOutput decodes a gzip-compressed output,
// as produced by the system-agent for applied-output and periodicOutput fields.
// The data is stored as raw gzip bytes containing JSON with base64-encoded values.
// Returns a formatted string with all instruction outputs.
func DecodeOutput(encoded []byte) (string, error) {
	if len(encoded) == 0 {
		return "", nil
	}

	// Step 1: Decompress gzip
	reader, err := gzip.NewReader(bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	defer reader.Close()

	gzipResult, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}

	// Step 2: Parse JSON (map of instruction name -> base64-encoded output)
	var outputMap map[string]string
	if err := json.Unmarshal(gzipResult, &outputMap); err != nil {
		return "", err
	}

	// Step 3: Decode base64 values and format as string
	var result bytes.Buffer
	for name, b64Output := range outputMap {
		decoded, err := base64.StdEncoding.DecodeString(b64Output)
		if err != nil {
			return "", err
		}
		result.WriteString(name)
		result.WriteString(": ")
		result.Write(decoded)
		if !bytes.HasSuffix(decoded, []byte("\n")) {
			result.WriteString("\n")
		}
	}

	return result.String(), nil
}

// GetOutputMap decodes a gzip-compressed output and returns a map of instruction names
// to their decoded string outputs.
func GetOutputMap(encoded []byte) (map[string]string, error) {
	if len(encoded) == 0 {
		return nil, nil
	}

	reader, err := gzip.NewReader(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	gzipResult, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	var rawMap map[string]string
	if err := json.Unmarshal(gzipResult, &rawMap); err != nil {
		return nil, err
	}

	result := make(map[string]string, len(rawMap))
	for k, v := range rawMap {
		decoded, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("failed to decode output for %s: %w", k, err)
		}
		result[k] = string(decoded)
	}

	return result, nil
}

// DecodePeriodicOutput decodes the gzip-compressed periodic output and returns
// a map of instruction names to their PeriodicInstructionOutput structs.
func DecodePeriodicOutput(encoded []byte) (map[string]applyinator.PeriodicInstructionOutput, error) {
	if len(encoded) == 0 {
		return nil, nil
	}

	reader, err := gzip.NewReader(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	gzipResult, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	var outputMap map[string]applyinator.PeriodicInstructionOutput
	if err := json.Unmarshal(gzipResult, &outputMap); err != nil {
		return nil, err
	}

	return outputMap, nil
}
