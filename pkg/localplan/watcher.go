package localplan

import (
	"context"
	"encoding/json"
	"github.com/oats87/rancher-agent/pkg/applyinator"
	"github.com/oats87/rancher-agent/pkg/types"
	"github.com/sirupsen/logrus"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func WatchFiles(ctx context.Context, applyinator applyinator.Applyinator, bases ...string) error {
	w := &watcher{
		bases:      bases,
		applyinator: applyinator,
	}

	go w.start(ctx)

	return nil
}

type watcher struct {
	bases []string
	applyinator applyinator.Applyinator
}

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

	//var errs []error
	for _, path := range keys {
		if skipFile(files[path].Name(), skips) {
			continue
		}

		logrus.Debugf("Processing file %s", path)

		var np types.NodePlan

		err := w.parsePlan(path, &np)
		if err != nil {
			logrus.Errorf("Error received when parsing plan: %s", err)
			continue
		}

		logrus.Debugf("Plan from file %s was: %v", path, np)

		needsApplied, err := w.needsApplication(path, np)
		if !needsApplied {
			continue
		}

		if err := w.applyinator.Apply(ctx, np); err != nil {
			logrus.Errorf("Error when applying node plan from file: %s: %v", path, err)
			continue
		}

		if err := w.writePosition(path, np); err != nil {
			logrus.Errorf("Error encountered when writing position file for %s: %v", path, err)
		}
	}

	return nil
}

func (w *watcher) parsePlan(file string, np interface{}) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(f).Decode(np)
}

// Returns true if the plan needs to be applied, false if not
func (w *watcher) needsApplication(file string, np types.NodePlan) (bool, error) {
	positionFile := file+positionSuffix
	f, err := os.Open(positionFile)
	if err != nil {
		if os.IsNotExist(err) {
			logrus.Debugf("Position file %s did not exist", positionFile)
			return true, nil
		}
	}
	defer f.Close()

	var planPosition types.NodePlanPosition
	if err := json.NewDecoder(f).Decode(&planPosition); err != nil {
		logrus.Errorf("Error encountered while decoding the node plan position: %v", err)
		return true, nil
	}

	computedChecksum := np.Checksum()
	if planPosition.AppliedChecksum == computedChecksum {
		logrus.Debugf("Plan %s checksum (%s) matched", file, computedChecksum)
		return false, nil
	}
	logrus.Infof("Plan checksums differed for %s (%s:%s)", file, computedChecksum, planPosition.AppliedChecksum)

	// Default to needing application.
	return true, nil

}

func (w *watcher) writePosition(file string, np types.NodePlan) error {
	positionFile := file+positionSuffix
	f, err := os.Create(positionFile)
	if err != nil {
		logrus.Errorf("Error encountered when opening position file %s for writing: %v", positionFile, err)
		return err
	}
	defer f.Close()

	var npp types.NodePlanPosition
	npp.AppliedChecksum = np.Checksum()

	return json.NewEncoder(f).Encode(npp)
}

func skipFile(fileName string, skips map[string]bool) bool {
	switch {
	case strings.HasPrefix(fileName, "."):
		return true
	case skips[fileName]:
		return true
	case strings.HasSuffix(fileName, ".plan"):
		return false
	default:
		return true
	}
}