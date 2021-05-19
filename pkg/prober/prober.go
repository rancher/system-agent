package prober

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/rancher/system-agent/pkg/types"
	k8sprobe "k8s.io/kubernetes/pkg/probe"
	k8shttp "k8s.io/kubernetes/pkg/probe/http"
)

func Probe(probe types.Probe, probeStatus *types.ProbeStatus, initial bool) error {
	logrus.Tracef("running probe %v", probe)
	if initial {
		initialDuration, err := time.ParseDuration(strconv.Itoa(probe.InitialDelaySeconds))
		if err != nil {
			return err
		}
		time.Sleep(initialDuration)
	}
	k8sProber := k8shttp.New(false)

	probeURL, err := url.Parse(probe.HttpGetAction.Path)
	if err != nil {
		return err
	}

	result, _, err := k8sProber.Probe(probeURL, http.Header{}, time.Duration(probe.TimeoutSeconds))

	if err != nil {
		return err
	}

	var successThreshold, failureThreshold int

	if probe.SuccessThreshold == 0 {
		successThreshold = 1
	} else {
		successThreshold = probe.SuccessThreshold
	}

	if probe.FailureThreshold == 0 {
		failureThreshold = 3
	} else {
		failureThreshold = probe.FailureThreshold
	}

	switch result {
	case k8sprobe.Success:
		if probeStatus.SuccessCount < probe.SuccessThreshold {
			probeStatus.SuccessCount = probeStatus.SuccessCount + 1
		}
		probeStatus.FailureCount = 0
	default:
		if probeStatus.FailureCount < probe.FailureThreshold {
			probeStatus.FailureCount = probeStatus.FailureCount + 1
		}
		probeStatus.SuccessCount = 0
	}

	if probeStatus.SuccessCount >= successThreshold {
		probeStatus.Healthy = true
	}

	if probeStatus.FailureCount >= failureThreshold {
		probeStatus.Healthy = false
	}

	return nil
}
