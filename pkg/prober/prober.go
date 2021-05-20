package prober

import (
	"net/http"
	"net/url"
	"time"

	"github.com/sirupsen/logrus"

	k8sprobe "k8s.io/kubernetes/pkg/probe"
	k8shttp "k8s.io/kubernetes/pkg/probe/http"
)

type HttpGetAction struct {
	Path       string `json:"path,omitempty"`
	ClientCert string `json:"clientCert,omitempty"`
	ClientKey  string `json:"clientKey,omitempty"`
	CACert     string `json:"caCert,omitempty"`
}

type Probe struct {
	Name                string        `json:"name,omitempty"`
	InitialDelaySeconds int           `json:"initialDelaySeconds,omitempty"` // default 0
	TimeoutSeconds      int           `json:"timeoutSeconds,omitempty"`      // default 1
	SuccessThreshold    int           `json:"successThreshold,omitempty"`    // default 1
	FailureThreshold    int           `json:"failureThreshold,omitempty"`    // default 3
	HttpGetAction       HttpGetAction `json:"httpGet,omitempty"`
}

type ProbeStatus struct {
	Healthy      bool `json:"healthy,omitempty"`
	SuccessCount int  `json:"successCount,omitempty"`
	FailureCount int  `json:"failureCount,omitempty"`
}

func DoProbe(probe Probe, probeStatus *ProbeStatus, initial bool) error {
	logrus.Tracef("running probe %v", probe)
	if initial {
		logrus.Debugf("sleeping for %d seconds before running probe", probe.InitialDelaySeconds)
		time.Sleep(time.Duration(probe.InitialDelaySeconds))
	}
	k8sProber := k8shttp.New(false)

	probeURL, err := url.Parse(probe.HttpGetAction.Path)
	if err != nil {
		return err
	}

	probeResult, _, err := k8sProber.Probe(probeURL, http.Header{}, time.Duration(probe.TimeoutSeconds))

	if err != nil {
		return err
	}

	var successThreshold, failureThreshold int

	if probe.SuccessThreshold == 0 {
		logrus.Debugf("Setting default success threshold")
		successThreshold = 1
	} else {
		successThreshold = probe.SuccessThreshold
	}

	if probe.FailureThreshold == 0 {
		logrus.Debugf("Setting default failure threshold")
		failureThreshold = 3
	} else {
		failureThreshold = probe.FailureThreshold
	}

	switch probeResult {
	case k8sprobe.Success:
		logrus.Debug("probe was successful")
		if probeStatus.SuccessCount < successThreshold {
			probeStatus.SuccessCount = probeStatus.SuccessCount + 1
			if probeStatus.SuccessCount >= successThreshold {
				probeStatus.Healthy = true
			}
		}
		probeStatus.FailureCount = 0
	default:
		logrus.Debug("probe status failed")
		if probeStatus.FailureCount < failureThreshold {
			probeStatus.FailureCount = probeStatus.FailureCount + 1
			if probeStatus.FailureCount >= failureThreshold {
				probeStatus.Healthy = false
			}
		}
		probeStatus.SuccessCount = 0
	}

	return nil
}
