package localplan

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rancher/system-agent/pkg/prober"

	"github.com/rancher/system-agent/pkg/applyinator"
	"github.com/rancher/system-agent/pkg/types"
	"github.com/sirupsen/logrus"
)

func WatchFiles(ctx context.Context, applyinator applyinator.Applyinator, bases ...string) error {
	w := &watcher{
		bases:       bases,
		applyinator: applyinator,
	}

	go w.start(ctx)

	return nil
}

type watcher struct {
	bases       []string
	applyinator applyinator.Applyinator
}

const planSuffix = ".plan"
const positionSuffix = ".pos"

func (w *watcher) start(ctx context.Context) {
	force := true
	for {
		if err := w.listFiles(ctx, force); err == nil {
			force = false
		} else {
			logrus.Errorf("Failed to process config: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (w *watcher) listFiles(ctx context.Context, force bool) error {
	var errs []error
	for _, base := range w.bases {
		if err := w.listFilesIn(ctx, base, force); err != nil {
			errs = append(errs, err)
		}
	}
	return nil
}

func (w *watcher) listFilesIn(ctx context.Context, base string, force bool) error {
	files := map[string]os.FileInfo{}
	if err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		files[path] = info
		return nil
	}); err != nil {
		return err
	}

	skips := map[string]bool{}
	keys := make([]string, len(files))
	keyIndex := 0
	for path, file := range files {
		if strings.HasSuffix(file.Name(), ".skip") {
			skips[strings.TrimSuffix(file.Name(), ".skip")] = true
		}
		keys[keyIndex] = path
		keyIndex++
	}
	sort.Strings(keys)

	for _, path := range keys {
		if skipFile(files[path].Name(), skips) {
			continue
		}

		logrus.Debugf("[local] Processing file %s", path)

		var anp types.AgentNodePlan

		err := w.parsePlan(path, &anp)
		if err != nil {
			logrus.Errorf("[local] Error received when parsing plan: %s", err)
			continue
		}

		logrus.Debugf("[local] Plan from file %s was: %v", path, anp.Plan)

		needsApplied, probeStatuses, initialApplication, err := w.needsApplication(path, anp)

		if err != nil {
			logrus.Errorf("[local] Error while determining if node plan needed application: %v", err)
			continue
		}

		if probeStatuses == nil {
			probeStatuses = make(map[string]types.ProbeStatus)
		}

		var output []byte
		if needsApplied {
			output, err = w.applyinator.Apply(ctx, anp)
			if err != nil {
				logrus.Errorf("[local] Error when applying node plan from file: %s: %v", path, err)
				continue
			}
		}

		for probeName, probe := range anp.Plan.Probes {
			logrus.Debugf("[local] running probe %s", probeName)
			probeStatus, ok := probeStatuses[probeName]
			if !ok {
				probeStatus = types.ProbeStatus{}
			}
			if err := prober.Probe(probe, &probeStatus, initialApplication); err != nil {
				logrus.Errorf("error running probe %s", probeName)
			}
			probeStatuses[probeName] = probeStatus
		}

		if err := w.writePosition(path, anp, output, probeStatuses); err != nil {
			logrus.Errorf("[local] Error encountered when writing position file for %s: %v", path, err)
		}
	}

	return nil
}

func (w *watcher) parsePlan(file string, anp *types.AgentNodePlan) error {
	var np types.NodePlan
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()

	b, err := ioutil.ReadAll(f)
	if err != nil {
		return err
	}

	logrus.Tracef("[local] Byte data: %v", b)

	logrus.Debugf("[local] Plan string was %s", string(b))

	err = json.Unmarshal(b, &np)
	if err != nil {
		return err
	}

	anp.Checksum = checksum(b)
	anp.Plan = np

	return nil
}

// Returns true if the plan needs to be applied, false if not
func (w *watcher) needsApplication(file string, anp types.AgentNodePlan) (bool, map[string]types.ProbeStatus, bool, error) {
	positionFile := strings.TrimSuffix(file, planSuffix) + positionSuffix
	f, err := os.Open(positionFile)
	if err != nil {
		if os.IsNotExist(err) {
			logrus.Debugf("[local] Position file %s did not exist", positionFile)
			return true, nil, true, nil
		}
	}
	defer f.Close()

	var planPosition types.NodePlanPosition
	if err := json.NewDecoder(f).Decode(&planPosition); err != nil {
		logrus.Errorf("[local] Error encountered while decoding the node plan position: %v", err)
		return true, nil, true, nil
	}

	computedChecksum := anp.Checksum
	if planPosition.AppliedChecksum == computedChecksum {
		logrus.Debugf("[local] Plan %s checksum (%s) matched", file, computedChecksum)
		return false, planPosition.ProbeStatus, false, nil
	}
	logrus.Infof("[local] Plan checksums differed for %s (%s:%s)", file, computedChecksum, planPosition.AppliedChecksum)

	// Default to needing application.
	return true, planPosition.ProbeStatus, false, nil

}

func (w *watcher) writePosition(file string, anp types.AgentNodePlan, output []byte, probeStatus map[string]types.ProbeStatus) error {
	positionFile := strings.TrimSuffix(file, planSuffix) + positionSuffix
	f, err := os.Create(positionFile)
	if err != nil {
		logrus.Errorf("Error encountered when opening position file %s for writing: %v", positionFile, err)
		return err
	}
	defer f.Close()

	var npp types.NodePlanPosition
	npp.AppliedChecksum = anp.Checksum
	npp.Output = output
	npp.ProbeStatus = probeStatus
	return json.NewEncoder(f).Encode(npp)
}

func skipFile(fileName string, skips map[string]bool) bool {
	switch {
	case strings.HasPrefix(fileName, "."):
		return true
	case skips[fileName]:
		return true
	case strings.HasSuffix(fileName, planSuffix):
		return false
	default:
		return true
	}
}

func checksum(input []byte) string {
	h := sha256.New()
	h.Write(input)

	return fmt.Sprintf("%x", h.Sum(nil))
}
