package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/nodeupgrade"
)

// The stack layer of `supavise upgrade`: with --aws the release's signed script and template are
// staged and `supavise-aws-deploy.sh update` runs with the operator's own AWS credentials. The
// node's instance role never changes its stack (internal/nodeupgrade, "The AWS stack").

var (
	stackNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,127}$`)
	setRe       = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*=[^\s]*$`)
)

// checkAWSFlags refuses the stack flags without --aws, and a stack name or a parameter that is not
// one, before the node is read.
func checkAWSFlags(aws bool, stackName string, sets []string) error {
	if !aws {
		if stackName != "" || len(sets) > 0 {
			return errors.New("--stack-name and --set belong to --aws")
		}
		return nil
	}
	if stackName != "" && !stackNameRe.MatchString(stackName) {
		return fmt.Errorf("--stack-name %q is not a CloudFormation stack name", stackName)
	}
	for _, s := range sets {
		if !setRe.MatchString(s) {
			return fmt.Errorf("--set %q: want Parameter=Value, for example Failover=on", s)
		}
	}
	return nil
}

// dmiDir is where the firmware describes the machine.
var dmiDir = "/sys/devices/virtual/dmi/id"

// ec2Likely says whether this machine looks like an EC2 instance, without a network call: its
// firmware names Amazon, or a metadata endpoint is configured (a test). The cloud questions are
// asked only then, because a machine that is not on EC2 waits several seconds for each.
func ec2Likely() bool {
	if os.Getenv(awsapi.EnvEndpointIMDS) != "" {
		return true
	}
	for _, f := range []string{"sys_vendor", "board_vendor", "bios_vendor"} {
		if b, err := os.ReadFile(filepath.Join(dmiDir, f)); err == nil && strings.Contains(strings.ToLower(string(b)), "amazon") {
			return true
		}
	}
	return false
}

// awsCredentialsPresent says whether the environment holds AWS credentials of the operator's: the
// standard variables, a web identity or container credential source, or a shared credentials or
// config file. The instance role does not count; the update script refuses to use it.
func awsCredentialsPresent(getenv func(string) string, home string) bool {
	if getenv("AWS_ACCESS_KEY_ID") != "" && getenv("AWS_SECRET_ACCESS_KEY") != "" {
		return true
	}
	for _, k := range []string{"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI"} {
		if getenv(k) != "" {
			return true
		}
	}
	files := []string{getenv("AWS_SHARED_CREDENTIALS_FILE"), getenv("AWS_CONFIG_FILE")}
	if home != "" {
		files = append(files, filepath.Join(home, ".aws", "credentials"), filepath.Join(home, ".aws", "config"))
	}
	for _, f := range files {
		if f == "" {
			continue
		}
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	return false
}

// awsUpdateArgs are the arguments of `supavise-aws-deploy.sh` for the stack update. The parameters
// of the stack are the ones it has (UsePreviousValue), so a parameter the operator does not name
// keeps its value.
func awsUpdateArgs(stack, template string, sets []string) []string {
	args := []string{"update", "--stack", stack, "--template", template, "--params-from-stack"}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	return args
}

// awsCommand is the command that runs the same update by hand, for a node that found no
// credentials.
func awsCommand(stack string, sets []string) string {
	cmd := "sudo -E supavise upgrade --aws --stack-name " + shellQuote(stack)
	for _, s := range sets {
		cmd += " --set " + shellQuote(s)
	}
	return cmd
}

// shellQuote quotes s for display, only when a shell would need it.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_./:=@%+,-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// stackName finds the CloudFormation stack that made this server: the flag, else [aws] stack_name
// (config.d/20-aws.toml), else the instance's supavise:stack-name tag. A stack from before the tags
// has to be named once, and the update then records it.
func (h *nodeHost) stackName(ctx context.Context, given string) (string, error) {
	name := given
	if name == "" {
		name = h.cfg.AWS.StackName
	}
	if name == "" {
		if c, err := awsapi.New(awsapi.Config{}); err == nil {
			if tags, err := c.IMDS.Tags(ctx); err == nil {
				name = tags["supavise:stack-name"]
			}
		}
	}
	if name == "" {
		return "", errors.New("cannot tell which CloudFormation stack made this server (the instance has no supavise:stack-name tag and [aws] stack_name is not set): name it with --stack-name")
	}
	if !stackNameRe.MatchString(name) {
		return "", fmt.Errorf("%q is not a CloudFormation stack name", name)
	}
	return name, nil
}

// UpdateStack implements nodeupgrade.StackUpdater.
func (h *nodeHost) UpdateStack(ctx context.Context, c *nodeupgrade.Candidate, so nodeupgrade.StackOptions) (nodeupgrade.StackOutcome, error) {
	stack, err := h.stackName(ctx, so.Name)
	if err != nil {
		return nodeupgrade.StackOutcome{}, err
	}
	if err := checkAWSFlags(true, "", so.Sets); err != nil {
		return nodeupgrade.StackOutcome{}, err
	}
	home, _ := os.UserHomeDir()
	if !awsCredentialsPresent(os.Getenv, home) {
		return nodeupgrade.StackOutcome{Command: awsCommand(stack, so.Sets)}, nil
	}
	r, ok := c.Data.(*resolved)
	if !ok {
		return nodeupgrade.StackOutcome{}, errors.New("internal error: the release was not resolved")
	}
	assets, err := r.ver.AWSAssets(ctx, r.opts)
	if err != nil {
		return nodeupgrade.StackOutcome{}, err
	}
	// A private directory of root's: the script is run from here and the template is read from here,
	// and neither may be changed between the check and the run.
	dir, err := os.MkdirTemp("", "supavise-aws-")
	if err != nil {
		return nodeupgrade.StackOutcome{}, err
	}
	defer os.RemoveAll(dir)
	script, template := filepath.Join(dir, "supavise-aws-deploy.sh"), filepath.Join(dir, assets.TemplateName)
	if err := os.WriteFile(script, assets.Script, 0o700); err != nil {
		return nodeupgrade.StackOutcome{}, err
	}
	if err := os.WriteFile(template, assets.Template, 0o600); err != nil {
		return nodeupgrade.StackOutcome{}, err
	}

	// The script asks the operator to type "apply" after it shows the change set, so it keeps the
	// terminal. It is run by bash, not executed, so a noexec /tmp does not matter.
	cmd := exec.CommandContext(ctx, "bash", append([]string{script}, awsUpdateArgs(stack, template, so.Sets)...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, h.out, h.errw
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code := ee.ExitCode()
			return nodeupgrade.StackOutcome{}, &nodeupgrade.StackError{Code: code, Err: fmt.Errorf("supavise-aws-deploy.sh update exited with status %d", code)}
		}
		return nodeupgrade.StackOutcome{}, fmt.Errorf("running supavise-aws-deploy.sh: %w", err)
	}
	// The stack is known now: later runs and `supavise status` need not be told its name.
	if err := writeAWSConfig(filepath.Dir(h.cfgPath), stack); err != nil {
		fmt.Fprintf(h.errw, "warning: could not record the stack name in config.d: %v\n", err)
	}
	return nodeupgrade.StackOutcome{}, nil
}

// writeAWSConfig records the node's stack in config.d/20-aws.toml. The file is read by the
// supavise user, so it is not root's alone; it holds no secret.
func writeAWSConfig(configDir, stack string) error {
	path := filepath.Join(configDir, config.ConfigDName, config.AWSConfigFile)
	body := fmt.Sprintf("# Written by `supavise upgrade --aws`: the CloudFormation stack that made this server.\n[aws]\nstack_name = %q\n", stack)
	if cur, err := os.ReadFile(path); err == nil && string(cur) == body {
		return nil
	}
	uid, gid := supaviseOwner()
	return writeFileAtomic(path, []byte(body), 0o644, uid, gid)
}
