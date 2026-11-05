//go:build ignore

package clients

import (
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func init() {
	log.SetLogger(klog.NewKlogr())
}
