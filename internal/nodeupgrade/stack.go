package nodeupgrade

import (
	"context"
	"errors"
	"fmt"
)

// StackOptions are the operator's choices for the stack update behind `supavise upgrade --aws`.
type StackOptions struct {
	// Name is the CloudFormation stack; empty lets the host find it (config, the instance's tags).
	Name string
	// Sets are template parameters, each "Key=Value" (Failover=on, PeerCidr1=203.0.113.7/32).
	Sets []string
}

// StackOutcome says what UpdateStack did.
type StackOutcome struct {
	// Command, when set, is the command line the operator runs by hand: the host found no AWS
	// credentials of the operator's own, so nothing was run.
	Command string
	// Standalone, with Command, is the same update run from a shell that holds the credentials and
	// is not this node (AWS CloudShell, a laptop): the release's signed script, which also runs on
	// its own.
	Standalone string
	// GuardLeftOn is a stack that was updated but still holds the script's temporary stack policy
	// (the guard), which denies later replacements; the script printed the command that takes it off.
	GuardLeftOn bool
}

// StackError is the stack update ending without success: the script's exit status (2 refused
// the change set, 3 failed, including a guarded update that CloudFormation rolled back; 129, 130
// or 143 interrupted, and -1 killed by a signal) and what it said.
type StackError struct {
	Code int
	Err  error
}

func (e *StackError) Error() string { return e.Err.Error() }
func (e *StackError) Unwrap() error { return e.Err }

// StackUpdater is the optional Host capability behind --aws: it stages the release's verified
// stack script and template and runs the update with the operator's AWS credentials, which are
// never the instance role's. A host without it refuses --aws.
type StackUpdater interface {
	// UpdateStack runs the update for the release c. It returns a *StackError when the script
	// refused or failed, and a StackOutcome with a Command when there were no credentials to run
	// it with. Neither changes the node.
	UpdateStack(ctx context.Context, c *Candidate, o StackOptions) (StackOutcome, error)
}

// HostConverger is the optional Host capability that runs the host layer without a change of
// binary: `supavise system converge` of the installed binary. When the binary changes, Install
// does it right after the swap.
type HostConverger interface {
	Converge(ctx context.Context) error
}

// stackStep runs the stack update for --aws, before anything on the node changes. It returns the
// command to print at the end when the update could not run, and a Failure (exit status 2: the node
// is unchanged) when the script refused or failed.
func (r *run) stackStep(ctx context.Context, cand *Candidate) (todo string, err error) {
	o := &r.o
	su, ok := r.h.(StackUpdater)
	if !ok {
		return "", refused("--aws is not available on this host")
	}
	o.say("Updating the AWS stack to the template of %s (your AWS credentials are used, not the instance role's)", cand.Tag)
	out, err := su.UpdateStack(ctx, cand, StackOptions{Name: o.StackName, Sets: o.StackSets})
	var se *StackError
	switch {
	case errors.As(err, &se):
		switch se.Code {
		case 2:
			return "", &Failure{Code: ExitRefused, Err: fmt.Errorf("the AWS stack update was refused: %w; the node was not changed", se.Err)}
		case -1, 129, 130, 143:
			return "", &Failure{Code: ExitRefused, Err: fmt.Errorf("the AWS stack update was interrupted: %w; the node was not changed. "+
				"The update may still be running in CloudFormation. If the script said that its guard (a temporary stack policy) is still on, "+
				"run the command it printed once the stack has stopped updating, or run `supavise upgrade --aws` again, which takes it off", se.Err)}
		}
		return "", &Failure{Code: ExitRefused, Err: fmt.Errorf("the AWS stack update failed: %w; the node was not changed", se.Err)}
	case err != nil:
		return "", refused("%v", err)
	}
	if out.Command != "" {
		o.say("There are no AWS credentials of yours in the environment (`sudo -E` keeps them), so the stack was not updated. To update it, run:\n\n  %s\n", out.Command)
		if out.Standalone != "" {
			o.say("or, where your credentials are (AWS CloudShell, your laptop), with the supavise-aws-deploy.sh attached to the release %s:\n\n  %s\n", cand.Tag, out.Standalone)
		}
		return out.Command, nil
	}
	if out.GuardLeftOn {
		r.stackWarn = "The AWS stack was updated, but the script could not take its guard (a temporary stack policy) off, and while it is on, " +
			"CloudFormation refuses to replace most resources of the stack. Run the set-stack-policy command the script printed, or run `supavise upgrade --aws` again, which takes it off."
		o.say("%s", r.stackWarn)
		return "", nil
	}
	o.say("The AWS stack is up to date")
	return "", nil
}
