package k8splan

import (
	"fmt"

	"github.com/sirupsen/logrus"
)

// decisionLevel identifies the logrus level a decisionLog should be emitted at.
type decisionLevel int

const (
	decisionTrace decisionLevel = iota
	decisionDebug
	decisionInfo
	decisionError
)

// decisionLog is a single log line produced while evaluating a plan decision.
//
// The decision functions in plan_decision.go are pure so they can be exhaustively unit tested, but
// the lines they would otherwise log are the primary field-debugging surface for this daemon: it
// runs on provisioned nodes where journalctl is often the only diagnostic available. Collecting
// entries instead of logging directly keeps the functions pure while preserving that surface, and
// makes the reasoning behind a decision assertable in tests.
type decisionLog struct {
	Level   decisionLevel
	Message string
}

func traceDecision(format string, args ...any) decisionLog {
	return decisionLog{Level: decisionTrace, Message: fmt.Sprintf("[k8splan] "+format, args...)}
}

func debugDecision(format string, args ...any) decisionLog {
	return decisionLog{Level: decisionDebug, Message: fmt.Sprintf("[k8splan] "+format, args...)}
}

func infoDecision(format string, args ...any) decisionLog {
	return decisionLog{Level: decisionInfo, Message: fmt.Sprintf("[k8splan] "+format, args...)}
}

func errorDecision(format string, args ...any) decisionLog {
	return decisionLog{Level: decisionError, Message: fmt.Sprintf("[k8splan] "+format, args...)}
}

// emitDecisionLogs writes collected decision logs at their recorded levels.
func emitDecisionLogs(logs []decisionLog) {
	for _, entry := range logs {
		switch entry.Level {
		case decisionTrace:
			logrus.Trace(entry.Message)
		case decisionDebug:
			logrus.Debug(entry.Message)
		case decisionInfo:
			logrus.Info(entry.Message)
		case decisionError:
			logrus.Error(entry.Message)
		}
	}
}
