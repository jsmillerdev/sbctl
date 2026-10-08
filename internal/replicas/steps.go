package replicas

import (
	"slices"

	"github.com/supavise/supavise/internal/registry"
)

// The seven setup steps of the spec, in order (design 2.7.2). The registry row holds the step
// reached; the work that leads to the next one happens while the row sits at this one.
const (
	StepRequested  = registry.ReplicaStepRequested
	StepStarted    = "1_started"
	StepLaunched   = "2_launched_read_replica_instance"
	StepInitiated  = "3_initiated_read_replica_setup"
	StepDownloaded = "4_downloaded_base_backup"
	StepReplayed   = "5_replayed_wal_archives"
	StepDone       = registry.ReplicaStepDone
)

var steps = []string{StepRequested, StepStarted, StepLaunched, StepInitiated, StepDownloaded, StepReplayed, StepDone}

// The failure codes of the spec. A failed setup keeps the step it reached in init_step and
// names what failed in init_error.
const (
	FailLaunch   = "1_read_replica_instance_launch_failed"
	FailInitiate = "2_initiate_read_replica_setup_failed"
	FailDownload = "3_download_base_backup_failed"
	FailReplay   = "4_replay_wal_archives_failed"
	FailComplete = "5_complete_read_replica_setup_failed"
)

var failureCodes = []string{FailLaunch, FailInitiate, FailDownload, FailReplay, FailComplete}

// stepIndex is the position of step in the sequence, or -1 for a value that is no step.
func stepIndex(step string) int { return slices.Index(steps, step) }

// laterStep is the one of a and b that comes further in the sequence; a value that is no step loses.
func laterStep(a, b string) string {
	if stepIndex(b) > stepIndex(a) {
		return b
	}
	return a
}

// failureFor is the code of a failure while the row is at step: the work that leads out of
// step 0 and step 1 (admission, the base backup, launching the instance) is the launch.
func failureFor(step string) string {
	i := min(max(stepIndex(step), 1), len(failureCodes))
	return failureCodes[i-1]
}

// knownFailure reports whether code is one of the spec's failure values.
func knownFailure(code string) bool { return slices.Contains(failureCodes, code) }

// failureStep is the step a row is at when code is the failure it holds.
func failureStep(code string) string {
	if i := slices.Index(failureCodes, code); i >= 0 {
		return steps[i+1]
	}
	return StepRequested
}

// Replica statuses the controller writes besides the registry's INIT_READ_REPLICA and
// INIT_READ_REPLICA_FAILED: the Management API's project status values (design 2.7.7).
const (
	statusHealthy   = string(registry.StatusActiveHealthy)
	statusUnhealthy = string(registry.StatusActiveUnhealthy)
	statusRestart   = string(registry.StatusRestarting)
	statusResizing  = string(registry.StatusResizing)
	statusGoingDown = string(registry.StatusGoingDown)
)

// settingUp reports whether the row is a setup in flight: not failed, not finished.
func settingUp(r *registry.Replica) bool {
	return r.Status == registry.ReplicaInit && r.InitError == ""
}

// active reports whether the row is a replica that finished its setup and is not being removed.
func active(r *registry.Replica) bool {
	switch r.Status {
	case statusHealthy, statusUnhealthy, statusRestart, statusResizing:
		return true
	}
	return false
}
