module github.com/oats87/rancher-agent

go 1.13

replace k8s.io/client-go => k8s.io/client-go v0.18.0

require (
	github.com/google/go-containerregistry v0.3.0
	github.com/mattn/go-colorable v0.1.8
	github.com/pkg/errors v0.9.1
	github.com/rancher/lasso v0.0.0-20200905045615-7fcb07d6a20b
	github.com/rancher/wrangler v0.7.2
	github.com/sirupsen/logrus v1.6.0
	k8s.io/api v0.18.8
	k8s.io/apimachinery v0.18.8
	sigs.k8s.io/yaml v1.2.0
)
