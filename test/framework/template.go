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
	"context"
	"os"
)

// ApplyFromTemplateInput is the input for ApplyFromTemplate.
type ApplyFromTemplateInput struct {
	// KubeconfigPath is the path to the kubeconfig file.
	KubeconfigPath string

	// Template is the raw YAML template content with ${VAR} placeholders.
	Template []byte

	// Variables maps placeholder names to their replacement values.
	Variables map[string]string
}

// ApplyFromTemplate renders a YAML template by substituting ${VAR} references
// from the provided variables map, then applies the result to the cluster.
func ApplyFromTemplate(ctx context.Context, input ApplyFromTemplateInput) {
	rendered := RenderTemplate(input.Template, input.Variables)
	KubectlApplyStdin(ctx, input.KubeconfigPath, rendered)
}

// RenderTemplate substitutes ${VAR} references in template with values from vars.
// Unknown variables are replaced with empty strings.
func RenderTemplate(template []byte, vars map[string]string) []byte {
	rendered := os.Expand(string(template), func(key string) string {
		if val, ok := vars[key]; ok {
			return val
		}
		return ""
	})
	return []byte(rendered)
}
