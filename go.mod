module github.com/oats87/rancher-agent

go 1.13

replace k8s.io/client-go => k8s.io/client-go v0.18.0

require (
	github.com/google/go-containerregistry v0.3.0
	github.com/mattn/go-colorable v0.1.8
	github.com/pkg/errors v0.9.1
	github.com/rancher/wrangler v0.7.2
	github.com/sirupsen/logrus v1.6.0
)
