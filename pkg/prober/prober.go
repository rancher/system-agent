package prober

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/sirupsen/logrus"

	planapi "github.com/rancher/rancher/pkg/plan"
	k8sprobe "k8s.io/kubernetes/pkg/probe"
	k8shttp "k8s.io/kubernetes/pkg/probe/http"
)

func DoProbe(probe planapi.Probe, probeStatus *planapi.ProbeStatus, initial bool) error {
	logrus.Tracef("Running probe %+v", probe)
	if initial {
		initialDelayDuration := time.Duration(probe.InitialDelaySeconds) * time.Second
		logrus.Debugf("[Probe: %s] Sleeping for %.0f seconds before running probe", probe.Name, initialDelayDuration.Seconds())
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
				logrus.Errorf("error loading x509 client cert/key for probe %s (%s/%s): %v", probe.Name, probe.HTTPGetAction.ClientCert, probe.HTTPGetAction.ClientKey, err)
			} else {
				tlsConfig.Certificates = []tls.Certificate{clientCert}
			}
		}

		caCertPool, err := GetSystemCertPool(probe.Name)
		if err != nil || caCertPool == nil {
			caCertPool = x509.NewCertPool()
			logrus.Errorf("error loading system cert pool for probe (%s): %v", probe.Name, err)
		}

		if probe.HTTPGetAction.CACert != "" {
			logrus.Debugf("[DoProbe] adding CA certificate [%s] for probe (%s)", probe.HTTPGetAction.CACert, probe.Name)
			caCert, err := os.ReadFile(probe.HTTPGetAction.CACert)
			if err != nil {
				logrus.Errorf("error loading CA cert for probe (%s) %s: %v", probe.Name, probe.HTTPGetAction.CACert, err)
			}
			if !caCertPool.AppendCertsFromPEM(caCert) {
				logrus.Errorf("error while appending ca cert to pool for probe %s", probe.Name)
			}
		}

		tlsConfig.RootCAs = caCertPool
		k8sProber = k8shttp.NewWithTLSConfig(&tlsConfig, false)
	}

	probeURL, err := url.Parse(probe.HTTPGetAction.URL)
	if err != nil {
		return err
	}

	probeRequest, err := k8shttp.NewProbeRequest(probeURL, http.Header{})
	if err != nil {
		return err
	}

	probeDuration := time.Duration(probe.TimeoutSeconds) * time.Second
	logrus.Tracef("[Probe: %s] timeout duration: %.0f seconds", probe.Name, probeDuration.Seconds())

	probeResult, output, err := k8sProber.Probe(probeRequest, probeDuration)
	if err != nil {
		logrus.Errorf("error while running probe (%s): %v", probe.Name, err)
		return err
	}

	logrus.Debugf("[Probe: %s] output was %s", probe.Name, output)

	successThreshold := resolveThreshold(probe.SuccessThreshold, defaultSuccessThreshold)
	failureThreshold := resolveThreshold(probe.FailureThreshold, defaultFailureThreshold)

	succeeded := probeResult == k8sprobe.Success
	if succeeded {
		logrus.Debugf("[Probe: %s] succeeded", probe.Name)
	} else {
		logrus.Debugf("[Probe: %s] failed", probe.Name)
	}
	applyProbeResult(probeStatus, succeeded, successThreshold, failureThreshold)

	return nil
}

// GetSystemCertPool returns a x509.CertPool that contains the
// root CA certificates if they are present at runtime
func GetSystemCertPool(probeName string) (*x509.CertPool, error) {
	caCertPool, err := x509.SystemCertPool()
	if err != nil {
		caCertPool = x509.NewCertPool()
		logrus.Errorf("[GetSystemCertPool] error loading system cert pool for probe (%s): %v", probeName, err)
	}
	if caCertPool == nil {
		return nil, fmt.Errorf("[GetSystemCertPool] x509 returned a nil certpool for probe (%s)", probeName)
	}
	return caCertPool, nil
}
