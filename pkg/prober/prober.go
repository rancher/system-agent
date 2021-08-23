package prober

import (
	"crypto/tls"
	"crypto/x509"
	"io/ioutil"
	"net/http"
	"net/url"
	"time"

	"github.com/sirupsen/logrus"
	k8sprobe "k8s.io/kubernetes/pkg/probe"
	k8shttp "k8s.io/kubernetes/pkg/probe/http"
)

type HTTPGetAction struct {
	URL        string `json:"url,omitempty"`
	Insecure   bool   `json:"insecure,omitempty"`
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
	HTTPGetAction       HTTPGetAction `json:"httpGet,omitempty"`
}

type ProbeStatus struct {
	Healthy      bool `json:"healthy,omitempty"`
	SuccessCount int  `json:"successCount,omitempty"`
	FailureCount int  `json:"failureCount,omitempty"`
}

func DoProbe(probe Probe, probeStatus *ProbeStatus, initial bool) error {
	logrus.Tracef("Running probe %v", probe)
	if initial {
		initialDelayDuration := time.Duration(probe.InitialDelaySeconds) * time.Second
		logrus.Debugf("sleeping for %.0f seconds before running probe", initialDelayDuration.Seconds())
		time.Sleep(initialDelayDuration)
	}

	var k8sProber k8shttp.Prober

	if probe.HTTPGetAction.Insecure {
		k8sProber = k8shttp.New(false)
	} else {
		tlsConfig := tls.Config{}
		if probe.HTTPGetAction.ClientCert != "" && probe.HTTPGetAction.ClientKey != "" {
			clientCert, err := tls.LoadX509KeyPair(probe.HTTPGetAction.ClientCert, probe.HTTPGetAction.ClientKey)
			if err != nil {
				logrus.Errorf("error loading x509 client cert/key (%s/%s): %v", probe.HTTPGetAction.ClientCert, probe.HTTPGetAction.ClientKey, err)
			}
			tlsConfig.Certificates = []tls.Certificate{clientCert}
		}

		caCertPool, err := x509.SystemCertPool()
		if err != nil {
			caCertPool = x509.NewCertPool()
			logrus.Errorf("error loading system cert pool: %v", err)
		}

		if probe.HTTPGetAction.CACert != "" {
			caCert, err := ioutil.ReadFile(probe.HTTPGetAction.CACert)
			if err != nil {
				logrus.Errorf("error loading CA cert %s: %v", probe.HTTPGetAction.CACert, err)
			}
			if !caCertPool.AppendCertsFromPEM(caCert) {
				logrus.Errorf("error while appending ca cert to pool")
			}
		}

		tlsConfig.RootCAs = caCertPool
		k8sProber = k8shttp.NewWithTLSConfig(&tlsConfig, false)
	}

	probeURL, err := url.Parse(probe.HTTPGetAction.URL)
	if err != nil {
		return err
	}

	probeDuration := time.Duration(probe.TimeoutSeconds) * time.Second
	logrus.Debugf("Probe timeout duration: %.0f seconds", probeDuration.Seconds())

	probeResult, output, err := k8sProber.Probe(probeURL, http.Header{}, probeDuration)

	if err != nil {
		logrus.Errorf("error while running probe: %v", err)
		return err
	}

	logrus.Debugf("Probe output was %s", output)

	var successThreshold, failureThreshold int

	if probe.SuccessThreshold == 0 {
		logrus.Debugf("Setting success threshold to default")
		successThreshold = 1
	} else {
		logrus.Debugf("Setting success threshold to %d", probe.SuccessThreshold)
		successThreshold = probe.SuccessThreshold
	}

	if probe.FailureThreshold == 0 {
		logrus.Debugf("Setting failure threshold to default")
		failureThreshold = 3
	} else {
		logrus.Debugf("Setting failure threshold to %d", probe.FailureThreshold)
		failureThreshold = probe.FailureThreshold
	}

	switch probeResult {
	case k8sprobe.Success:
		logrus.Debug("Probe succeeded")
		if probeStatus.SuccessCount < successThreshold {
			probeStatus.SuccessCount = probeStatus.SuccessCount + 1
			if probeStatus.SuccessCount >= successThreshold {
				probeStatus.Healthy = true
			}
		}
		probeStatus.FailureCount = 0
	default:
		logrus.Debug("Probe failed")
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
