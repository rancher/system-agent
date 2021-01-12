package util

import (
	"crypto/sha256"
	"fmt"
	"github.com/oats87/rancher-agent/pkg/types"
)

func ComputeChecksum(np types.NodePlan) string {
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%v", np)))

	return fmt.Sprintf("%x", h.Sum(nil))
}