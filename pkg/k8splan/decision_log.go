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

// decisionLog is one log line produced while evaluating a plan decision.
// Decision functions are pure for unit testing.
// The collected log lines are the primary field-debugging surface on provisioned nodes.
// Tests can assert on the collected messages.
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
