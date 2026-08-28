package prober

import (
	"sync"

	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/sirupsen/logrus"
)

// DoProbes runs probes concurrently and updates probeStatuses.
// It waits for all probes to finish before returning.
func DoProbes(probes map[string]planapi.Probe, probeStatuses map[string]planapi.ProbeStatus, initial bool) {
	var wg sync.WaitGroup
	var mu sync.Mutex

	for probeName, probe := range probes {
		wg.Add(1)
		go func(probeName string, probe planapi.Probe, wg *sync.WaitGroup) {
			defer wg.Done()
			logrus.Debugf("[prober] running probe %s", probeName)
			mu.Lock()
			logrus.Tracef("[prober] retrieving existing probe status for %s from map if existing", probeName)
			probeStatus, ok := probeStatuses[probeName]
			mu.Unlock()
			if !ok {
				logrus.Tracef("[prober] probe status for %s was not present in map, initializing", probeName)
				probeStatus = planapi.ProbeStatus{}
			}
			probe.Name = probeName
			if err := DoProbe(probe, &probeStatus, initial); err != nil {
				logrus.Errorf("[prober] error running probe %s: %v", probeName, err)
			}
			mu.Lock()
			logrus.Tracef("[prober] writing probe status for %s to map", probeName)
			probeStatuses[probeName] = probeStatus
			mu.Unlock()
		}(probeName, probe, &wg)
	}
	// Wait for all probes to complete.
	wg.Wait()
}
