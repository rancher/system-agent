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
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CreatePlanSecret creates a Kubernetes Secret containing a plan in the "plan" data key.
func CreatePlanSecret(ctx context.Context, cl client.Client, namespace, name string, plan []byte) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"plan": plan,
		},
	}
	return cl.Create(ctx, secret)
}

// UpdatePlanSecret updates the plan data in an existing Secret.
func UpdatePlanSecret(ctx context.Context, cl client.Client, namespace, name string, plan []byte) error {
	secret := &corev1.Secret{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		return err
	}
	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	secret.Data["plan"] = plan
	return cl.Update(ctx, secret)
}

// GetSecret retrieves a Secret by namespace and name.
func GetSecret(ctx context.Context, cl client.Client, namespace, name string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret)
	return secret, err
}

// DeleteSecret deletes a Secret by namespace and name.
func DeleteSecret(ctx context.Context, cl client.Client, namespace, name string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	return client.IgnoreNotFound(cl.Delete(ctx, secret))
}

// WaitForSecretField polls a Secret until the specified data key appears and is non-empty.
func WaitForSecretField(ctx context.Context, cl client.Client, namespace, name, field string, timeout, interval time.Duration) []byte {
	var value []byte
	Eventually(func() bool {
		secret := &corev1.Secret{}
		if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
			return false
		}
		val, ok := secret.Data[field]
		if !ok || len(val) == 0 {
			return false
		}
		value = val
		return true
	}, timeout, interval).Should(BeTrue(), fmt.Sprintf("Secret %s/%s field %q not populated in time", namespace, name, field))
	return value
}

// GetSecretData retrieves all data keys from a Secret.
func GetSecretData(ctx context.Context, cl client.Client, namespace, name string) map[string][]byte {
	secret := &corev1.Secret{}
	Expect(cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret)).
		NotTo(HaveOccurred(), "Failed to get secret %s/%s", namespace, name)
	return secret.Data
}

// GetAppliedChecksum retrieves the "applied-checksum" field from a plan Secret.
func GetAppliedChecksum(ctx context.Context, cl client.Client, namespace, name string) string {
	data := GetSecretData(ctx, cl, namespace, name)
	return string(data["applied-checksum"])
}

// GetProbeStatuses retrieves and unmarshals the "probe-statuses" field from a plan Secret.
func GetProbeStatuses(ctx context.Context, cl client.Client, namespace, name string) map[string]interface{} {
	data := GetSecretData(ctx, cl, namespace, name)
	raw, ok := data["probe-statuses"]
	if !ok {
		return nil
	}
	var result map[string]interface{}
	err := json.Unmarshal(raw, &result)
	Expect(err).NotTo(HaveOccurred(), "Failed to unmarshal probe-statuses")
	return result
}

// CreatePlanSecretWithData creates a Kubernetes Secret containing a plan plus additional data fields.
func CreatePlanSecretWithData(ctx context.Context, cl client.Client, namespace, name string, plan []byte, extraData map[string][]byte) error {
	data := map[string][]byte{
		"plan": plan,
	}
	for k, v := range extraData {
		data[k] = v
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: data,
	}
	return cl.Create(ctx, secret)
}

// WaitForSecretFieldCondition polls a Secret until the specified condition function returns true for the field.
func WaitForSecretFieldCondition(ctx context.Context, cl client.Client, namespace, name, field string, condition func([]byte) bool, timeout, interval time.Duration) []byte {
	var value []byte
	Eventually(func() bool {
		secret := &corev1.Secret{}
		if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
			return false
		}
		val, ok := secret.Data[field]
		if !ok {
			return false
		}
		if condition(val) {
			value = val
			return true
		}
		return false
	}, timeout, interval).Should(BeTrue(), fmt.Sprintf("Secret %s/%s field %q condition not met in time", namespace, name, field))
	return value
}

// WaitForSecretFieldIntAtLeast polls until an integer field in the Secret reaches the specified minimum value.
func WaitForSecretFieldIntAtLeast(ctx context.Context, cl client.Client, namespace, name, field string, minVal int, timeout, interval time.Duration) int {
	var result int
	Eventually(func() bool {
		secret := &corev1.Secret{}
		if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
			return false
		}
		val, ok := secret.Data[field]
		if !ok {
			return false
		}
		n, err := strconv.Atoi(string(val))
		if err != nil {
			return false
		}
		result = n
		return n >= minVal
	}, timeout, interval).Should(BeTrue(), fmt.Sprintf("Secret %s/%s field %q should be >= %d", namespace, name, field, minVal))
	return result
}
