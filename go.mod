module github.com/rancher/system-agent

go 1.16

require (
	github.com/docker/cli v20.10.6+incompatible
	github.com/google/go-containerregistry v0.5.0
	github.com/mattn/go-colorable v0.1.8
	github.com/rancher/lasso v0.0.0-20210408231703-9ddd9378d08d
	github.com/rancher/wharfie v0.1.0
	github.com/rancher/wrangler v0.8.0
	github.com/sirupsen/logrus v1.8.1
	gotest.tools/v3 v3.0.3 // indirect
	k8s.io/api v0.21.0
	k8s.io/apimachinery v0.21.0
	k8s.io/client-go v0.21.0
	sigs.k8s.io/yaml v1.2.0
)
