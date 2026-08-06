package localplan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/rancher/system-agent/pkg/applyinator"
	"github.com/rancher/system-agent/pkg/prober"
	"github.com/sirupsen/logrus"
)

func WatchFiles(ctx context.Context, applyinator applyinator.Applyinator, bases ...string) {
	w := &watcher{
		bases:       bases,
		applyinator: applyinator,
	}

	go w.start(ctx)
}

// stdout and stderr are both base64, gzipped
type NodePlanPosition struct {
	AppliedChecksum string                         `json:"appliedChecksum,omitempty"`
	Output          []byte                         `json:"output,omitempty"`
	ProbeStatus     map[string]planapi.ProbeStatus `json:"probeStatus,omitempty"`
	PeriodicOutput  []byte                         `json:"periodicOutput,omitempty"`
}

type watcher struct {
	bases       []string
	applyinator applyinator.Applyinator
}

const (
	planSuffix     = ".plan"
	positionSuffix = ".pos"
)

func (w *watcher) start(ctx context.Context) {
	force := true
	for {
		if err := w.listFiles(ctx, force); err == nil {
			force = false
		} else {
			logrus.Errorf("[localplan] Failed to process config: %v", err)
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
	return errors.Join(errs...)
}

func (w *watcher) listFilesIn(ctx context.Context, base string, _ bool) error {
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

		logrus.Debugf("[localplan] Processing file %s", path)

		cp, err := w.parsePlan(path)
		if err != nil {
			logrus.Errorf("[localplan] Error received when parsing plan: %v", err)
			continue
		}

		logrus.Debugf("[localplan] Plan from file %s was: %v", path, cp.Plan)

		posFile := positionFileName(path)
		posData, err := readPositionFile(posFile)
		if err != nil {
			logrus.Errorf("[localplan] Error reading position file: %v", err)
		}

		planPosition, err := parsePositionData(posData)
		if err != nil { // this is going to be mad that its empty
			logrus.Errorf("[localplan] Error parsing position data: %v", err)
		}

		needsApplied, probeStatuses, err := w.needsApplication(planPosition, cp)

		if err != nil {
			logrus.Errorf("[localplan] Error while determining if node plan needed application: %v", err)
			continue
		}

		if probeStatuses == nil {
			probeStatuses = make(map[string]planapi.ProbeStatus)
		}

		input := applyinator.ApplyInput{
			CalculatedPlan:         cp,
			ReconcileFiles:         needsApplied,
			ExistingOneTimeOutput:  planPosition.Output,
			ExistingPeriodicOutput: planPosition.PeriodicOutput,
			RunOneTimeInstructions: needsApplied,
		}

		applyOutput, err := w.applyinator.Apply(ctx, input)
		if err != nil {
			logrus.Errorf("[localplan] Error when applying node plan from file: %s: %v", path, err)
			continue
		}

		prober.DoProbes(cp.Plan.Probes, probeStatuses, needsApplied)

		var npp NodePlanPosition
		npp.AppliedChecksum = cp.Checksum
		npp.Output = applyOutput.OneTimeOutput
		npp.ProbeStatus = probeStatuses
		npp.PeriodicOutput = applyOutput.PeriodicOutput

		newPPData, err := json.Marshal(npp)
		if err != nil {
			logrus.Errorf("[localplan] Error marshalling new plan position data: %v", err)
		}

		if !bytes.Equal(newPPData, posData) {
			logrus.Debugf("[localplan] Writing position data")
			if err := os.WriteFile(posFile, newPPData, 0600); err != nil {
				logrus.Errorf("[localplan] Error encountered when writing position file for %s: %v", path, err)
			}
		}
	}

	return nil
}

func (w *watcher) parsePlan(file string) (applyinator.CalculatedPlan, error) {
	f, err := os.Open(file)
	if err != nil {
		return applyinator.CalculatedPlan{}, err
	}
	defer f.Close()

	b, err := io.ReadAll(f)
	if err != nil {
		return applyinator.CalculatedPlan{}, err
	}

	logrus.Tracef("[localplan] Byte data: %v", b)

	logrus.Debugf("[localplan] Plan string was %s", string(b))

	cp, err := applyinator.CalculatePlan(b)
	if err != nil {
		return cp, err
	}

	return cp, nil
}

func positionFileName(planPath string) string {
	return strings.TrimSuffix(planPath, planSuffix) + positionSuffix
}

func readPositionFile(positionFile string) ([]byte, error) {
	data, err := os.ReadFile(positionFile)
	if err != nil {
		if os.IsNotExist(err) {
			logrus.Debugf("[localplan] Position file %s did not exist", positionFile)
			return []byte{}, nil
		}
		return []byte{}, err
	}
	return data, nil
}

func parsePositionData(positionData []byte) (NodePlanPosition, error) {
	var planPosition NodePlanPosition
	if len(positionData) == 0 {
		return planPosition, nil
	}
	err := json.Unmarshal(positionData, &planPosition)
	return planPosition, err
}

// Returns true if the plan needs to be applied, false if not
// needsApplication, probeStatus, error
func (w *watcher) needsApplication(planPosition NodePlanPosition, cp applyinator.CalculatedPlan) (bool, map[string]planapi.ProbeStatus, error) {
	computedChecksum := cp.Checksum
	if planPosition.AppliedChecksum == computedChecksum {
		logrus.Debugf("[localplan] Plan checksum (%s) matched", computedChecksum)
		return false, planPosition.ProbeStatus, nil
	}
	logrus.Infof("[localplan] Plan checksums differed (%s:%s)", computedChecksum, planPosition.AppliedChecksum)

	// Default to needing application.
	return true, planPosition.ProbeStatus, nil

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
