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
	logrus.Tracef("[prober] running probe %+v", probe)
	if initial {
		initialDelayDuration := time.Duration(probe.InitialDelaySeconds) * time.Second
		logrus.Debugf("[prober] probe %s sleeping for %.0f seconds before running", probe.Name, initialDelayDuration.Seconds())
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
				logrus.Errorf("[prober] error loading x509 client cert/key for probe %s (%s/%s): %v", probe.Name, probe.HTTPGetAction.ClientCert, probe.HTTPGetAction.ClientKey, err)
			} else {
				tlsConfig.Certificates = []tls.Certificate{clientCert}
			}
		}

		caCertPool, err := GetSystemCertPool(probe.Name)
		if err != nil || caCertPool == nil {
			caCertPool = x509.NewCertPool()
			logrus.Errorf("[prober] error loading system cert pool for probe %s: %v", probe.Name, err)
		}

		if probe.HTTPGetAction.CACert != "" {
			logrus.Debugf("[prober] adding CA certificate %s for probe %s", probe.HTTPGetAction.CACert, probe.Name)
			caCert, err := os.ReadFile(probe.HTTPGetAction.CACert)
			if err != nil {
				logrus.Errorf("[prober] error loading CA cert %s for probe %s: %v", probe.HTTPGetAction.CACert, probe.Name, err)
				// Skip the append rather than falling through with a nil caCert, which would
				// produce a second, misleading "error appending" log on top of the real cause.
			} else if !caCertPool.AppendCertsFromPEM(caCert) {
				logrus.Errorf("[prober] error while appending CA cert to pool for probe %s", probe.Name)
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

	probeDuration := time.Duration(orDefault(probe.TimeoutSeconds, defaultTimeoutSeconds)) * time.Second
	logrus.Tracef("[prober] probe %s timeout duration: %.0f seconds", probe.Name, probeDuration.Seconds())

	probeResult, output, err := k8sProber.Probe(probeRequest, probeDuration)
	if err != nil {
		logrus.Errorf("[prober] error while running probe %s: %v", probe.Name, err)
		return err
	}

	logrus.Debugf("[prober] probe %s output was %s", probe.Name, output)

	successThreshold := orDefault(probe.SuccessThreshold, defaultSuccessThreshold)
	failureThreshold := orDefault(probe.FailureThreshold, defaultFailureThreshold)

	succeeded := probeResult == k8sprobe.Success
	if succeeded {
		logrus.Debugf("[prober] probe %s succeeded", probe.Name)
	} else {
		logrus.Debugf("[prober] probe %s failed", probe.Name)
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
		logrus.Errorf("[prober] error loading system cert pool for probe %s: %v", probeName, err)
	}
	if caCertPool == nil {
		return nil, fmt.Errorf("x509 returned a nil certpool for probe %s", probeName)
	}
	return caCertPool, nil
}
